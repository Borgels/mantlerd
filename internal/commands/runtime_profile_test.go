package commands

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyAndVerifyRuntimeProfileOverridesForVLLM(t *testing.T) {
	tempDir := t.TempDir()
	oldVLLMEnvPath := vllmRuntimeEnvPath
	oldStateRoot := runtimeProfileStateRoot
	oldMounts := runtimeMountPaths
	vllmRuntimeEnvPath = filepath.Join(tempDir, "etc", "mantler", "vllm.env")
	runtimeProfileStateRoot = filepath.Join(tempDir, "state")
	runtimeMountPaths = map[string]map[string]string{
		"vllm": {
			"/app": filepath.Join(tempDir, "app"),
		},
	}
	defer func() {
		vllmRuntimeEnvPath = oldVLLMEnvPath
		runtimeProfileStateRoot = oldStateRoot
		runtimeMountPaths = oldMounts
	}()

	requiredHostPath := filepath.Join(tempDir, "app", "super_v3_reasoning_parser.py")
	params := map[string]interface{}{
		"runtimeProfile": map[string]interface{}{
			"id":             "nemotron-super120b-nvfp4-vllm",
			"label":          "Nemotron Super 120B NVFP4 (vLLM nightly)",
			"runtime":        "vllm",
			"containerImage": "vllm/vllm-openai:cu130-nightly",
			"environmentVariables": map[string]interface{}{
				"VLLM_RUNTIME_MODE":             "container",
				"VLLM_ALLOW_LONG_MAX_MODEL_LEN": "1",
			},
			"extraArgs": []interface{}{
				"--trust-remote-code",
				"--reasoning-parser-plugin /app/super_v3_reasoning_parser.py",
			},
			"requiredFiles": []interface{}{
				map[string]interface{}{
					"source":      "file://ignored",
					"destination": "/app/super_v3_reasoning_parser.py",
				},
			},
		},
	}

	if err := os.MkdirAll(filepath.Dir(requiredHostPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requiredHostPath, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Bypass network download for this unit test: verify path handling via precreated file.
	profile, runtimeName, err := decodeRuntimeProfile(params, "vllm")
	if err != nil {
		t.Fatalf("decodeRuntimeProfile() error = %v", err)
	}
	if profile == nil {
		t.Fatal("expected profile")
	}
	profile.RequiredFiles = []requiredProfileFile{{Source: "file://ignored", Destination: "/app/super_v3_reasoning_parser.py"}}
	if err := writeEnvFile(vllmRuntimeEnvPath, map[string]string{
		"VLLM_CONTAINER_IMAGE":          profile.ContainerImage,
		"VLLM_RUNTIME_MODE":             profile.EnvironmentVariables["VLLM_RUNTIME_MODE"],
		"VLLM_ALLOW_LONG_MAX_MODEL_LEN": profile.EnvironmentVariables["VLLM_ALLOW_LONG_MAX_MODEL_LEN"],
		"VLLM_EXTRA_ARGS":               "--trust-remote-code --reasoning-parser-plugin /app/super_v3_reasoning_parser.py",
	}); err != nil {
		t.Fatalf("writeEnvFile() error = %v", err)
	}
	if err := verifyRuntimeProfileApplied(runtimeName, *profile); err != nil {
		t.Fatalf("verifyRuntimeProfileApplied() error = %v", err)
	}
	statePath := runtimeProfileStatePath("vllm")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("expected runtime profile state file at %s: %v", statePath, err)
	}
}
