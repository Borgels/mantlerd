package main

import (
	"os"
	"testing"

	"github.com/Borgels/mantlerd/internal/types"
)

func TestShouldAutoEjectLlamaCpp(t *testing.T) {
	t.Setenv(llamaCppKeepWarmEnv, "")
	got := shouldAutoEjectLlamaCpp(types.DesiredConfig{
		Runtimes: []types.RuntimeType{types.RuntimeLlamaCpp, types.RuntimeVLLM},
	})
	if !got {
		t.Fatalf("shouldAutoEjectLlamaCpp() = %v, want true", got)
	}
}

func TestLlamaCppKeepWarmEnabled(t *testing.T) {
	orig, had := os.LookupEnv(llamaCppKeepWarmEnv)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(llamaCppKeepWarmEnv, orig)
			return
		}
		_ = os.Unsetenv(llamaCppKeepWarmEnv)
	})

	_ = os.Setenv(llamaCppKeepWarmEnv, "true")
	if !llamaCppKeepWarmEnabled() {
		t.Fatalf("expected keep warm enabled for true")
	}
	if shouldAutoEjectLlamaCpp(types.DesiredConfig{}) {
		t.Fatalf("expected auto-eject disabled when keep warm is enabled")
	}

	_ = os.Setenv(llamaCppKeepWarmEnv, "0")
	if llamaCppKeepWarmEnabled() {
		t.Fatalf("expected keep warm disabled for 0")
	}
	if !shouldAutoEjectLlamaCpp(types.DesiredConfig{}) {
		t.Fatalf("expected auto-eject enabled when keep warm is disabled")
	}
}
