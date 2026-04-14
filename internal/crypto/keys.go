package crypto

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

type StageKeys struct {
	EncryptionPublicKey string
	SigningPublicKey    string
	Fingerprint         string
}

type StageKeyMaterial struct {
	StageKeys
	EncryptionPrivateKey []byte
	SigningPrivateKey    ed25519.PrivateKey
}

type persistedStageKeys struct {
	X25519Private string `json:"x25519Private"`
	X25519Public  string `json:"x25519Public"`
	Ed25519Private string `json:"ed25519Private"`
	Ed25519Public  string `json:"ed25519Public"`
}

func EnsureStageKeys() (StageKeys, error) {
	material, err := LoadStageKeyMaterial()
	if err == nil {
		return material.StageKeys, nil
	}

	x25519Private, x25519Public, err := generateX25519Keypair()
	if err != nil {
		return StageKeys{}, err
	}
	edPrivate, edPublic, err := generateEd25519Keypair()
	if err != nil {
		return StageKeys{}, err
	}

	keys := deriveKeys(x25519Public, edPublic)
	persisted := persistedStageKeys{
		X25519Private: x25519Private,
		X25519Public:  x25519Public,
		Ed25519Private: edPrivate,
		Ed25519Public:  edPublic,
	}
	rawPersisted, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return StageKeys{}, err
	}
	statePath, err := stageKeysPath()
	if err != nil {
		return StageKeys{}, err
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return StageKeys{}, err
	}
	if err := os.WriteFile(statePath, rawPersisted, 0o600); err != nil {
		return StageKeys{}, err
	}
	return keys, nil
}

func LoadStageKeyMaterial() (StageKeyMaterial, error) {
	statePath, err := stageKeysPath()
	if err != nil {
		return StageKeyMaterial{}, err
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return StageKeyMaterial{}, err
	}
	var persisted persistedStageKeys
	if err := json.Unmarshal(raw, &persisted); err != nil {
		return StageKeyMaterial{}, err
	}
	encryptionPrivate, err := base64.StdEncoding.DecodeString(persisted.X25519Private)
	if err != nil {
		return StageKeyMaterial{}, err
	}
	signingPrivateRaw, err := base64.StdEncoding.DecodeString(persisted.Ed25519Private)
	if err != nil {
		return StageKeyMaterial{}, err
	}
	material := StageKeyMaterial{
		StageKeys:             deriveKeys(persisted.X25519Public, persisted.Ed25519Public),
		EncryptionPrivateKey:  encryptionPrivate,
		SigningPrivateKey:     ed25519.PrivateKey(signingPrivateRaw),
	}
	return material, nil
}

func stageKeysPath() (string, error) {
	stateDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, ".mantler", "stage-keys.json"), nil
}

func deriveKeys(enc string, sig string) StageKeys {
	encRaw, encErr := base64.StdEncoding.DecodeString(enc)
	sigRaw, sigErr := base64.StdEncoding.DecodeString(sig)
	fingerprintInput := []byte(enc + sig)
	if encErr == nil && sigErr == nil {
		fingerprintInput = append(append([]byte{}, encRaw...), sigRaw...)
	}
	sum := sha256.Sum256(fingerprintInput)
	return StageKeys{
		EncryptionPublicKey: enc,
		SigningPublicKey:    sig,
		Fingerprint:         hex.EncodeToString(sum[:]),
	}
}

func generateX25519Keypair() (string, string, error) {
	curve := ecdh.X25519()
	private, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(private.Bytes()),
		base64.StdEncoding.EncodeToString(private.PublicKey().Bytes()),
		nil
}

func generateEd25519Keypair() (string, string, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(private),
		base64.StdEncoding.EncodeToString(public),
		nil
}
