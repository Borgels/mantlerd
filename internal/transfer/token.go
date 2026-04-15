package transfer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	tokenVersion  = "t1"
	tokenTTL      = 15 * time.Minute
)

// TokenClaims are embedded inside a transfer token.
type TokenClaims struct {
	// RequesterMachineID is the machine that will download the file.
	RequesterMachineID string `json:"rid"`
	// ServedMachineID is the machine being asked to serve the file.
	ServedMachineID string `json:"sid"`
	// ModelID is the catalog/Ollama model identifier being requested.
	ModelID string `json:"mid"`
	// Runtime is the runtime ("ollama", "vllm", etc.).
	Runtime string `json:"rt"`
	// IssuedAt is the Unix timestamp when the token was created.
	IssuedAt int64 `json:"iat"`
	// ExpiresAt is the Unix timestamp when the token expires.
	ExpiresAt int64 `json:"exp"`
}

// CreateToken signs a new transfer token with the provided HMAC secret.
// The secret should be the org-scoped TransferSecret from the server.
func CreateToken(secret, requesterMachineID, servedMachineID, modelID, runtime string) (string, error) {
	if secret == "" {
		return "", errors.New("transfer: token secret must not be empty")
	}
	now := time.Now()
	claims := TokenClaims{
		RequesterMachineID: requesterMachineID,
		ServedMachineID:    servedMachineID,
		ModelID:            modelID,
		Runtime:            runtime,
		IssuedAt:           now.Unix(),
		ExpiresAt:          now.Add(tokenTTL).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("transfer: marshal token claims: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	sig := computeHMAC(secret, tokenVersion+"."+encoded)
	return tokenVersion + "." + encoded + "." + sig, nil
}

// VerifyToken validates the token signature and expiry, returning the claims.
// requesterMachineID is the machine ID from the HTTP request (from agent auth).
func VerifyToken(secret, token, servedMachineID string) (*TokenClaims, error) {
	if secret == "" {
		return nil, errors.New("transfer: token secret must not be empty")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("transfer: malformed token")
	}
	version, encoded, sig := parts[0], parts[1], parts[2]
	if version != tokenVersion {
		return nil, fmt.Errorf("transfer: unsupported token version %q", version)
	}
	expectedSig := computeHMAC(secret, version+"."+encoded)
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return nil, errors.New("transfer: invalid token signature")
	}

	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("transfer: decode token: %w", err)
	}
	var claims TokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("transfer: parse token claims: %w", err)
	}
	if time.Now().Unix() > claims.ExpiresAt {
		return nil, errors.New("transfer: token has expired")
	}
	if claims.ServedMachineID != servedMachineID {
		return nil, fmt.Errorf("transfer: token not issued for this machine (got %q, want %q)",
			claims.ServedMachineID, servedMachineID)
	}
	return &claims, nil
}

func computeHMAC(secret, message string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
