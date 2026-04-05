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
