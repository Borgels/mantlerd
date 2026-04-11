package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

func TestOutcomeBufferPersistsAndLoads(t *testing.T) {
	tempPath := filepath.Join(t.TempDir(), "outcome-buffer.json")
	oldPath := outcomeBufferPath
	outcomeBufferPath = tempPath
	defer func() { outcomeBufferPath = oldPath }()

	buffer := &outcomeBuffer{}
	buffer.Add(types.OutcomeEvent{
		EventType:    "harness_eval",
		Detail:       "ok",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		EvalPromptID: "prompt-1",
	})

	loaded := newOutcomeBuffer()
	snapshot := loaded.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("expected one persisted outcome event, got %d", len(snapshot))
	}
	if snapshot[0].EvalPromptID != "prompt-1" {
		t.Fatalf("unexpected event after reload: %#v", snapshot[0])
	}
}
