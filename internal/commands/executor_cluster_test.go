package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Borgels/mantlerd/internal/types"
)

func writeExecutable(t *testing.T, dir string, name string, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", name, err)
	}
	return path
}

func TestApplyDistributedStartModelParamsSetsSwitchEnv(t *testing.T) {
	tempDir := t.TempDir()
	oldVLLMEnvPath := vllmRuntimeEnvPath
	vllmRuntimeEnvPath = filepath.Join(tempDir, "vllm.env")
	defer func() { vllmRuntimeEnvPath = oldVLLMEnvPath }()

	if err := applyDistributedStartModelParams(map[string]interface{}{
		"clusterNodeCount":     4,
		"clusterTopology":      "qsfp_switch",
		"machineIds":           []interface{}{"m1", "m2", "m3", "m4"},
		"ncclSocketInterface":  "enp1s0f1np1",
		"tensorParallelSize":   4,
		"pipelineParallelSize": 4,
	}, "vllm"); err != nil {
		t.Fatalf("applyDistributedStartModelParams() error = %v", err)
	}

	values := readEnvFile(vllmRuntimeEnvPath)
	if values["UCX_NET_DEVICES"] != "enp1s0f1np1" {
		t.Fatalf("expected UCX_NET_DEVICES to be set, got %q", values["UCX_NET_DEVICES"])
	}
	if values["NCCL_SOCKET_IFNAME"] != "enp1s0f1np1" {
		t.Fatalf("expected NCCL_SOCKET_IFNAME to be set, got %q", values["NCCL_SOCKET_IFNAME"])
	}
	if values["OMPI_MCA_btl_tcp_if_include"] != "enp1s0f1np1" {
		t.Fatalf("expected OMPI_MCA_btl_tcp_if_include to be set, got %q", values["OMPI_MCA_btl_tcp_if_include"])
	}
	if !strings.Contains(values["VLLM_EXTRA_ARGS"], "--tensor-parallel-size 4") {
		t.Fatalf("expected tensor-parallel flag in VLLM_EXTRA_ARGS, got %q", values["VLLM_EXTRA_ARGS"])
	}
	if !strings.Contains(values["VLLM_EXTRA_ARGS"], "--pipeline-parallel-size 4") {
		t.Fatalf("expected pipeline-parallel flag in VLLM_EXTRA_ARGS, got %q", values["VLLM_EXTRA_ARGS"])
	}
}

func TestRunNCCLTestUsesMpiRunForQSFPSwitch(t *testing.T) {
	tempDir := t.TempDir()
	writeExecutable(t, tempDir, "all_reduce_perf", "#!/usr/bin/env sh\necho \"all_reduce_perf\"\n")
	writeExecutable(t, tempDir, "mpirun", "#!/usr/bin/env sh\necho \"mpirun args: $*\"\necho \"busbw 123.4\"\n")

	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", tempDir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatalf("set PATH: %v", err)
	}
	defer func() {
		_ = os.Setenv("PATH", oldPath)
	}()

	var outcome types.OutcomeEvent
	executor := &Executor{
		outcome: func(event types.OutcomeEvent) {
			outcome = event
		},
	}

	result, err := executor.runNCCLTest(context.Background(), types.AgentCommand{
		ID:   "cmd-1",
		Type: "nccl_test",
		Params: map[string]interface{}{
			"clusterNodeCount":    4,
			"clusterTopology":     "qsfp_switch",
			"peerAddresses":       []interface{}{"10.0.0.10", "10.0.0.11", "10.0.0.12", "10.0.0.13"},
			"ncclSocketInterface": "enp1s0f1np1",
		},
	})
	if err != nil {
		t.Fatalf("runNCCLTest() error = %v", err)
	}

	payload, ok := result.ResultPayload.(map[string]any)
	if !ok {
		t.Fatalf("expected result payload map, got %T", result.ResultPayload)
	}
	if used, _ := payload["usedMpiRun"].(bool); !used {
		t.Fatalf("expected usedMpiRun=true in payload, got %#v", payload["usedMpiRun"])
	}
	if !strings.Contains(result.Details, "mpirun args:") {
		t.Fatalf("expected mpirun output in details, got %q", result.Details)
	}
	if outcome.EventType != "nccl_test_success" {
		t.Fatalf("expected nccl_test_success outcome, got %q", outcome.EventType)
	}
	if outcome.ClusterTopology != "qsfp_switch" {
		t.Fatalf("expected qsfp_switch outcome topology, got %q", outcome.ClusterTopology)
	}
}
