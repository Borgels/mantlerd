package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"encoding/json"
	"sort"
	"strings"

	"github.com/Borgels/mantlerd/internal/types"
)

func CanonicalJSON(value any) ([]byte, error) {
	normalized := normalizeForCanonical(value)
	return json.Marshal(normalized)
}

func HashCanonicalJSON(value any) (string, error) {
	raw, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeForCanonical(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		next := make(map[string]any, len(keys))
		for _, key := range keys {
			next[key] = normalizeForCanonical(typed[key])
		}
		return next
	case []any:
		next := make([]any, 0, len(typed))
		for _, item := range typed {
			next = append(next, normalizeForCanonical(item))
		}
		return next
	default:
		return value
	}
}

func SerializeIntegrityForSigning(integrity types.StageIntegrity) []byte {
	fields := []string{
		integrity.StageID,
		integrity.StageKind,
		integrity.ContractVersion,
		integrity.ModelID,
		integrity.RuntimeID,
		integrity.InputHash,
		integrity.OutputHash,
		fmt.Sprintf("%d", integrity.InputTokens),
		fmt.Sprintf("%d", integrity.OutputTokens),
		fmt.Sprintf("%d", integrity.DurationMs),
		integrity.Timestamp,
		integrity.MachineKeyFingerprint,
	}
	return []byte(strings.Join(fields, "\x00"))
}

func ValidateCompressedContext(input map[string]any) error {
	requiredString := []string{"contractVersion", "summary", "latestUserIntent"}
	for _, key := range requiredString {
		value, ok := input[key]
		if !ok {
			return fmt.Errorf("compression_contract_violation")
		}
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return fmt.Errorf("compression_contract_violation")
		}
		if key == "contractVersion" && text != "1" {
			return fmt.Errorf("compression_contract_violation")
		}
	}
	requiredArray := []string{"preservedFacts", "decisions", "referencedArtifacts", "unresolvedQuestions"}
	for _, key := range requiredArray {
		rawArray, ok := input[key].([]any)
		if !ok {
			return fmt.Errorf("compression_contract_violation")
		}
		if key == "referencedArtifacts" {
			for _, item := range rawArray {
				entry, ok := item.(map[string]any)
				if !ok {
					return fmt.Errorf("compression_contract_violation")
				}
				kind := strings.TrimSpace(asString(entry["kind"]))
				if kind != "code" && kind != "url" && kind != "file" && kind != "data" {
					return fmt.Errorf("compression_contract_violation")
				}
				if strings.TrimSpace(asString(entry["reference"])) == "" ||
					strings.TrimSpace(asString(entry["description"])) == "" {
					return fmt.Errorf("compression_contract_violation")
				}
			}
		}
	}
	if rawToolState, exists := input["toolState"]; exists && rawToolState != nil {
		toolState, ok := rawToolState.(map[string]any)
		if !ok {
			return fmt.Errorf("compression_contract_violation")
		}
		if _, ok := toolState["activeTools"].([]any); !ok {
			return fmt.Errorf("compression_contract_violation")
		}
		pendingCalls, ok := toolState["pendingCalls"].([]any)
		if !ok {
			return fmt.Errorf("compression_contract_violation")
		}
		for _, pending := range pendingCalls {
			entry, ok := pending.(map[string]any)
			if !ok {
				return fmt.Errorf("compression_contract_violation")
			}
			if strings.TrimSpace(asString(entry["name"])) == "" ||
				strings.TrimSpace(asString(entry["status"])) == "" {
				return fmt.Errorf("compression_contract_violation")
			}
		}
	}
	return nil
}

func asString(value any) string {
	text, _ := value.(string)
	return text
}
