package types

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestScoreResponseContractSchemaExists(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	schemaPath := filepath.Join(root, "mantler", "lib", "agent-protocol", "schemas", "score-response.schema.json")
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read score contract schema: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse score contract schema: %v", err)
	}
	if _, ok := parsed["properties"]; !ok {
		t.Fatalf("score contract schema missing properties")
	}
}

func TestScoreResponseMarshalingContainsContractFields(t *testing.T) {
	score := ScoreResponse{
		MantleFingerprint: "fp",
		Overall:           1234,
		ProfileID:         "balanced",
		FormulaVersion:    1,
		ConfidenceTier:    "verified",
		EvidenceSignals:   4,
		EvidenceCount:     4,
		EvidenceBreakdown: EvidenceBreakdown{Verified: 2, Observed: 1, SelfReported: 1},
		RawSignals: ScoreRawSignals{},
		UpdatedAt:  "2026-01-01T00:00:00.000Z",
	}
	raw, err := json.Marshal(score)
	if err != nil {
		t.Fatalf("marshal score response: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode score response json: %v", err)
	}
	required := []string{
		"mantleFingerprint",
		"overall",
		"profileId",
		"formulaVersion",
		"confidenceTier",
		"evidenceSignals",
		"evidenceCount",
		"rawSignals",
		"updatedAt",
		"evidenceBreakdown",
	}
	for _, key := range required {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("missing required score key %s", key)
		}
	}
}
