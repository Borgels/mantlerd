package commands

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseOrchestratorExecParamsRequiresType(t *testing.T) {
	_, err := parseOrchestratorExecParams(map[string]interface{}{})
	if err == nil {
		t.Fatalf("expected missing orchestratorType to fail")
	}
}

func TestWriteOrchestratorPayloadFileWritesJSON(t *testing.T) {
	path, err := writeOrchestratorPayloadFile("task", map[string]any{"title": "hello"})
	if err != nil {
		t.Fatalf("writeOrchestratorPayloadFile returned error: %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read payload file: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("expected payload file to contain json")
	}
	if filepath.Ext(path) != ".json" {
		t.Fatalf("expected json file extension, got %s", path)
	}
}

func TestValidateOrchestratorArgs(t *testing.T) {
	if err := validateOrchestratorArgs("crewai", []string{"--help", "--port=9000"}); err != nil {
		t.Fatalf("expected allowed flags to pass: %v", err)
	}
	if err := validateOrchestratorArgs("crewai", []string{"--dangerous"}); err == nil {
		t.Fatalf("expected disallowed flags to fail")
	}
}

func TestSanitizeOrchestratorWorkingDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home: %v", err)
	}
	allowed := filepath.Join(home, "project")
	if _, err := sanitizeOrchestratorWorkingDir(allowed); err != nil {
		t.Fatalf("expected home subpath to be allowed: %v", err)
	}
	if _, err := sanitizeOrchestratorWorkingDir("relative/path"); err == nil {
		t.Fatalf("expected relative path to fail")
	}
	if _, err := sanitizeOrchestratorWorkingDir("/tmp/not-allowed"); err == nil {
		t.Fatalf("expected unapproved root to fail")
	}
}
