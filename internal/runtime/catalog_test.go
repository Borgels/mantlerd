package runtime

import "testing"

func TestSupportedRuntimeNames(t *testing.T) {
	names := SupportedRuntimeNames()
	want := []string{"llamacpp", "mlx", "ollama", "quantcpp", "tensorrt", "vllm"}
	if len(names) != len(want) {
		t.Fatalf("SupportedRuntimeNames length = %d, want %d", len(names), len(want))
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("SupportedRuntimeNames[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestRuntimeCatalogMapsLlamaCppToLlamaCPPFamily(t *testing.T) {
	catalog := RuntimeCatalog()
	for _, spec := range catalog {
		if spec.Name != "llamacpp" {
			continue
		}
		if spec.Family != FamilyLlamaCPP {
			t.Fatalf("llamacpp family = %q, want %q", spec.Family, FamilyLlamaCPP)
		}
		if len(spec.BackendVariants) == 0 {
			t.Fatalf("expected llamacpp backend variants to be declared")
		}
		return
	}
	t.Fatalf("llamacpp runtime spec not found")
}
