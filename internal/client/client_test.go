package client

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	t.Run("parses integer seconds", func(t *testing.T) {
		got := parseRetryAfter("120")
		if got != 120*time.Second {
			t.Fatalf("expected 120s, got %v", got)
		}
	})

	t.Run("rejects non-spec duration suffixes", func(t *testing.T) {
		if got := parseRetryAfter("10m"); got != 0 {
			t.Fatalf("expected 0 for invalid Retry-After seconds, got %v", got)
		}
	})

	t.Run("parses http-date and clamps", func(t *testing.T) {
		future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
		got := parseRetryAfter(future)
		if got <= 0 || got > maxRetryAfter {
			t.Fatalf("expected positive delay <= %v, got %v", maxRetryAfter, got)
		}

		farFuture := time.Now().Add(30 * time.Minute).UTC().Format(http.TimeFormat)
		clamped := parseRetryAfter(farFuture)
		if clamped != maxRetryAfter {
			t.Fatalf("expected clamp to %v, got %v", maxRetryAfter, clamped)
		}
	})
}

func TestRetryDecision(t *testing.T) {
	wait, retryable := retryDecision(errors.New("boom"))
	if !retryable || wait != 0 {
		t.Fatalf("expected non-http error to be retryable with zero wait, got retryable=%v wait=%v", retryable, wait)
	}

	wait, retryable = retryDecision(&HTTPError{StatusCode: http.StatusTooManyRequests, RetryAfter: 45 * time.Second})
	if retryable || wait != 0 {
		t.Fatalf("expected 429 to be non-retryable, got retryable=%v wait=%v", retryable, wait)
	}

	wait, retryable = retryDecision(&HTTPError{StatusCode: http.StatusBadRequest})
	if retryable || wait != 0 {
		t.Fatalf("expected 400 to be non-retryable, got retryable=%v wait=%v", retryable, wait)
	}

	wait, retryable = retryDecision(&HTTPError{StatusCode: http.StatusInternalServerError})
	if !retryable || wait != 0 {
		t.Fatalf("expected 500 to be retryable, got retryable=%v wait=%v", retryable, wait)
	}

	wait, retryable = retryDecision(&HTTPError{StatusCode: http.StatusInternalServerError, RetryAfter: 7 * time.Second})
	if !retryable || wait != 7*time.Second {
		t.Fatalf("expected 500 to honor retry-after, got retryable=%v wait=%v", retryable, wait)
	}
}

func TestRetryStopsOn429Immediately(t *testing.T) {
	attempts := 0
	start := time.Now()
	_, err := Retry(context.Background(), 3, func() (int, error) {
		attempts++
		return 0, &HTTPError{StatusCode: http.StatusTooManyRequests, RetryAfter: 30 * time.Second}
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Fatalf("expected single attempt for 429, got %d", attempts)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("expected no internal backoff for 429, elapsed=%v", elapsed)
	}
}

func TestRetryStopsOn4xxImmediately(t *testing.T) {
	attempts := 0
	_, err := Retry(context.Background(), 3, func() (int, error) {
		attempts++
		return 0, &HTTPError{StatusCode: http.StatusUnauthorized}
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Fatalf("expected single attempt for 4xx, got %d", attempts)
	}
}

func TestRetryHonorsContextDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	attempts := 0
	_, err := Retry(ctx, 3, func() (int, error) {
		attempts++
		return 0, &HTTPError{StatusCode: http.StatusInternalServerError}
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("expected one attempt before context cancel, got %d", attempts)
	}
}
