package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Borgels/mantlerd/internal/client"
	"github.com/Borgels/mantlerd/internal/commands"
	"github.com/Borgels/mantlerd/internal/config"
	"github.com/Borgels/mantlerd/internal/runtime"
	agenttools "github.com/Borgels/mantlerd/internal/tools"
	"github.com/Borgels/mantlerd/internal/trainer"
	"github.com/Borgels/mantlerd/internal/types"
)

func TestRunCheckInFollowUpDoesNotResendOutcomeEvents(t *testing.T) {
	oldDesiredPath := desiredConfigCachePath
	desiredConfigCachePath = filepath.Join(t.TempDir(), "desired-config.json")
	defer func() { desiredConfigCachePath = oldDesiredPath }()

	var (
		mu       sync.Mutex
		requests []types.CheckinRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/checkin" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var payload types.CheckinRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, payload)
		mu.Unlock()

		response := map[string]any{
			"data": types.CheckinResponse{
				Ack:        true,
				ServerTime: time.Now().UTC().Format(time.RFC3339),
				DesiredConfig: types.DesiredConfig{
					Harnesses: []types.DesiredHarness{
						{
							ID:     "h1",
							Name:   "Harness 1",
							Type:   "codex_cli",
							Status: "ready",
							Transport: types.HarnessTransportConfig{
								Kind: "cli",
							},
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	cl, err := client.New(server.URL, "test-token", true)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	cfg := config.Config{
		ServerURL: server.URL,
		Token:     "test-token",
		MachineID: "machine-1",
		Interval:  30 * time.Second,
		Insecure:  true,
	}
	runtimeManager := runtime.NewManager()
	trainerManager := trainer.NewManager()
	toolManager := agenttools.NewManager()
	executor := commands.NewExecutor(runtimeManager, trainerManager, toolManager, cfg, nil, nil)
	dispatcher := newCommandDispatcher(context.Background(), executor, cl, defaultLightCommandConcurrency)
	outcomes := &outcomeBuffer{}
	outcomes.Add(types.OutcomeEvent{
		EventType: "task_success",
		TaskID:    "task-1",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})

	runCheckIn(
		context.Background(),
		cfg,
		cl,
		runtimeManager,
		trainerManager,
		toolManager,
		executor,
		outcomes,
		dispatcher,
		time.Now(),
		true,
		nil,
	)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	if !dispatcher.WaitForIdle(waitCtx) {
		t.Fatal("timed out waiting for command completion")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) < 2 {
		t.Fatalf("expected at least 2 checkin requests (primary + follow-up), got %d", len(requests))
	}
	if len(requests[0].OutcomeEvents) != 1 {
		t.Fatalf("expected primary checkin to send 1 outcome event, got %d", len(requests[0].OutcomeEvents))
	}
	if len(requests[1].OutcomeEvents) != 0 {
		t.Fatalf("expected follow-up checkin to send 0 outcome events, got %d", len(requests[1].OutcomeEvents))
	}
}
