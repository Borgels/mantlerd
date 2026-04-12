package trainer

import "testing"

func TestResolveContainerSpec(t *testing.T) {
	spec, err := resolveContainerSpec("unsloth")
	if err != nil {
		t.Fatalf("expected unsloth spec, got error: %v", err)
	}
	if spec.Image == "" {
		t.Fatalf("expected non-empty image")
	}
}

func TestResolveContainerSpecRejectsUnknown(t *testing.T) {
	if _, err := resolveContainerSpec("unknown"); err == nil {
		t.Fatalf("expected unsupported trainer error")
	}
}

func TestFloatOrDefault(t *testing.T) {
	values := map[string]interface{}{
		"epochs": 4.0,
	}
	got := floatOrDefault(values, "epochs", "3")
	if got != "4" {
		t.Fatalf("expected 4, got %s", got)
	}
}
