package pipeline

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

func RegisterHTTPRoute(mux *http.ServeMux, handler *Handler) {
	if mux == nil || handler == nil {
		return
	}
	mux.HandleFunc("/v1/pipeline/stage", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		defer r.Body.Close()
		var envelope types.StageEnvelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			http.Error(w, "invalid_envelope", http.StatusBadRequest)
			return
		}
		if !verifyPipelineSignature(r, envelope) {
			http.Error(w, "invalid_signature", http.StatusUnauthorized)
			return
		}
		result, err := handler.HandleStageRequest(r.Context(), envelope)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
}

func verifyPipelineSignature(r *http.Request, envelope types.StageEnvelope) bool {
	secret := strings.TrimSpace(os.Getenv("PIPELINE_STAGE_SHARED_SECRET"))
	if secret == "" {
		return false
	}
	signature := strings.TrimSpace(r.Header.Get("X-Pipeline-Signature"))
	timestamp := strings.TrimSpace(r.Header.Get("X-Pipeline-Timestamp"))
	if signature == "" || timestamp == "" {
		return false
	}
	parsedTime, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return false
	}
	if time.Since(parsedTime) > 60*time.Second || time.Until(parsedTime) > 60*time.Second {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(envelope.RequestID))
	mac.Write([]byte(envelope.StageID))
	mac.Write([]byte(envelope.TargetMachineID))
	mac.Write([]byte(timestamp))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}
