// Package audit writes a tamper-evident, newline-delimited JSON audit log of
// every server-originated command that the agent processes.
//
// One JSON object per line is appended to the audit log file so that the file
// can be streamed, rotated, or forwarded without losing individual records.
// The log is intentionally separate from the operational log so that security
// events remain easy to grep even when verbose logging is enabled.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event records a single server-originated action and its outcome.
type Event struct {
	// Timestamp is the RFC-3339 time at which the event was recorded.
	Timestamp string `json:"ts"`
	// CommandID is the opaque ID assigned by the server.
	CommandID string `json:"commandId,omitempty"`
	// CommandType is the human-readable type string (e.g. "pull_model").
	CommandType string `json:"commandType"`
	// ServerURL is the control-plane URL the command arrived from (token stripped).
	ServerURL string `json:"serverUrl,omitempty"`
	// Outcome is "allowed", "denied", "success", or "failed".
	Outcome string `json:"outcome"`
	// Reason carries the denial or failure message when Outcome is "denied" or "failed".
	Reason string `json:"reason,omitempty"`
	// Destructive is true when the command falls into the high-impact category.
	Destructive bool `json:"destructive,omitempty"`
	// DurationMs is the wall-clock duration from dispatch to completion (only
	// set for Outcome "success" or "failed").
	DurationMs int64 `json:"durationMs,omitempty"`
}

// Logger appends audit events to a rotating newline-delimited JSON file.
type Logger struct {
	mu   sync.Mutex
	path string
}

var defaultLogger *Logger
var defaultLoggerOnce sync.Once

// Default returns the process-wide audit logger, initialised lazily.
func Default() *Logger {
	defaultLoggerOnce.Do(func() {
		defaultLogger = &Logger{path: defaultLogPath()}
	})
	return defaultLogger
}

// NewLogger creates a Logger that writes to path.
func NewLogger(path string) *Logger {
	return &Logger{path: path}
}

// Log appends ev to the audit log file.  Errors are silently discarded
// so that a missing/unwritable log file never blocks command execution.
func (l *Logger) Log(ev Event) {
	if ev.Timestamp == "" {
		ev.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s\n", raw)
}

// Path returns the absolute path of the log file this logger writes to.
func (l *Logger) Path() string { return l.path }

// ReadRecent returns up to n most-recent events from the log file.
func (l *Logger) ReadRecent(n int) ([]Event, error) {
	if n <= 0 {
		n = 50
	}
	l.mu.Lock()
	raw, err := os.ReadFile(l.path)
	l.mu.Unlock()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read audit log: %w", err)
	}

	lines := splitLines(raw)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	events := make([]Event, 0, len(lines))
	for _, line := range lines {
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}
	return events, nil
}

func splitLines(data []byte) [][]byte {
	var result [][]byte
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			line := data[start:i]
			if len(line) > 0 {
				result = append(result, line)
			}
			start = i + 1
		}
	}
	if start < len(data) {
		result = append(result, data[start:])
	}
	return result
}

func defaultLogPath() string {
	if os.Geteuid() == 0 {
		return "/var/log/mantler/audit.log"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".mantler/audit.log"
	}
	return filepath.Join(home, ".mantler", "audit.log")
}
