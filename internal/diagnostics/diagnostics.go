package diagnostics

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	defaultMaxLogSize = 2 << 20
	defaultRotations  = 4
	defaultCrashFiles = 10
	maxMessageBytes   = 32 << 10
	maxSnapshotBytes  = 256 << 10
)

var (
	emailPattern    = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
	secretPattern   = regexp.MustCompile(`(?i)(password|passwd|token|authorization|secret|api[_-]?key)(\s*[:=]\s*)([^\s,;]+)`)
	userPathPattern = regexp.MustCompile(`(?i)(?:[A-Z]:\\Users\\[^\\\s]+|/(?:Users|home)/[^/\s]+)`)
	urlPattern      = regexp.MustCompile(`(?i)\b(?:https?|imap|imaps)://[^\s]+`)
	hostPattern     = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:com|net|org|de|io|co|dev|test|local)\b`)
)

type Config struct {
	Directory  string
	Version    string
	Debug      bool
	MaxLogSize int64
	Rotations  int
	CrashFiles int
	Console    io.Writer
}

type Manager struct {
	mu           sync.Mutex
	directory    string
	logPath      string
	markerPath   string
	version      string
	sessionID    string
	debug        bool
	maxLogSize   int64
	rotations    int
	crashFiles   int
	latestCrash  string
	frontendSeen map[[32]byte]struct{}
	sensitive    []string
	closed       bool
	console      io.Writer
}

type entry struct {
	Time        string         `json:"time"`
	Level       string         `json:"level"`
	Event       string         `json:"event"`
	SessionID   string         `json:"sessionId"`
	Version     string         `json:"version,omitempty"`
	MigrationID int64          `json:"migrationId,omitempty"`
	Service     string         `json:"service,omitempty"`
	Phase       string         `json:"phase,omitempty"`
	State       string         `json:"state,omitempty"`
	ErrorCode   string         `json:"errorCode,omitempty"`
	Message     string         `json:"message,omitempty"`
	Values      map[string]any `json:"values,omitempty"`
}

type Fields struct {
	MigrationID int64
	Service     string
	Phase       string
	State       string
	ErrorCode   string
	Message     string
	Values      map[string]any
}

type FrontendError struct {
	Message        string
	Stack          string
	ComponentStack string
	View           string
	Fatal          bool
}

func DefaultDirectory() (string, error) {
	base := ""
	if runtime.GOOS == "windows" {
		base = os.Getenv("LOCALAPPDATA")
	}
	if base == "" {
		var err error
		base, err = os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("locate application data directory: %w", err)
		}
	}
	if runtime.GOOS == "darwin" {
		base = filepath.Join(base, "Tenbyte Mail Migrator")
	} else {
		base = filepath.Join(base, "Tenbyte", "Mail Migrator")
	}
	return filepath.Join(base, "logs"), nil
}

func New(version string) (*Manager, error) {
	directory, err := DefaultDirectory()
	if err != nil {
		return nil, err
	}
	level := strings.ToLower(strings.TrimSpace(os.Getenv("TENBYTE_LOG_LEVEL")))
	debugEnabled := level == "debug" || level == "trace"
	var console io.Writer
	if debugEnabled {
		console = os.Stderr
	}
	return NewWithConfig(Config{Directory: directory, Version: version, Debug: debugEnabled, Console: console})
}

func NewWithConfig(config Config) (*Manager, error) {
	if strings.TrimSpace(config.Directory) == "" {
		return nil, errors.New("diagnostics directory is required")
	}
	if config.MaxLogSize <= 0 {
		config.MaxLogSize = defaultMaxLogSize
	}
	if config.Rotations <= 0 {
		config.Rotations = defaultRotations
	}
	if config.CrashFiles <= 0 {
		config.CrashFiles = defaultCrashFiles
	}
	if err := os.MkdirAll(config.Directory, 0o700); err != nil {
		return nil, fmt.Errorf("create diagnostics directory: %w", err)
	}
	manager := &Manager{
		directory: config.Directory, logPath: filepath.Join(config.Directory, "app.log"), markerPath: filepath.Join(config.Directory, "session.active"),
		version: config.Version, sessionID: newSessionID(), debug: config.Debug, maxLogSize: config.MaxLogSize, rotations: config.Rotations, crashFiles: config.CrashFiles,
		frontendSeen: make(map[[32]byte]struct{}), console: config.Console,
	}
	previous, previousErr := os.ReadFile(manager.markerPath)
	if previousErr == nil && strings.TrimSpace(string(previous)) != "" {
		_ = manager.write("warning", "unclean_previous_session", Fields{Values: map[string]any{"previousSessionId": cleanToken(string(previous))}})
	}
	if err := os.WriteFile(manager.markerPath, []byte(manager.sessionID+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write diagnostics session marker: %w", err)
	}
	_ = os.Chmod(manager.markerPath, 0o600)
	if err := manager.write("info", "session_started", Fields{Values: map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH, "debug": manager.debug}}); err != nil {
		return nil, err
	}
	return manager, nil
}

func newSessionID() string {
	value := make([]byte, 8)
	if _, err := rand.Read(value); err == nil {
		return hex.EncodeToString(value)
	}
	return fmt.Sprintf("%x", time.Now().UTC().UnixNano())
}

func cleanToken(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 64 {
		value = value[:64]
	}
	return regexp.MustCompile(`[^a-zA-Z0-9._-]`).ReplaceAllString(value, "")
}

func (m *Manager) Directory() string { return m.directory }
func (m *Manager) LogPath() string   { return m.logPath }
func (m *Manager) SessionID() string { return m.sessionID }

// Protect registers runtime-only values that must be removed if a dependency
// includes them in an otherwise useful error message. Values are never written
// to disk and are cleared by a factory reset.
func (m *Manager) Protect(values ...string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, value := range values {
		value = strings.TrimSpace(value)
		if len(value) < 3 {
			continue
		}
		seen := false
		for _, existing := range m.sensitive {
			if existing == value {
				seen = true
				break
			}
		}
		if !seen {
			m.sensitive = append(m.sensitive, value)
		}
	}
}

func (m *Manager) DebugEvent(event string, fields Fields) {
	if m != nil && m.debug {
		_ = m.write("debug", event, fields)
	}
}

func (m *Manager) InfoEvent(event string, fields Fields) {
	if m != nil {
		_ = m.write("info", event, fields)
	}
}

func (m *Manager) WarningEvent(event string, fields Fields) {
	if m != nil {
		_ = m.write("warning", event, fields)
	}
}

func (m *Manager) ErrorEvent(event string, fields Fields) {
	if m != nil {
		_ = m.write("error", event, fields)
	}
}

func (m *Manager) write(level, eventName string, fields Fields) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("diagnostics manager is closed")
	}
	return m.writeLocked(level, eventName, fields)
}

func (m *Manager) writeLocked(level, eventName string, fields Fields) error {
	item := entry{
		Time: time.Now().UTC().Format(time.RFC3339Nano), Level: level, Event: cleanEvent(eventName), SessionID: m.sessionID, Version: m.version,
		MigrationID: fields.MigrationID, Service: cleanEvent(fields.Service), Phase: cleanEvent(fields.Phase), State: cleanEvent(fields.State), ErrorCode: cleanEvent(fields.ErrorCode),
		Message: m.scrubLocked(fields.Message), Values: m.scrubValuesLocked(fields.Values),
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("encode diagnostic entry: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := m.rotateLocked(int64(len(encoded))); err != nil {
		return err
	}
	file, err := os.OpenFile(m.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open diagnostics log: %w", err)
	}
	_ = os.Chmod(m.logPath, 0o600)
	_, writeErr := file.Write(encoded)
	if level == "error" || level == "fatal" {
		_ = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write diagnostics log: %w", writeErr)
	}
	if m.console != nil {
		_, _ = m.console.Write(encoded)
	}
	return closeErr
}

func cleanEvent(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 {
		value = value[:128]
	}
	return regexp.MustCompile(`[^a-zA-Z0-9._:-]`).ReplaceAllString(value, "_")
}

func Scrub(value string) string {
	var sanitized strings.Builder
	for _, character := range value {
		if unicode.IsControl(character) && character != '\n' {
			sanitized.WriteByte(' ')
			continue
		}
		sanitized.WriteRune(character)
	}
	value = sanitized.String()
	value = secretPattern.ReplaceAllString(value, "$1$2[REDACTED]")
	value = emailPattern.ReplaceAllString(value, "[EMAIL]")
	value = userPathPattern.ReplaceAllString(value, "[USER_HOME]")
	value = urlPattern.ReplaceAllString(value, "[URL]")
	value = hostPattern.ReplaceAllString(value, "[HOST]")
	if len(value) > maxMessageBytes {
		value = value[:maxMessageBytes] + "…[TRUNCATED]"
	}
	return strings.TrimSpace(value)
}

func (m *Manager) scrubLocked(value string) string {
	value = Scrub(value)
	for _, sensitive := range m.sensitive {
		value = strings.ReplaceAll(value, sensitive, "[REDACTED]")
	}
	return value
}

func (m *Manager) scrubValuesLocked(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]any, len(values))
	for key, value := range values {
		cleanKey := cleanEvent(key)
		switch typed := value.(type) {
		case string:
			result[cleanKey] = m.scrubLocked(typed)
		case bool, int, int32, int64, uint, uint32, uint64, float32, float64:
			result[cleanKey] = typed
		default:
			result[cleanKey] = "[OMITTED]"
		}
	}
	return result
}

func (m *Manager) rotateLocked(incoming int64) error {
	info, err := os.Stat(m.logPath)
	if errors.Is(err, os.ErrNotExist) || err == nil && info.Size()+incoming <= m.maxLogSize {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect diagnostics log: %w", err)
	}
	oldest := fmt.Sprintf("%s.%d", m.logPath, m.rotations)
	_ = os.Remove(oldest)
	for index := m.rotations - 1; index >= 1; index-- {
		from := fmt.Sprintf("%s.%d", m.logPath, index)
		to := fmt.Sprintf("%s.%d", m.logPath, index+1)
		if renameErr := os.Rename(from, to); renameErr != nil && !errors.Is(renameErr, os.ErrNotExist) {
			return fmt.Errorf("rotate diagnostics log: %w", renameErr)
		}
	}
	if err := os.Rename(m.logPath, m.logPath+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rotate diagnostics log: %w", err)
	}
	return nil
}

func (m *Manager) RecordFrontendError(report FrontendError) (string, error) {
	if strings.TrimSpace(report.Message) == "" {
		return "", errors.New("frontend error message is required")
	}
	fingerprint := sha256.Sum256([]byte(fmt.Sprintf("%t\x00%s\x00%s\x00%s\x00%s", report.Fatal, report.View, report.Message, report.Stack, report.ComponentStack)))
	m.mu.Lock()
	if _, exists := m.frontendSeen[fingerprint]; exists {
		path := m.latestCrash
		m.mu.Unlock()
		m.DebugEvent("frontend_error_duplicate", Fields{Values: map[string]any{"fatal": report.Fatal}})
		return path, nil
	}
	m.frontendSeen[fingerprint] = struct{}{}
	m.mu.Unlock()
	level := "error"
	if report.Fatal {
		level = "fatal"
	}
	m.ErrorEvent("frontend_error", Fields{State: level, Message: report.Message, Values: map[string]any{"view": report.View, "fatal": report.Fatal}})
	return m.writeCrash("frontend", report.Message, report.Stack, report.ComponentStack)
}

func (m *Manager) RecordPanic(component string, migrationID int64, service string, recovered any, stack []byte) string {
	message := fmt.Sprintf("panic in %s: %v", cleanEvent(component), recovered)
	m.ErrorEvent("panic", Fields{MigrationID: migrationID, Service: service, State: "fatal", Message: message})
	path, _ := m.writeCrash("panic", message, string(stack), "")
	return path
}

func (m *Manager) writeCrash(kind, message, stack, componentStack string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", errors.New("diagnostics manager is closed")
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	path := filepath.Join(m.directory, fmt.Sprintf("crash-%s-%s.log", stamp, m.sessionID))
	body := strings.Join([]string{
		"Tenbyte Mail Migrator crash report", "time: " + time.Now().UTC().Format(time.RFC3339Nano), "version: " + Scrub(m.version),
		"os: " + runtime.GOOS, "arch: " + runtime.GOARCH, "session: " + m.sessionID, "kind: " + cleanEvent(kind),
		"", "message:", m.scrubLocked(message), "", "stack:", m.scrubLocked(stack), "", "react component stack:", m.scrubLocked(componentStack), "",
	}, "\n")
	if len(body) > maxSnapshotBytes {
		body = body[:maxSnapshotBytes] + "\n…[TRUNCATED]\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", fmt.Errorf("write crash report: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	m.latestCrash = path
	if err := m.pruneCrashFilesLocked(); err != nil {
		return path, err
	}
	return path, nil
}

func (m *Manager) pruneCrashFilesLocked() error {
	files, err := filepath.Glob(filepath.Join(m.directory, "crash-*.log"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for len(files) > m.crashFiles {
		if err := os.Remove(files[0]); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		files = files[1:]
	}
	return nil
}

func (m *Manager) Snapshot(maxLines int) (string, error) {
	if maxLines <= 0 || maxLines > 200 {
		maxLines = 200
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	crashPath := m.latestCrash
	if crashPath == "" {
		files, _ := filepath.Glob(filepath.Join(m.directory, "crash-*.log"))
		sort.Strings(files)
		if len(files) > 0 {
			crashPath = files[len(files)-1]
		}
	}
	crash := "No crash report recorded."
	if crashPath != "" {
		if data, err := os.ReadFile(crashPath); err == nil {
			crash = string(data)
		}
	}
	lines, err := m.tailLocked(maxLines)
	if err != nil {
		return "", err
	}
	prefix := fmt.Sprintf("Tenbyte Mail Migrator diagnostics\nversion: %s\nos: %s\narch: %s\nsession: %s\n\nLATEST CRASH\n%s\nRECENT LOGS (%d lines maximum)\n", Scrub(m.version), runtime.GOOS, runtime.GOARCH, m.sessionID, crash, maxLines)
	remaining := maxSnapshotBytes - len(prefix)
	if remaining < 0 {
		prefix = prefix[:maxSnapshotBytes]
		remaining = 0
	}
	start := len(lines)
	used := 0
	for start > 0 {
		lineSize := len(lines[start-1]) + 1
		if used+lineSize > remaining {
			break
		}
		used += lineSize
		start--
	}
	if start > 0 && remaining > len("…[OLDER LOGS OMITTED]\n") {
		prefix += "…[OLDER LOGS OMITTED]\n"
	}
	return prefix + strings.Join(lines[start:], "\n"), nil
}

func (m *Manager) tailLocked(maxLines int) ([]string, error) {
	paths := []string{m.logPath + ".1", m.logPath}
	var lines []string
	for _, path := range paths {
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64<<10), maxMessageBytes*2)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
			if len(lines) > maxLines {
				lines = lines[len(lines)-maxLines:]
			}
		}
		scanErr := scanner.Err()
		_ = file.Close()
		if scanErr != nil {
			return nil, scanErr
		}
	}
	return lines, nil
}

func (m *Manager) Clear() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	patterns := []string{m.logPath, m.logPath + ".*", filepath.Join(m.directory, "crash-*.log")}
	for _, pattern := range patterns {
		paths := []string{pattern}
		if strings.Contains(pattern, "*") {
			paths, _ = filepath.Glob(pattern)
		}
		for _, path := range paths {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	m.latestCrash = ""
	m.sensitive = nil
	clear(m.frontendSeen)
	return m.writeLocked("info", "diagnostics_reset", Fields{})
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	_ = m.writeLocked("info", "session_finished", Fields{})
	marker, _ := os.ReadFile(m.markerPath)
	if strings.TrimSpace(string(marker)) == m.sessionID {
		_ = os.Remove(m.markerPath)
	}
	m.closed = true
	return nil
}

// The methods below implement Wails' logger.Logger interface while applying
// the same privacy filter and file retention as application events.
func (m *Manager) Print(message string)   { m.InfoEvent("wails.print", Fields{Message: message}) }
func (m *Manager) Trace(message string)   { m.DebugEvent("wails.trace", Fields{Message: message}) }
func (m *Manager) Debug(message string)   { m.DebugEvent("wails.debug", Fields{Message: message}) }
func (m *Manager) Info(message string)    { m.InfoEvent("wails.info", Fields{Message: message}) }
func (m *Manager) Warning(message string) { m.WarningEvent("wails.warning", Fields{Message: message}) }
func (m *Manager) Error(message string)   { m.ErrorEvent("wails.error", Fields{Message: message}) }
func (m *Manager) Fatal(message string) {
	m.ErrorEvent("wails.fatal", Fields{State: "fatal", Message: message})
	_, _ = m.writeCrash("wails", message, "", "")
	os.Exit(1)
}
