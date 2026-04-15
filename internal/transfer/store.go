// Package transfer implements LAN peer-to-peer model distribution.
// Machines in the same org/subnet can serve model files to each other,
// avoiding repeated downloads from HuggingFace or the Ollama registry.
package transfer

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// DefaultOllamaModelsDir is the standard Ollama blob store location.
	DefaultOllamaModelsDir = "/root/.ollama/models"
	// DefaultHFCacheDir matches the install.sh environment variable.
	DefaultHFCacheDir = "/var/cache/huggingface/hub"
)

// ModelFile describes a model file that can be served to a peer.
type ModelFile struct {
	// Path is the absolute path to the file on disk.
	Path string
	// Digest is the sha256 digest of the file (without "sha256:" prefix).
	Digest string
	// Size is the file size in bytes.
	Size int64
}

// Store locates model files on disk for Ollama and HuggingFace runtimes.
type Store struct {
	ollamaModelsDir string
	hfCacheDir      string
}

// NewStore creates a Store with default paths derived from the environment.
func NewStore() *Store {
	ollamaDir := os.Getenv("OLLAMA_MODELS")
	if ollamaDir == "" {
		// Use the home directory if available; this also works when Ollama
		// hasn't run yet and the models dir doesn't exist (we create it).
		if home := os.Getenv("HOME"); home != "" {
			ollamaDir = filepath.Join(home, ".ollama", "models")
		}
	}
	if ollamaDir == "" {
		ollamaDir = DefaultOllamaModelsDir
	}

	hfDir := os.Getenv("HUGGINGFACE_HUB_CACHE")
	if hfDir == "" {
		hfDir = os.Getenv("HF_HOME")
	}
	if hfDir == "" {
		hfDir = DefaultHFCacheDir
	}

	return &Store{
		ollamaModelsDir: ollamaDir,
		hfCacheDir:      hfDir,
	}
}

// FindOllamaModel returns the GGUF blob file for an Ollama model by asking the
// local Ollama daemon for its manifest, then locating the largest layer blob.
// Falls back to a filesystem scan if the API is unavailable.
func (s *Store) FindOllamaModel(modelID string) (*ModelFile, error) {
	// Prefer the Ollama API approach (POST /api/show) — it gives us the exact
	// manifest layers without needing to know the on-disk layout.
	if f, err := s.findOllamaViaAPI(modelID); err == nil {
		return f, nil
	}
	// Fallback: scan the blob store directory.
	return s.findOllamaViaFilesystem(modelID)
}

// findOllamaViaAPI queries the local Ollama API for the model manifest.
func (s *Store) findOllamaViaAPI(modelID string) (*ModelFile, error) {
	ollamaBase := "http://127.0.0.1:11434"
	if v := os.Getenv("OLLAMA_HOST"); v != "" {
		v = strings.TrimRight(v, "/")
		if !strings.HasPrefix(v, "http") {
			v = "http://" + v
		}
		v = strings.ReplaceAll(v, "://0.0.0.0", "://127.0.0.1")
		ollamaBase = v
	}

	out, err := exec.Command("curl", "-sf", "-X", "POST",
		ollamaBase+"/api/show",
		"-H", "Content-Type: application/json",
		"-d", fmt.Sprintf(`{"name":%q}`, modelID),
	).Output()
	if err != nil {
		return nil, fmt.Errorf("ollama show: %w", err)
	}

	var result struct {
		Modelfile string `json:"modelfile"`
		Details   struct {
			Format string `json:"format"`
		} `json:"details"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("ollama show parse: %w", err)
	}

	// The Ollama modelfile contains FROM lines like:
	//   FROM /root/.ollama/models/blobs/sha256-abc123...
	blobsDir := filepath.Join(s.ollamaModelsDir, "blobs")
	var bestPath string
	var bestSize int64
	for _, line := range strings.Split(result.Modelfile, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "FROM ") {
			continue
		}
		candidate := strings.TrimSpace(line[5:])
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(blobsDir, candidate)
		}
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Size() > bestSize {
			bestPath = candidate
			bestSize = info.Size()
		}
	}

	if bestPath == "" {
		return nil, fmt.Errorf("no blob found in modelfile for %q", modelID)
	}

	digest, err := sha256FileHex(bestPath)
	if err != nil {
		return nil, fmt.Errorf("digest %q: %w", bestPath, err)
	}
	return &ModelFile{Path: bestPath, Digest: digest, Size: bestSize}, nil
}

// findOllamaViaFilesystem scans ~/.ollama/models/blobs for the largest file
// whose name contains the normalised model tag (best-effort fallback).
func (s *Store) findOllamaViaFilesystem(modelID string) (*ModelFile, error) {
	blobsDir := filepath.Join(s.ollamaModelsDir, "blobs")
	if _, err := os.Stat(blobsDir); err != nil {
		return nil, fmt.Errorf("ollama blobs dir not found: %w", err)
	}

	// Walk and pick the largest blob (heuristic: largest file is the model).
	var bestPath string
	var bestSize int64
	err := filepath.WalkDir(blobsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > bestSize {
			bestSize = info.Size()
			bestPath = path
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan ollama blobs: %w", err)
	}
	if bestPath == "" {
		return nil, fmt.Errorf("no blobs found for %q", modelID)
	}

	digest, err := sha256FileHex(bestPath)
	if err != nil {
		return nil, fmt.Errorf("digest %q: %w", bestPath, err)
	}
	return &ModelFile{Path: bestPath, Digest: digest, Size: bestSize}, nil
}

// FindHFModel returns the snapshot directory for a HuggingFace model in the
// hub cache. Returns the directory path (a directory of files/symlinks) that
// the HF hub stores for the given repo. Since the vLLM download is a
// snapshot (directory), the server streams a tar archive.
func (s *Store) FindHFModel(repoID string) (*ModelFile, error) {
	// HF hub layout: $HUGGINGFACE_HUB_CACHE/models--{org}--{name}/snapshots/{hash}/
	// Normalize repoID: "meta-llama/Llama-2-7b" → "models--meta-llama--Llama-2-7b"
	dirName := "models--" + strings.ReplaceAll(repoID, "/", "--")
	repoDir := filepath.Join(s.hfCacheDir, dirName)

	snapshotsDir := filepath.Join(repoDir, "snapshots")
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		return nil, fmt.Errorf("HF snapshots dir for %q: %w", repoID, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no snapshots for %q", repoID)
	}

	// Pick the most recently modified snapshot.
	var bestSnapshot string
	var bestTime int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Unix() > bestTime {
			bestTime = info.ModTime().Unix()
			bestSnapshot = filepath.Join(snapshotsDir, e.Name())
		}
	}
	if bestSnapshot == "" {
		return nil, fmt.Errorf("no snapshot directory for %q", repoID)
	}

	// Compute the total size of files in the snapshot.
	var totalSize int64
	_ = filepath.WalkDir(bestSnapshot, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err == nil {
			totalSize += info.Size()
		}
		return nil
	})

	return &ModelFile{Path: bestSnapshot, Size: totalSize}, nil
}

// ListAvailable returns all models currently available to serve.
func (s *Store) ListAvailable() []AvailableModel {
	var result []AvailableModel
	result = append(result, s.listOllamaModels()...)
	result = append(result, s.listHFModels()...)
	return result
}

// AvailableModel describes a model available for peer transfer.
type AvailableModel struct {
	ModelID  string
	Runtime  string
	Digest   string
	Size     int64
	FilePath string
}

func (s *Store) listOllamaModels() []AvailableModel {
	out, err := exec.Command("ollama", "list", "--json").Output()
	if err != nil {
		// Try plain text parse if --json not supported
		return s.listOllamaModelsPlain()
	}
	var models []struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	}
	if err := json.Unmarshal(out, &models); err != nil {
		return s.listOllamaModelsPlain()
	}
	result := make([]AvailableModel, 0, len(models))
	for _, m := range models {
		f, err := s.FindOllamaModel(m.Name)
		if err != nil {
			continue
		}
		result = append(result, AvailableModel{
			ModelID:  m.Name,
			Runtime:  "ollama",
			Digest:   f.Digest,
			Size:     f.Size,
			FilePath: f.Path,
		})
	}
	return result
}

func (s *Store) listOllamaModelsPlain() []AvailableModel {
	out, err := exec.Command("ollama", "list").Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(string(out), "\n")
	var result []AvailableModel
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(strings.ToUpper(line), "NAME") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		modelID := fields[0]
		f, err := s.FindOllamaModel(modelID)
		if err != nil {
			continue
		}
		result = append(result, AvailableModel{
			ModelID:  modelID,
			Runtime:  "ollama",
			Digest:   f.Digest,
			Size:     f.Size,
			FilePath: f.Path,
		})
	}
	return result
}

func (s *Store) listHFModels() []AvailableModel {
	entries, err := os.ReadDir(s.hfCacheDir)
	if err != nil {
		return nil
	}
	var result []AvailableModel
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "models--") {
			continue
		}
		// Reverse the models--org--name to org/name
		repoID := strings.TrimPrefix(e.Name(), "models--")
		repoID = strings.ReplaceAll(repoID, "--", "/")
		f, err := s.FindHFModel(repoID)
		if err != nil {
			continue
		}
		result = append(result, AvailableModel{
			ModelID:  repoID,
			Runtime:  "vllm",
			Digest:   f.Digest,
			Size:     f.Size,
			FilePath: f.Path,
		})
	}
	return result
}
