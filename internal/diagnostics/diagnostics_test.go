package diagnostics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func newTestManager(t *testing.T, mutate func(*Config)) *Manager {
	t.Helper()
	config := Config{Directory: t.TempDir(), Version: "test", Debug: true, MaxLogSize: 512, Rotations: 4, CrashFiles: 10}
	if mutate != nil {
		mutate(&config)
	}
	manager, err := NewWithConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func TestScrubRemovesSensitiveIdentifiers(t *testing.T) {
	value := Scrub("password=hunter2 user@example.com https://mail.example.com/a /Users/alice/project C:\\Users\\alice\\project imap.example.com")
	for _, secret := range []string{"hunter2", "user@example.com", "mail.example.com", "/Users/alice", `C:\Users\alice`, "imap.example.com"} {
		if strings.Contains(value, secret) {
			t.Fatalf("sensitive value %q remained in %q", secret, value)
		}
	}
	if !strings.Contains(value, "[REDACTED]") || !strings.Contains(value, "[EMAIL]") || !strings.Contains(value, "[USER_HOME]") {
		t.Fatalf("expected redaction markers in %q", value)
	}
}

func TestProtectedRuntimeValuesNeverReachDisk(t *testing.T) {
	manager := newTestManager(t, nil)
	manager.Protect("custom-user", "private-folder", "unusual.example.cloud", "one-time-password")
	manager.ErrorEvent("dependency_failed", Fields{Message: "custom-user private-folder unusual.example.cloud one-time-password"})
	data, err := os.ReadFile(manager.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"custom-user", "private-folder", "unusual.example.cloud", "one-time-password"} {
		if strings.Contains(string(data), value) {
			t.Fatalf("protected value %q reached the log: %s", value, data)
		}
	}
}

func TestConcurrentStructuredWritesAndRotation(t *testing.T) {
	manager := newTestManager(t, nil)
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for index := 0; index < 20; index++ {
				manager.InfoEvent("test_event", Fields{MigrationID: int64(worker + 1), Values: map[string]any{"index": index}})
			}
		}(worker)
	}
	wait.Wait()
	files, err := filepath.Glob(manager.LogPath() + "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 2 || len(files) > 5 {
		t.Fatalf("unexpected rotated files: %v", files)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(line), &decoded); err != nil {
				t.Fatalf("invalid JSON line in %s: %v", path, err)
			}
		}
	}
}

func TestCrashRetentionAndSnapshotLimit(t *testing.T) {
	manager := newTestManager(t, func(config *Config) { config.CrashFiles = 3; config.MaxLogSize = 1 << 20 })
	for index := 0; index < 5; index++ {
		if _, err := manager.RecordFrontendError(FrontendError{Message: fmt.Sprintf("Cannot read properties of null %d", index), Stack: "stack", Fatal: true}); err != nil {
			t.Fatal(err)
		}
	}
	crashes, err := filepath.Glob(filepath.Join(manager.Directory(), "crash-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(crashes) != 3 {
		t.Fatalf("crash retention kept %d files: %v", len(crashes), crashes)
	}
	for index := 0; index < 250; index++ {
		manager.InfoEvent("tail_test", Fields{Values: map[string]any{"index": index}})
	}
	snapshot, err := manager.Snapshot(200)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot, "LATEST CRASH") || !strings.Contains(snapshot, "RECENT LOGS") {
		t.Fatalf("snapshot missing sections: %s", snapshot)
	}
	if strings.Count(snapshot, `"event":"tail_test"`) > 200 {
		t.Fatal("snapshot exceeded the requested log line count")
	}
}

func TestSessionMarkerReportsUncleanPreviousRun(t *testing.T) {
	directory := t.TempDir()
	first, err := NewWithConfig(Config{Directory: directory, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewWithConfig(Config{Directory: directory, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close(); _ = second.Close() })
	data, err := os.ReadFile(second.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"event":"unclean_previous_session"`) {
		t.Fatalf("unclean session was not recorded: %s", data)
	}
}

func TestClearRemovesCrashAndRotatedLogs(t *testing.T) {
	manager := newTestManager(t, nil)
	_, _ = manager.RecordFrontendError(FrontendError{Message: "boom", Fatal: true})
	for index := 0; index < 20; index++ {
		manager.InfoEvent("rotate", Fields{Message: strings.Repeat("x", 100)})
	}
	if err := manager.Clear(); err != nil {
		t.Fatal(err)
	}
	crashes, _ := filepath.Glob(filepath.Join(manager.Directory(), "crash-*.log"))
	rotated, _ := filepath.Glob(manager.LogPath() + ".*")
	if len(crashes) != 0 || len(rotated) != 0 {
		t.Fatalf("diagnostics clear left files: crashes=%v rotated=%v", crashes, rotated)
	}
	data, err := os.ReadFile(manager.LogPath())
	if err != nil || !strings.Contains(string(data), `"event":"diagnostics_reset"`) {
		t.Fatalf("fresh diagnostics log missing: %s, %v", data, err)
	}
}

func TestRecordPanicWritesStackWithOwnerOnlyPermissions(t *testing.T) {
	manager := newTestManager(t, nil)
	path := manager.RecordPanic("mail_worker", 42, "mail", "boom", []byte("goroutine stack"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "panic in mail_worker: boom") || !strings.Contains(string(data), "goroutine stack") {
		t.Fatalf("panic crash report is incomplete: %s", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if permissions := info.Mode().Perm(); permissions != 0o600 {
			t.Fatalf("crash report permissions = %o, want 600", permissions)
		}
	}
}
