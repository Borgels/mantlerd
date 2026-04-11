package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Borgels/mantlerd/internal/client"
	"github.com/Borgels/mantlerd/internal/types"
)

func TestAckCommandWithRetryRetries429ThenSuccess(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/ack" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		count := calls.Add(1)
		if count == 1 {
			w.Header().Set("Retry-After", time.Now().Add(100*time.Millisecond).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cl, err := client.New(server.URL, "token", true)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = ackCommandWithRetry(ctx, cl, types.AckRequest{CommandID: "cmd-1", Status: "success"})
	if err != nil {
		t.Fatalf("expected ack retry to succeed, got %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 ack attempts, got %d", got)
	}
}

func TestAckCommandWithRetryStopsOn4xx(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/ack" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	}))
	defer server.Close()

	cl, err := client.New(server.URL, "token", true)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	err = ackCommandWithRetry(context.Background(), cl, types.AckRequest{CommandID: "cmd-1", Status: "failed"})
	if err == nil {
		t.Fatal("expected ack to fail for 400")
	}
	var httpErr *client.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 error, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected single ack attempt for 4xx, got %d", got)
	}
}

func TestAckCommandWithRetryHonorsContextCancellation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/ack" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls.Add(1)
		w.Header().Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer server.Close()

	cl, err := client.New(server.URL, "token", true)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err = ackCommandWithRetry(ctx, cl, types.AckRequest{CommandID: "cmd-1", Status: "failed"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected one ack attempt before cancellation, got %d", got)
	}
}

func TestFlushFailedAcksStopsOnRateLimitAndPreservesRemaining(t *testing.T) {
	tempDir := t.TempDir()
	oldQueuePath := failedAckQueuePath
	failedAckQueuePath = filepath.Join(tempDir, "failed-acks.json")
	defer func() { failedAckQueuePath = oldQueuePath }()

	initial := []types.AckRequest{
		{CommandID: "cmd-1", Status: "success"},
		{CommandID: "cmd-2", Status: "failed"},
	}
	raw, err := json.Marshal(initial)
	if err != nil {
		t.Fatalf("marshal initial queue: %v", err)
	}
	if err := os.WriteFile(failedAckQueuePath, raw, 0o600); err != nil {
		t.Fatalf("seed queue file: %v", err)
	}

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/ack" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer server.Close()

	cl, err := client.New(server.URL, "token", true)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	flushFailedAcks(cl)
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected flush to stop after first rate-limited ack, got %d calls", got)
	}

	queued, err := loadFailedAcks()
	if err != nil {
		t.Fatalf("loadFailedAcks: %v", err)
	}
	if len(queued) != len(initial) {
		t.Fatalf("expected all queued acks to remain, got %d want %d", len(queued), len(initial))
	}
}
