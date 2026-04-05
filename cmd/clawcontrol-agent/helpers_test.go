package main

import (
	"testing"

	"github.com/Borgels/clawcontrol-agent/internal/types"
)

func TestOrchestratorReportsDiffer(t *testing.T) {
	left := []types.InstalledOrchestrator{{ID: "o1", Type: "crewai", Status: "ready", Version: "1.0.0", Detail: "Detected crewai"}}
	right := []types.InstalledOrchestrator{{ID: "o1", Type: "crewai", Status: "ready", Version: "1.0.0", Detail: "Detected crewai"}}
	if orchestratorReportsDiffer(left, right) {
		t.Fatalf("expected identical orchestrator reports to match")
	}

	right[0].Status = "offline"
	if !orchestratorReportsDiffer(left, right) {
		t.Fatalf("expected differing orchestrator reports to be detected")
	}
}

func TestToInstalledOrchestratorsBuiltinReady(t *testing.T) {
	installed := toInstalledOrchestrators(types.DesiredConfig{
		Orchestrators: []types.DesiredOrchestrator{{
			ID:   "builtin",
			Name: "ClawControl Pipeline",
			Type: "builtin",
		}},
	})

	if len(installed) != 1 {
		t.Fatalf("expected 1 orchestrator, got %d", len(installed))
	}
	if installed[0].Status != "ready" {
		t.Fatalf("expected builtin orchestrator to be ready, got %s", installed[0].Status)
	}
	if installed[0].Capabilities == nil || installed[0].Capabilities.SupportsQualityGates == nil || !*installed[0].Capabilities.SupportsQualityGates {
		t.Fatalf("expected builtin orchestrator capabilities to be populated")
	}
}
