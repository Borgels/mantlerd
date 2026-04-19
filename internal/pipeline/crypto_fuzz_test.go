//go:build go1.22

package pipeline

import (
	"encoding/base64"
	"testing"
)

func FuzzEncryptStagePayload(f *testing.F) {
	targetPublicKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	f.Add([]byte("hello"), "req-1")
	f.Fuzz(func(t *testing.T, data []byte, reqID string) {
		if reqID == "" {
			t.Skip()
		}
		_, _, _, _ = encryptStagePayload(data, reqID, targetPublicKey)
	})
}
