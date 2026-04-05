package main

import (
	"os"
	"testing"

	"github.com/Borgels/clawcontrol-agent/internal/types"
)

func TestShouldAutoEjectLMStudio(t *testing.T) {
	t.Setenv(lmstudioKeepWarmEnv, "")
	got := shouldAutoEjectLMStudio(types.DesiredConfig{
		Runtimes: []types.RuntimeType{types.RuntimeLMStudio, types.RuntimeVLLM},
	})
	if !got {
		t.Fatalf("shouldAutoEjectLMStudio() = %v, want true", got)
	}
}

func TestLMStudioKeepWarmEnabled(t *testing.T) {
	orig, had := os.LookupEnv(lmstudioKeepWarmEnv)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(lmstudioKeepWarmEnv, orig)
			return
		}
		_ = os.Unsetenv(lmstudioKeepWarmEnv)
	})

	_ = os.Setenv(lmstudioKeepWarmEnv, "true")
	if !lmstudioKeepWarmEnabled() {
		t.Fatalf("expected keep warm enabled for true")
	}
	if shouldAutoEjectLMStudio(types.DesiredConfig{}) {
		t.Fatalf("expected auto-eject disabled when keep warm is enabled")
	}

	_ = os.Setenv(lmstudioKeepWarmEnv, "0")
	if lmstudioKeepWarmEnabled() {
		t.Fatalf("expected keep warm disabled for 0")
	}
	if !shouldAutoEjectLMStudio(types.DesiredConfig{}) {
		t.Fatalf("expected auto-eject enabled when keep warm is disabled")
	}
}
