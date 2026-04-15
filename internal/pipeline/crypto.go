package pipeline

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

const stageHKDFInfo = "mantler-stage-v1"

func decryptStagePayload(encryptedPayload string, nonce string, senderPublicKey string, requestID string, receiverPrivateKey []byte) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encryptedPayload)
	if err != nil {
		return nil, fmt.Errorf("invalid_encrypted_payload")
	}
	nonceRaw, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil {
		return nil, fmt.Errorf("invalid_nonce")
	}
	if len(nonceRaw) != 12 {
		return nil, fmt.Errorf("invalid_nonce")
	}
	senderKeyRaw, err := base64.StdEncoding.DecodeString(senderPublicKey)
	if err != nil {
		return nil, fmt.Errorf("invalid_ephemeral_public_key")
	}
	curve := ecdh.X25519()
	receiverKey, err := curve.NewPrivateKey(receiverPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("invalid_private_key")
	}
	senderKey, err := curve.NewPublicKey(senderKeyRaw)
	if err != nil {
		return nil, fmt.Errorf("invalid_ephemeral_public_key")
	}
	sharedSecret, err := receiverKey.ECDH(senderKey)
	if err != nil {
		return nil, fmt.Errorf("ecdh_failed")
	}
	key, err := deriveAESKey(sharedSecret, requestID)
	if err != nil {
		return nil, fmt.Errorf("hkdf_failed")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cipher_failed")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cipher_failed")
	}
	plaintext, err := aead.Open(nil, nonceRaw, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt_failed")
	}
	return plaintext, nil
}

func encryptStagePayload(plaintext []byte, requestID string, targetPublicKeyBase64 string) (encryptedPayload string, nonce string, ephemeralPublicKey string, err error) {
	targetKeyRaw, err := base64.StdEncoding.DecodeString(targetPublicKeyBase64)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid_target_public_key")
	}
	curve := ecdh.X25519()
	targetKey, err := curve.NewPublicKey(targetKeyRaw)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid_target_public_key")
	}
	ephemeralKey, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", "", fmt.Errorf("ephemeral_key_failed")
	}
	sharedSecret, err := ephemeralKey.ECDH(targetKey)
	if err != nil {
		return "", "", "", fmt.Errorf("ecdh_failed")
	}
	key, err := deriveAESKey(sharedSecret, requestID)
	if err != nil {
		return "", "", "", fmt.Errorf("hkdf_failed")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", "", fmt.Errorf("cipher_failed")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", "", fmt.Errorf("cipher_failed")
	}
	nonceRaw := make([]byte, 12)
	if _, err := rand.Read(nonceRaw); err != nil {
		return "", "", "", fmt.Errorf("nonce_failed")
	}
	ciphertext := aead.Seal(nil, nonceRaw, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ciphertext),
		base64.StdEncoding.EncodeToString(nonceRaw),
		base64.StdEncoding.EncodeToString(ephemeralKey.PublicKey().Bytes()),
		nil
}

func deriveAESKey(sharedSecret []byte, requestID string) ([]byte, error) {
	return hkdf.Key(sha256.New, sharedSecret, []byte(requestID), stageHKDFInfo, 32)
}

func signIntegrityPayload(payload []byte, privateKey ed25519.PrivateKey) string {
	signature := ed25519.Sign(privateKey, payload)
	return base64.StdEncoding.EncodeToString(signature)
}

func verifyIntegritySignature(payload []byte, signatureBase64 string, publicKeyBase64 string) bool {
	signatureRaw, err := base64.StdEncoding.DecodeString(signatureBase64)
	if err != nil {
		return false
	}
	publicKeyRaw, err := base64.StdEncoding.DecodeString(publicKeyBase64)
	if err != nil {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(publicKeyRaw), payload, signatureRaw)
}
