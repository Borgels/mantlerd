package eval

import "testing"

func TestValidatePromptsRejectsDuplicates(t *testing.T) {
	_, err := validatePrompts([]Prompt{
		{ID: "p1", Category: "quality", Workload: "coding", Prompt: "a"},
		{ID: "p1", Category: "speed", Workload: "coding", Prompt: "b"},
	}, "coding")
	if err == nil {
		t.Fatalf("expected duplicate prompt error")
	}
}

func TestValidatePromptsRejectsWorkloadMismatch(t *testing.T) {
	_, err := validatePrompts([]Prompt{
		{ID: "p1", Category: "quality", Workload: "chat", Prompt: "a"},
	}, "coding")
	if err == nil {
		t.Fatalf("expected workload mismatch error")
	}
}

func TestValidatePromptsNormalizesDefaults(t *testing.T) {
	prompts, err := validatePrompts([]Prompt{
		{ID: " p1 ", Category: "quality", Workload: "coding", Prompt: "  hello  "},
	}, "coding")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("expected 1 prompt, got %d", len(prompts))
	}
	if prompts[0].ID != "p1" {
		t.Fatalf("expected trimmed ID, got %q", prompts[0].ID)
	}
	if prompts[0].Prompt != "hello" {
		t.Fatalf("expected trimmed prompt, got %q", prompts[0].Prompt)
	}
	if prompts[0].MaxTokens != 128 {
		t.Fatalf("expected default max tokens 128, got %d", prompts[0].MaxTokens)
	}
}
