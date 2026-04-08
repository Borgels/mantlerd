package manifest

import (
	"testing"

	"github.com/Borgels/mantlerd/internal/types"
)

func TestPlanModelLoadingWithSnapshotRejectsSteadyStateOverflow(t *testing.T) {
	manifest := types.ResourceManifest{
		Models: []types.ManifestModel{
			{
				ModelID:        "model-a-34b",
				Runtime:        "vllm",
				MachineID:      "m1",
				Source:         "machine",
				ParameterCount: "34B",
			},
			{
				ModelID:        "model-b-34b",
				Runtime:        "vllm",
				MachineID:      "m1",
				Source:         "machine",
				ParameterCount: "34B",
			},
		},
	}

	plan := PlanModelLoadingWithSnapshot(manifest, "m1", nil, MemorySnapshot{
		TotalMB: 80 * 1024,
		UsedMB:  10 * 1024,
		Known:   true,
		Source:  "gpu_vram",
	})

	if plan.SteadyStateFits {
		t.Fatalf("expected steady-state fit check to fail")
	}
	if plan.Sequential {
		t.Fatalf("did not expect sequential fallback when even a single load cannot fit safely")
	}
}

func TestPlanModelLoadingWithSnapshotAccountsForEjectingOldModels(t *testing.T) {
	manifest := types.ResourceManifest{
		Models: []types.ManifestModel{
			{
				ModelID:        "new-model-13b",
				Runtime:        "vllm",
				MachineID:      "m1",
				Source:         "machine",
				ParameterCount: "13B",
			},
		},
	}

	plan := PlanModelLoadingWithSnapshot(manifest, "m1", []string{"old-model-34b"}, MemorySnapshot{
		TotalMB: 80 * 1024,
		UsedMB:  60 * 1024,
		Known:   true,
		Source:  "gpu_vram",
	})

	if len(plan.EjectModelIDs) != 1 || plan.EjectModelIDs[0] != "old-model-34b" {
		t.Fatalf("expected old model to be ejected, got %#v", plan.EjectModelIDs)
	}
	if !plan.SteadyStateFits {
		t.Fatalf("expected plan to fit after reclaiming memory from ejected models")
	}
}

func TestPlanModelLoadingWithSnapshotUsesUnifiedMemoryHeadroom(t *testing.T) {
	manifest := types.ResourceManifest{
		Models: []types.ManifestModel{
			{
				ModelID:        "phi-4-14b",
				Runtime:        "llamacpp",
				MachineID:      "m1",
				Source:         "machine",
				ParameterCount: "14B",
			},
		},
	}

	plan := PlanModelLoadingWithSnapshot(manifest, "m1", nil, MemorySnapshot{
		TotalMB: 64 * 1024,
		UsedMB:  20 * 1024,
		Known:   true,
		Unified: true,
		Source:  "system_ram",
	})

	if plan.HeadroomMB < 4096 {
		t.Fatalf("expected larger unified-memory headroom, got %d", plan.HeadroomMB)
	}
	if plan.MemorySource != "system_ram" {
		t.Fatalf("expected system_ram source, got %q", plan.MemorySource)
	}
}
