package runtime

import (
	"strings"
	"testing"
)

func TestInferTensorRTInstalled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		hasNativeBinary bool
		hasServiceUnit  bool
		hasConfig       bool
		hasEnv          bool
		want            bool
	}{
		{
			name:            "not installed with no artifacts",
			hasNativeBinary: false,
			hasServiceUnit:  false,
			hasConfig:       false,
			hasEnv:          false,
			want:            false,
		},
		{
			name:            "installed when native binary exists",
			hasNativeBinary: true,
			hasServiceUnit:  false,
			hasConfig:       false,
			hasEnv:          false,
			want:            true,
		},
		{
			name:            "installed when managed unit exists",
			hasNativeBinary: false,
			hasServiceUnit:  true,
			hasConfig:       false,
			hasEnv:          false,
			want:            true,
		},
		{
			name:            "installed when managed config exists",
			hasNativeBinary: false,
			hasServiceUnit:  false,
			hasConfig:       true,
			hasEnv:          false,
			want:            true,
		},
		{
			name:            "installed when managed env exists",
			hasNativeBinary: false,
			hasServiceUnit:  false,
			hasConfig:       false,
			hasEnv:          true,
			want:            true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := inferTensorRTInstalled(tc.hasNativeBinary, tc.hasServiceUnit, tc.hasConfig, tc.hasEnv)
			if got != tc.want {
				t.Fatalf("inferTensorRTInstalled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInferVLLMInstalled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		hasNativeImport bool
		hasServiceUnit  bool
		hasConfig       bool
		hasEnv          bool
		want            bool
	}{
		{
			name:            "not installed with no artifacts",
			hasNativeImport: false,
			hasServiceUnit:  false,
			hasConfig:       false,
			hasEnv:          false,
			want:            false,
		},
		{
			name:            "installed when python import works",
			hasNativeImport: true,
			hasServiceUnit:  false,
			hasConfig:       false,
			hasEnv:          false,
			want:            true,
		},
		{
			name:            "installed when managed unit exists",
			hasNativeImport: false,
			hasServiceUnit:  true,
			hasConfig:       false,
			hasEnv:          false,
			want:            true,
		},
		{
			name:            "installed when managed config exists",
			hasNativeImport: false,
			hasServiceUnit:  false,
			hasConfig:       true,
			hasEnv:          false,
			want:            true,
		},
		{
			name:            "installed when managed env exists",
			hasNativeImport: false,
			hasServiceUnit:  false,
			hasConfig:       false,
			hasEnv:          true,
			want:            true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := inferVLLMInstalled(tc.hasNativeImport, tc.hasServiceUnit, tc.hasConfig, tc.hasEnv)
			if got != tc.want {
				t.Fatalf("inferVLLMInstalled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldAutoEnableTrustRemoteCode(t *testing.T) {
	t.Parallel()

	driver := &vllmDriver{}
	cases := []struct {
		name        string
		diagnostics string
		want        bool
	}{
		{
			name:        "detects trust_remote_code token",
			diagnostics: "Value error: please pass trust_remote_code=True",
			want:        true,
		},
		{
			name:        "detects trust-remote-code token",
			diagnostics: "pass --trust-remote-code to continue",
			want:        true,
		},
		{
			name:        "detects custom code phrasing",
			diagnostics: "The repository contains custom code which must be executed.",
			want:        true,
		},
		{
			name:        "detects allow custom code phrasing",
			diagnostics: "Please allow custom code to be run.",
			want:        true,
		},
		{
			name:        "does not match unrelated diagnostics",
			diagnostics: "dial tcp 127.0.0.1:8000: connect: connection refused",
			want:        false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := driver.shouldAutoEnableTrustRemoteCode(tc.diagnostics)
			if got != tc.want {
				t.Fatalf("shouldAutoEnableTrustRemoteCode() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestKnownModelImageIncompatibility(t *testing.T) {
	t.Parallel()

	driver := &vllmDriver{}
	cases := []struct {
		name           string
		modelID        string
		containerImage string
		wantBlocked    bool
	}{
		{
			name:           "blocks known incompatible nemotron model on 26.02 image",
			modelID:        "nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4",
			containerImage: "nvcr.io/nvidia/vllm:26.02-py3",
			wantBlocked:    true,
		},
		{
			name:           "does not block same model on different image",
			modelID:        "nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4",
			containerImage: "nvcr.io/nvidia/vllm:26.03-py3",
			wantBlocked:    false,
		},
		{
			name:           "does not block different model",
			modelID:        "meta-llama/Llama-3.1-8B-Instruct",
			containerImage: "nvcr.io/nvidia/vllm:26.02-py3",
			wantBlocked:    false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := driver.knownModelImageIncompatibility(tc.modelID, tc.containerImage)
			blocked := strings.TrimSpace(got) != ""
			if blocked != tc.wantBlocked {
				t.Fatalf("knownModelImageIncompatibility() blocked=%v, want %v (msg=%q)", blocked, tc.wantBlocked, got)
			}
		})
	}
}

func TestLlamaCppAuthPasskeyError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "matches invalid passkey message",
			output: "Invalid passkey for lms CLI client. Please make sure you are using the bundled lms binary.",
			want:   true,
		},
		{
			name:   "matches bundled lms hint",
			output: "Please make sure you are using the bundled lms binary.",
			want:   true,
		},
		{
			name:   "ignores unrelated output",
			output: "model unloaded successfully",
			want:   false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := llamacppAuthPasskeyError(tc.output)
			if got != tc.want {
				t.Fatalf("llamacppAuthPasskeyError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLlamaCppModelAlreadyRemoved(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "matches not loaded error",
			output: "Model is not loaded",
			want:   true,
		},
		{
			name:   "matches no model loaded error",
			output: "No model is loaded",
			want:   true,
		},
		{
			name:   "ignores unknown failure",
			output: "network timeout",
			want:   false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := llamacppModelAlreadyRemoved(tc.output)
			if got != tc.want {
				t.Fatalf("llamacppModelAlreadyRemoved() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCollapseLMStudioModelIDs(t *testing.T) {
	t.Parallel()

	input := []string{
		"google/gemma-4-26b-a4b",
		"google/gemma-4-26b-a4b:2",
		"google/gemma-4-26b-a4b:3",
		"meta-llama/Llama-3.1-8B-Instruct",
		"meta-llama/Llama-3.1-8B-Instruct:2",
	}
	got := collapseLMStudioModelIDs(input)
	want := []string{
		"google/gemma-4-26b-a4b",
		"meta-llama/Llama-3.1-8B-Instruct",
	}
	if len(got) != len(want) {
		t.Fatalf("collapseLMStudioModelIDs() len=%d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("collapseLMStudioModelIDs()[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestLlamaCppModelIDsEquivalent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		left  string
		right string
		want  bool
	}{
		{
			name:  "equivalent for suffixed id",
			left:  "google/gemma-4-26b-a4b",
			right: "google/gemma-4-26b-a4b:4",
			want:  true,
		},
		{
			name:  "not equivalent for different models",
			left:  "google/gemma-4-26b-a4b",
			right: "meta-llama/Llama-3.1-8B-Instruct",
			want:  false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := llamacppModelIDsEquivalent(tc.left, tc.right)
			if got != tc.want {
				t.Fatalf("llamacppModelIDsEquivalent() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestContainsOOMSignal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{
			name:  "detects cuda oom",
			value: "torch.cuda.OutOfMemoryError: CUDA out of memory",
			want:  true,
		},
		{
			name:  "detects generic insufficient memory",
			value: "failed to start model: insufficient memory for allocation",
			want:  true,
		},
		{
			name:  "ignores unrelated error",
			value: "dial tcp 127.0.0.1:8000: connect: connection refused",
			want:  false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := containsOOMSignal(tc.value)
			if got != tc.want {
				t.Fatalf("containsOOMSignal() = %v, want %v", got, tc.want)
			}
		})
	}
}
