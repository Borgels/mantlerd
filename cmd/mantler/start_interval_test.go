package main

import (
	"testing"
	"time"
)

func TestNextCheckinIntervalRanges(t *testing.T) {
	idle := nextCheckinInterval(30*time.Second, false)
	if idle < 24*time.Second || idle >= 36*time.Second {
		t.Fatalf("idle jitter out of range: %v", idle)
	}

	active := nextCheckinInterval(60*time.Second, true)
	if active < 12*time.Second || active >= 18*time.Second {
		t.Fatalf("active jitter out of range: %v", active)
	}

	fallback := nextCheckinInterval(0, false)
	if fallback < 12*time.Second || fallback >= 18*time.Second {
		t.Fatalf("fallback jitter out of range: %v", fallback)
	}
}

func TestComputeCheckinDelayRateLimitPrecedenceAndCap(t *testing.T) {
	if got := computeCheckinDelay(30*time.Second, false, 3, 90*time.Second); got < 72*time.Second || got > 108*time.Second {
		t.Fatalf("expected jittered rate-limited delay in [72s, 108s], got %v", got)
	}
	if got := computeCheckinDelay(30*time.Second, false, 3, 30*time.Minute); got < 4*time.Minute || got > maxDegradedInterval {
		t.Fatalf("expected capped+jittered degraded delay in [4m, %v], got %v", maxDegradedInterval, got)
	}
}

func TestComputeCheckinDelayExponentialRange(t *testing.T) {
	delay := computeCheckinDelay(30*time.Second, false, 2, 0)
	// base interval is [24s, 36s), multiplied by 4 for two consecutive failures
	if delay < 96*time.Second || delay >= 144*time.Second {
		t.Fatalf("expected exponential backoff in [96s, 144s), got %v", delay)
	}
}

func TestComputeCheckinDelayFailureCap(t *testing.T) {
	delay := computeCheckinDelay(5*time.Minute, false, 3, 0)
	if delay != maxDegradedInterval {
		t.Fatalf("expected capped delay of %v, got %v", maxDegradedInterval, delay)
	}
}
