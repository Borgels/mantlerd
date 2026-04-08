package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUserConfigFallbackPath(t *testing.T) {
	fallback, ok := userConfigFallbackPath("/home/abo/.mantler/agent.json")
	if !ok {
		t.Fatalf("expected fallback for user config path")
	}
	if fallback != "/etc/mantler/agent.json" {
		t.Fatalf("unexpected fallback path: %s", fallback)
	}
	if _, ok := userConfigFallbackPath("/tmp/agent.json"); ok {
		t.Fatalf("did not expect fallback for non-user path")
	}
}

func TestLoadFromUserPathFallsBackToEtcConfig(t *testing.T) {
	tempHome := t.TempDir()
	homeConfig := filepath.Join(tempHome, ".mantler", "agent.json")
	if err := os.MkdirAll("/etc/mantler", 0o755); err != nil {
		t.Skipf("cannot create /etc/mantler in test env: %v", err)
	}
	etcConfig := "/etc/mantler/agent.json"
	original, hadOriginal := readIfExists(etcConfig)
	t.Cleanup(func() {
		if hadOriginal {
			_ = os.WriteFile(etcConfig, original, 0o600)
		} else {
			_ = os.Remove(etcConfig)
		}
	})
	payload := []byte("{\"serverUrl\":\"http://localhost:3000\",\"token\":\"tok\",\"machineId\":\"m1\",\"intervalMs\":30000,\"insecure\":true,\"logLevel\":\"info\"}\n")
	if err := os.WriteFile(etcConfig, payload, 0o600); err != nil {
		t.Skipf("cannot write /etc/mantler/agent.json in test env: %v", err)
	}

	cfg, err := Load(homeConfig)
	if err != nil {
		t.Fatalf("Load() fallback failed: %v", err)
	}
	if cfg.ServerURL != "http://localhost:3000" || cfg.Token != "tok" || cfg.MachineID != "m1" {
		t.Fatalf("unexpected loaded config: %+v", cfg)
	}
}

func readIfExists(path string) ([]byte, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return raw, true
}
