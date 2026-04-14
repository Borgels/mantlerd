package pipeline

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

func (h *Handler) handleInfer(
	ctx context.Context,
	envelope types.StageEnvelope,
	input map[string]any,
	inputHash string,
	startedAt time.Time,
) (map[string]any, error) {
	if err := ValidateCompressedContext(input); err != nil {
		return nil, err
	}
	if envelope.PriorIntegrity == nil || envelope.PriorStageSigningKey == "" {
		return nil, errors.New("stage_integrity_missing")
	}
	if envelope.PriorIntegrity.OutputHash != inputHash {
		return nil, errors.New("stage_integrity_mismatch")
	}
	if !verifyIntegritySignature(
		SerializeIntegrityForSigning(*envelope.PriorIntegrity),
		envelope.PriorIntegrity.Signature,
		envelope.PriorStageSigningKey,
	) {
		return nil, errors.New("stage_integrity_mismatch")
	}

	runtimeName := resolveRuntimeName(input)
	modelID := resolveModelID(input)
	request := map[string]any{
		"model": modelID,
		"messages": []any{
			map[string]any{
				"role":    "system",
				"content": buildInferSystemPrompt(input),
			},
			map[string]any{
				"role":    "user",
				"content": inferLatestIntent(input),
			},
		},
		"stream": false,
	}
	completion, err := callLocalChatCompletion(ctx, runtimeName, request)
	if err != nil {
		return nil, err
	}
	outputHash, err := HashCanonicalJSON(completion)
	if err != nil {
		return nil, errors.New("infer_output_hash_failed")
	}
	integrity := h.buildIntegrity(
		envelope,
		modelID,
		runtimeName,
		inputHash,
		outputHash,
		estimateTokensFromMap(input),
		estimateTokensFromMap(completion),
		startedAt,
	)
	return map[string]any{
		"body":      completion,
		"integrity": integrity,
	}, nil
}

func buildInferSystemPrompt(input map[string]any) string {
	parts := make([]string, 0, 6)
	parts = append(parts, "Use the provided compressed context to answer accurately.")
	if summary := stringFromAny(input["summary"]); summary != "" {
		parts = append(parts, "Summary: "+summary)
	}
	parts = append(parts, formatArrayLine("Preserved facts", input["preservedFacts"]))
	parts = append(parts, formatArrayLine("Decisions", input["decisions"]))
	parts = append(parts, formatArtifactsLine("Referenced artifacts", input["referencedArtifacts"]))
	parts = append(parts, formatArrayLine("Unresolved questions", input["unresolvedQuestions"]))
	return strings.Join(parts, "\n")
}

func inferLatestIntent(input map[string]any) string {
	if latest := stringFromAny(input["latestUserIntent"]); latest != "" {
		return latest
	}
	return "Respond to the latest request using the compressed context."
}

func formatArrayLine(label string, value any) string {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return label + ": none"
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if text := stringFromAny(item); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return label + ": none"
	}
	return label + ": " + strings.Join(parts, "; ")
}

func formatArtifactsLine(label string, value any) string {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return label + ": none"
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		artifact, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind := stringFromAny(artifact["kind"])
		reference := stringFromAny(artifact["reference"])
		description := stringFromAny(artifact["description"])
		if kind == "" && reference == "" && description == "" {
			continue
		}
		parts = append(parts, "["+kind+"] "+reference+": "+description)
	}
	if len(parts) == 0 {
		return label + ": none"
	}
	return label + ": " + strings.Join(parts, "; ")
}
