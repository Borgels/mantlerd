package commands

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestSanitizeRuntimeProfileDestination(t *testing.T) {
	tempDir := t.TempDir()
	oldRoot := runtimeProfileStateRoot
	runtimeProfileStateRoot = tempDir
	defer func() { runtimeProfileStateRoot = oldRoot }()

	valid := filepath.Join(tempDir, "nested", "profile.txt")
	got, err := sanitizeRuntimeProfileDestination(valid)
	if err != nil {
		t.Fatalf("expected valid destination, got error: %v", err)
	}
	if got != valid {
		t.Fatalf("unexpected sanitized destination: %s", got)
	}

	if _, err := sanitizeRuntimeProfileDestination("/tmp/outside-root"); err == nil {
		t.Fatalf("expected destination outside root to fail")
	}
}

func TestFilterRuntimeProfileEnv(t *testing.T) {
	filtered := filterRuntimeProfileEnv("vllm", map[string]string{
		"VLLM_PORT":    "8000",
		"CUDA_VISIBLE": "0",
		"LD_PRELOAD":   "/tmp/libhack.so",
	})
	if _, ok := filtered["VLLM_PORT"]; !ok {
		t.Fatalf("expected VLLM_PORT to be preserved")
	}
	if _, ok := filtered["LD_PRELOAD"]; ok {
		t.Fatalf("expected LD_PRELOAD to be filtered out")
	}
}

func TestDownloadProfileFileRequiresHTTPS(t *testing.T) {
	tempDir := t.TempDir()
	dest := filepath.Join(tempDir, "profile.txt")
	err := downloadProfileFile("http://example.com/profile.txt", dest)
	if err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("expected non-https source to fail, got: %v", err)
	}
}

func TestDownloadProfileFileWritesSecureFile(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("profile-data"))
	}))
	defer server.Close()
	oldClient := profileDownloadHTTPClient
	profileDownloadHTTPClient = server.Client()
	defer func() { profileDownloadHTTPClient = oldClient }()

	tempDir := t.TempDir()
	dest := filepath.Join(tempDir, "profile.txt")
	if err := downloadProfileFile(server.URL, dest); err != nil {
		t.Fatalf("downloadProfileFile failed: %v", err)
	}
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(raw) != "profile-data" {
		t.Fatalf("unexpected downloaded content: %q", string(raw))
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat downloaded file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("expected file mode 0600, got %#o", info.Mode().Perm())
	}
}
