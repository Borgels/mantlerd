package main

import (
	"context"
	"testing"

	"github.com/Borgels/mantlerd/internal/client"
)

func TestSanitizeRuntimePlanPath(t *testing.T) {
	valid, err := sanitizeRuntimePlanPath("/etc/mantler/runtime.conf")
	if err != nil {
		t.Fatalf("expected /etc/mantler path to be valid: %v", err)
	}
	if valid != "/etc/mantler/runtime.conf" {
		t.Fatalf("unexpected sanitized path: %s", valid)
	}

	if _, err := sanitizeRuntimePlanPath("relative/path"); err == nil {
		t.Fatalf("expected relative path to fail")
	}
	if _, err := sanitizeRuntimePlanPath("/tmp/outside"); err == nil {
		t.Fatalf("expected path outside allowed roots to fail")
	}
}

func TestShouldRetryExploreRequest(t *testing.T) {
	if !shouldRetryExploreRequest(context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded to be retryable")
	}
	if !shouldRetryExploreRequest(&client.HTTPError{StatusCode: 504}) {
		t.Fatalf("expected HTTP 504 to be retryable")
	}
	if shouldRetryExploreRequest(&client.HTTPError{StatusCode: 400}) {
		t.Fatalf("did not expect HTTP 400 to be retryable")
	}
}

func TestShouldForceOllamaFromExploreErrorTimeouts(t *testing.T) {
	if !shouldForceOllamaFromExploreError(&client.HTTPError{StatusCode: 504}) {
		t.Fatalf("expected HTTP 504 to trigger ollama fallback")
	}
	if !shouldForceOllamaFromExploreError(context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded to trigger ollama fallback")
	}
	if shouldForceOllamaFromExploreError(&client.HTTPError{StatusCode: 400}) {
		t.Fatalf("did not expect HTTP 400 to trigger ollama fallback")
	}
}
