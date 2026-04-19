//go:build go1.22

package pipeline

import (
	"crypto/rand"
	"crypto/ecdh"
	"encoding/base64"
	"testing"
)

func FuzzEncryptDecryptStagePayloadRoundTrip(f *testing.F) {
	receiverKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		f.Fatalf("failed to generate receiver key: %v", err)
	}
	receiverPublicKey := base64.StdEncoding.EncodeToString(receiverKey.PublicKey().Bytes())
	f.Add([]byte("hello"), "req-1")
	f.Fuzz(func(t *testing.T, data []byte, reqID string) {
		if reqID == "" {
			t.Skip()
		}
		ciphertext, nonce, senderPublic, err := encryptStagePayload(data, reqID, receiverPublicKey)
		if err != nil {
			return
		}
		plaintext, err := decryptStagePayload(ciphertext, nonce, senderPublic, reqID, receiverKey.Bytes())
		if err != nil {
			t.Fatalf("decrypt failed after successful encrypt: %v", err)
		}
		if string(plaintext) != string(data) {
			t.Fatalf("round-trip mismatch")
		}
	})
}
