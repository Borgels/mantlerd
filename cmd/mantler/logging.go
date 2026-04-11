package main

import (
	"encoding/json"
	"io"
	"log"
	"strings"
	"time"
)

func configureStructuredLogging(logLevel string) {
	level := strings.ToLower(strings.TrimSpace(logLevel))
	if level == "" {
		level = "info"
	}
	base := log.Writer()
	log.SetFlags(0)
	log.SetOutput(&passthroughJSONWriter{level: level, base: base})
}

type passthroughJSONWriter struct {
	level string
	base  io.Writer
}

func (w *passthroughJSONWriter) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	if line == "" {
		return len(p), nil
	}
	payload := map[string]any{
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"level":     w.level,
		"message":   line,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return w.base.Write(p)
	}
	raw = append(raw, '\n')
	_, writeErr := w.base.Write(raw)
	return len(p), writeErr
}

