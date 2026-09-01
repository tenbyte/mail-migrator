package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

type memoryCredentialStore struct {
	values         map[string]string
	deleteAllErr   error
	deleteAllCalls int
}

type countingUpdateChecker struct {
	calls int
	info  domain.UpdateInfo
}

func (c *countingUpdateChecker) Check(context.Context, string) (domain.UpdateInfo, error) {
	c.calls++
	return c.info, nil
}

func (s *memoryCredentialStore) Set(id, password string) error {
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[id] = password
	return nil
}

func (s *memoryCredentialStore) Get(id string) (string, error) {
	value, ok := s.values[id]
	if !ok {
		return "", errors.New("not found")
	}
	return value, nil
}

func (s *memoryCredentialStore) DeleteAll() error {
	s.deleteAllCalls++
	if s.deleteAllErr != nil {
		return s.deleteAllErr
	}
	clear(s.values)
	return nil
}

func newResetTestApp(t *testing.T, path string, store *memoryCredentialStore) *App {
	t.Helper()
	app := NewApp()
	app.ctx = context.Background()
	app.databasePath = path
	app.credentials = store
	app.reloadApplication = nil
	if err := app.openDatabase(app.ctx, path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if app.db != nil {
			_ = app.db.Close()
		}
	})
	return app
}

func TestResolveResumeAccountKeepsStoredCredential(t *testing.T) {
	store := &memoryCredentialStore{values: map[string]string{"stored-id": "secret"}}
	app := &App{credentials: store}
	persisted := domain.AccountConfig{Host: "imap.example", Username: "user", CredentialID: "stored-id"}
	if err := app.resolveResumeAccount("mail-source", &persisted, domain.AccountConfig{}); err != nil {
		t.Fatal(err)
	}
	if persisted.Password != "secret" || !persisted.RememberCredential || persisted.CredentialID != "stored-id" {
		t.Fatalf("stored credential was not preserved: %#v", persisted)
	}
}

func TestResolveResumeAccountAcceptsOneTimeMissingCredential(t *testing.T) {
	app := &App{credentials: &memoryCredentialStore{}}
	persisted := domain.AccountConfig{Host: "imap.example", Username: "user", CredentialID: "missing-id", RememberCredential: true}
	visible := domain.AccountConfig{Password: "one-time", RememberCredential: false}
	if err := app.resolveResumeAccount("mail-destination", &persisted, visible); err != nil {
		t.Fatal(err)
	}
	if persisted.Password != "one-time" || persisted.RememberCredential {
		t.Fatalf("one-time credential was not applied correctly: %#v", persisted)
	}
}

func TestCheckForUpdateCachesResult(t *testing.T) {
	checker := &countingUpdateChecker{info: domain.UpdateInfo{CurrentVersion: "0.3.0", LatestVersion: "0.3.1", UpdateAvailable: true}}
	app := &App{updateChecker: checker}
	first := app.CheckForUpdate()
	second := app.CheckForUpdate()
	if checker.calls != 1 || first != second || !first.UpdateAvailable {
		t.Fatalf("update result was not cached: calls=%d first=%#v second=%#v", checker.calls, first, second)
	}
}

func TestResetMigrationDataClearsHistoryAndKeepsCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrations.db")
	store := &memoryCredentialStore{values: map[string]string{"saved": "secret"}}
	app := newResetTestApp(t, path, store)
	if _, err := app.db.CreateMigration(context.Background(), domain.StartRequest{
		Source:      domain.AccountConfig{Host: "source.example", Port: 993, Encryption: domain.EncryptionTLS},
		Destination: domain.AccountConfig{Host: "destination.example", Port: 993, Encryption: domain.EncryptionTLS},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".v3.bak", []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.ResetMigrationData(); err != nil {
		t.Fatal(err)
	}
	history, err := app.db.Recent(context.Background(), 20)
	if err != nil || len(history) != 0 {
		t.Fatalf("history was not reset: history=%v err=%v", history, err)
	}
	if store.values["saved"] != "secret" || store.deleteAllCalls != 0 {
		t.Fatalf("credentials changed during migration reset: %#v", store)
	}
	if _, err := os.Stat(path + ".v3.bak"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("schema backup still exists: %v", err)
	}
}

func TestFactoryResetClearsCredentialsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrations.db")
	store := &memoryCredentialStore{values: map[string]string{"saved": "secret"}}
	app := newResetTestApp(t, path, store)
	reloaded := false
	app.reloadApplication = func(context.Context) { reloaded = true }
	if err := app.FactoryReset(); err != nil {
		t.Fatal(err)
	}
	if len(store.values) != 0 || store.deleteAllCalls != 1 {
		t.Fatalf("credentials were not cleared: %#v", store)
	}
	if !reloaded {
		t.Fatal("application reload was not requested")
	}
}

func TestFactoryResetReportsCredentialFailureAfterDataReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrations.db")
	store := &memoryCredentialStore{deleteAllErr: errors.New("keyring locked")}
	app := newResetTestApp(t, path, store)
	reloaded := false
	app.reloadApplication = func(context.Context) { reloaded = true }
	err := app.FactoryReset()
	if err == nil || !strings.Contains(err.Error(), "migration data was reset") {
		t.Fatalf("expected partial reset error, got %v", err)
	}
	if reloaded {
		t.Fatal("application reloaded after a partial reset")
	}
}

func TestResetMigrationDataRecoversFromCorruptDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrations.db")
	if err := os.WriteFile(path, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := NewApp()
	app.ctx = context.Background()
	app.databasePath = path
	app.startupErr = errors.New("database corrupt")
	app.credentials = &memoryCredentialStore{}
	if err := app.ResetMigrationData(); err != nil {
		t.Fatal(err)
	}
	defer app.db.Close()
	if err := app.ensureReady(); err != nil {
		t.Fatalf("application did not recover: %v", err)
	}
}

func TestResetMigrationDataCreatesMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrations.db")
	app := NewApp()
	app.ctx = context.Background()
	app.databasePath = path
	app.startupErr = errors.New("database missing")
	app.credentials = &memoryCredentialStore{}
	if err := app.ResetMigrationData(); err != nil {
		t.Fatal(err)
	}
	defer app.db.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fresh database was not created: %v", err)
	}
}

func TestResetMigrationDataReopensDatabaseAfterRemovalFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrations.db")
	app := newResetTestApp(t, path, &memoryCredentialStore{})
	if _, err := app.db.CreateMigration(context.Background(), domain.StartRequest{
		Source:      domain.AccountConfig{Host: "source.example", Port: 993, Encryption: domain.EncryptionTLS},
		Destination: domain.AccountConfig{Host: "destination.example", Port: 993, Encryption: domain.EncryptionTLS},
	}); err != nil {
		t.Fatal(err)
	}
	app.removeStateFiles = func(string) error { return errors.New("permission denied") }
	err := app.ResetMigrationData()
	if err == nil || !strings.Contains(err.Error(), "database was reopened") {
		t.Fatalf("expected recoverable removal error, got %v", err)
	}
	history, historyErr := app.db.Recent(context.Background(), 20)
	if historyErr != nil || len(history) != 1 {
		t.Fatalf("existing database was not reopened: history=%v err=%v", history, historyErr)
	}
}

func TestResetMigrationDataRejectsActiveMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrations.db")
	app := newResetTestApp(t, path, &memoryCredentialStore{})
	app.resetBlocked = func() bool { return true }
	if err := app.ResetMigrationData(); err == nil || !strings.Contains(err.Error(), "currently active") {
		t.Fatalf("expected active migration error, got %v", err)
	}
}

func TestApplicationVersionsAreConsistent(t *testing.T) {
	readJSON := func(path string) map[string]any {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	wails := readJSON("wails.json")
	frontend := readJSON("frontend/package.json")
	lockfile := readJSON("frontend/package-lock.json")
	info, ok := wails["info"].(map[string]any)
	if !ok {
		t.Fatal("wails.json has no info object")
	}
	packages, ok := lockfile["packages"].(map[string]any)
	if !ok {
		t.Fatal("frontend/package-lock.json has no packages object")
	}
	lockRoot, ok := packages[""].(map[string]any)
	if !ok {
		t.Fatal("frontend/package-lock.json has no root package")
	}
	for path, version := range map[string]any{
		"appVersion":                      appVersion,
		"wails.json":                      info["productVersion"],
		"frontend/package.json":           frontend["version"],
		"frontend/package-lock.json":      lockfile["version"],
		"frontend/package-lock.json root": lockRoot["version"],
	} {
		if version != "0.3.0" {
			t.Errorf("%s has version %v, want 0.3.0", path, version)
		}
	}
}
