package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

func (h *Handler) handleCompress(
	ctx context.Context,
	envelope types.StageEnvelope,
	input map[string]any,
	inputHash string,
	startedAt time.Time,
) (map[string]any, error) {
	if envelope.Continuation == nil {
		return nil, errors.New("missing_continuation")
	}
	compressed, err := compressViaLocalRuntime(ctx, input)
	if err != nil {
		return nil, err
	}
	if err := ValidateCompressedContext(compressed); err != nil {
		return nil, err
	}
	outputHash, err := HashCanonicalJSON(compressed)
	if err != nil {
		return nil, errors.New("compression_contract_violation")
	}
	runtimeName := resolveRuntimeName(input)
	modelID := resolveModelID(input)
	payload, err := json.Marshal(compressed)
	if err != nil {
		return nil, errors.New("compression_contract_violation")
	}
	encryptedPayload, nonce, ephemeralPublicKey, err := encryptStagePayload(
		payload,
		envelope.RequestID,
		envelope.Continuation.NextTargetEncryptionKey,
	)
	if err != nil {
		return nil, err
	}
	integrity := h.buildIntegrity(
		envelope,
		modelID,
		runtimeName,
		inputHash,
		outputHash,
		estimateTokensFromMap(input),
		estimateTokensFromMap(compressed),
		startedAt,
	)
	return map[string]any{
		"encryptedPayload":   encryptedPayload,
		"nonce":              nonce,
		"ephemeralPublicKey": ephemeralPublicKey,
		"integrity":          integrity,
		"continuation":       envelope.Continuation,
	}, nil
}

const compressionExtractionPrompt = `You are a deterministic context compressor.
Return ONLY JSON that exactly matches:
{
  "contractVersion":"1",
  "summary":"...",
  "preservedFacts":["..."],
  "decisions":["..."],
  "referencedArtifacts":[{"kind":"code|url|file|data","reference":"...","description":"..."}],
  "unresolvedQuestions":["..."],
  "latestUserIntent":"...",
  "toolState":{"activeTools":[],"pendingCalls":[{"name":"...","status":"..."}]}
}`

func compressViaLocalRuntime(ctx context.Context, input map[string]any) (map[string]any, error) {
	runtimeName := resolveRuntimeName(input)
	modelID := resolveModelID(input)
	messages := append(
		[]any{
			map[string]any{
				"role":    "system",
				"content": compressionExtractionPrompt,
			},
		},
		normalizeMessages(input)...,
	)
	requestBody := map[string]any{
		"model":       modelID,
		"messages":    messages,
		"stream":      false,
		"temperature": 0,
	}
	response, err := callLocalChatCompletion(ctx, runtimeName, requestBody)
	if err != nil {
		return nil, err
	}
	content := firstChoiceContent(response)
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("compression_contract_violation")
	}
	decoded, err := parseJSONBlock(content)
	if err != nil {
		return nil, errors.New("compression_contract_violation")
	}
	if decoded["toolState"] == nil {
		decoded["toolState"] = map[string]any{
			"activeTools":  []any{},
			"pendingCalls": []any{},
		}
	}
	return decoded, nil
}

func resolveModelID(input map[string]any) string {
	if model := stringFromAny(input["model"]); model != "" {
		return model
	}
	if toolState, ok := input["toolState"].(map[string]any); ok {
		if model := stringFromAny(toolState["model"]); model != "" {
			return model
		}
	}
	return "local-model"
}

func resolveRuntimeName(input map[string]any) string {
	if runtime := stringFromAny(input["runtime"]); runtime != "" {
		return runtime
	}
	if toolState, ok := input["toolState"].(map[string]any); ok {
		if runtime := stringFromAny(toolState["runtime"]); runtime != "" {
			return runtime
		}
	}
	return defaultLocalRuntime
}

func normalizeMessages(input map[string]any) []any {
	if messages, ok := input["messages"].([]any); ok && len(messages) > 0 {
		return messages
	}
	if prompt := stringFromAny(input["prompt"]); prompt != "" {
		return []any{map[string]any{"role": "user", "content": prompt}}
	}
	return []any{map[string]any{"role": "user", "content": "Summarize the current request context."}}
}

func firstChoiceContent(response map[string]any) string {
	choices, ok := response["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return ""
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return ""
	}
	if content := stringFromAny(message["content"]); content != "" {
		return content
	}
	contentParts, ok := message["content"].([]any)
	if !ok || len(contentParts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(contentParts))
	for _, entry := range contentParts {
		if text := contentTextFromPart(entry); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func contentTextFromPart(value any) string {
	if text := stringFromAny(value); text != "" {
		return text
	}
	part, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if text := stringFromAny(part["text"]); text != "" {
		return text
	}
	if text := stringFromAny(part["content"]); text != "" {
		return text
	}
	return ""
}

func parseJSONBlock(value string) (map[string]any, error) {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func extractLatestUserIntent(input map[string]any) string {
	if messages, ok := input["messages"].([]any); ok {
		for index := len(messages) - 1; index >= 0; index-- {
			entry, ok := messages[index].(map[string]any)
			if !ok {
				continue
			}
			role, _ := entry["role"].(string)
			content, _ := entry["content"].(string)
			if role == "user" && strings.TrimSpace(content) != "" {
				return truncate(content, 600)
			}
		}
	}
	return "Answer the latest user request."
}

func extractFacts(input map[string]any) []any {
	facts := make([]any, 0, 3)
	if model, ok := input["model"].(string); ok && strings.TrimSpace(model) != "" {
		facts = append(facts, "Requested model: "+model)
	}
	if stream, ok := input["stream"].(bool); ok {
		if stream {
			facts = append(facts, "Streaming response requested.")
		} else {
			facts = append(facts, "Non-streaming response requested.")
		}
	}
	if len(facts) == 0 {
		facts = append(facts, "No explicit request metadata.")
	}
	return facts
}

func truncate(value string, max int) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= max {
		return trimmed
	}
	return trimmed[:max]
}
