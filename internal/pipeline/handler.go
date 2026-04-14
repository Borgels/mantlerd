package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	stagecrypto "github.com/Borgels/mantlerd/internal/crypto"
	"github.com/Borgels/mantlerd/internal/types"
)

type Handler struct {
	keyMaterial stagecrypto.StageKeyMaterial
}

func NewHandler() (*Handler, error) {
	material, err := stagecrypto.LoadStageKeyMaterial()
	if err != nil {
		return nil, err
	}
	return &Handler{
		keyMaterial: material,
	}, nil
}

func (h *Handler) HandleStageRequest(ctx context.Context, envelope types.StageEnvelope) (map[string]any, error) {
	if len(h.keyMaterial.EncryptionPrivateKey) == 0 || len(h.keyMaterial.SigningPrivateKey) == 0 {
		return nil, errors.New("stage_keys_unavailable")
	}
	if envelope.StageKind == "" {
		return nil, errors.New("missing_stage_kind")
	}
	plaintext, err := decryptStagePayload(
		envelope.EncryptedPayload,
		envelope.Nonce,
		envelope.EphemeralPublicKey,
		envelope.RequestID,
		h.keyMaterial.EncryptionPrivateKey,
	)
	if err != nil {
		return nil, err
	}
	defer zeroBytes(plaintext)

	var parsed map[string]any
	if err := json.Unmarshal(plaintext, &parsed); err != nil {
		return nil, errors.New("invalid_payload_json")
	}
	inputHash, err := HashCanonicalJSON(parsed)
	if err != nil {
		return nil, errors.New("invalid_payload_json")
	}
	startedAt := time.Now()

	switch envelope.StageKind {
	case "compress":
		return h.handleCompress(ctx, envelope, parsed, inputHash, startedAt)
	case "infer":
		return h.handleInfer(ctx, envelope, parsed, inputHash, startedAt)
	default:
		return nil, errors.New("unsupported_stage_kind")
	}
}

func (h *Handler) buildIntegrity(
	envelope types.StageEnvelope,
	modelID string,
	runtimeID string,
	inputHash string,
	outputHash string,
	inputTokens int,
	outputTokens int,
	startedAt time.Time,
) *types.StageIntegrity {
	endedAt := time.Now()
	integrity := types.StageIntegrity{
		StageID:               envelope.StageID,
		StageKind:             envelope.StageKind,
		ContractVersion:       "1",
		ModelID:               modelID,
		RuntimeID:             runtimeID,
		InputHash:             inputHash,
		OutputHash:            outputHash,
		InputTokens:           inputTokens,
		OutputTokens:          outputTokens,
		DurationMs:            endedAt.Sub(startedAt).Milliseconds(),
		Timestamp:             endedAt.UTC().Format(time.RFC3339),
		MachineKeyFingerprint: h.keyMaterial.Fingerprint,
	}
	integrity.Signature = signIntegrityPayload(
		SerializeIntegrityForSigning(integrity),
		h.keyMaterial.SigningPrivateKey,
	)
	return &integrity
}

func zeroBytes(value []byte) {
	for idx := range value {
		value[idx] = 0
	}
}
