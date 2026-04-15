package transfer

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

// PullResult is an alias for types.TransferResult.
type PullResult = types.TransferResult

// Client pulls model files from peer transfer servers.
type Client struct {
	machineID  string
	store      *Store
	secretFunc func() string
	httpClient *http.Client
}

// NewClient creates a transfer client.
func NewClient(machineID string, store *Store, secretFunc func() string) *Client {
	return &Client{
		machineID:  machineID,
		store:      store,
		secretFunc: secretFunc,
		httpClient: &http.Client{
			Timeout: 0, // no overall timeout — transfers can take a long time
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				ResponseHeaderTimeout: 15 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
			},
		},
	}
}

// PullFromPeers attempts to pull a model from the provided peer hints in order.
// Returns nil if no peer could serve the model (caller should fall back to upstream).
func (c *Client) PullFromPeers(ctx context.Context, modelID, runtime string, peers []types.PeerHint, progress func(received, total int64)) (*PullResult, error) {
	if len(peers) == 0 {
		return nil, nil
	}
	secret := c.secretFunc()
	if secret == "" {
		return nil, nil
	}
	for _, peer := range peers {
		if peer.MachineID == c.machineID {
			continue // skip self
		}
		result, err := c.pullFromPeer(ctx, peer, modelID, runtime, secret, progress)
		if err != nil {
			log.Printf("[transfer] peer %s failed for %q: %v — trying next", peer.MachineID, modelID, err)
			continue
		}
		// Verify digest matches what the server told us.
		if peer.Digest != "" && result.Digest != strings.TrimPrefix(peer.Digest, "sha256:") {
			log.Printf("[transfer] peer %s digest mismatch for %q (got %s, want %s) — trying next",
				peer.MachineID, modelID, result.Digest, peer.Digest)
			// Clean up the partial download.
			continue
		}
		return result, nil
	}
	return nil, nil
}

func (c *Client) pullFromPeer(ctx context.Context, peer types.PeerHint, modelID, runtime, secret string, progress func(received, total int64)) (*PullResult, error) {
	// Try each reported address until one succeeds.
	for _, addr := range peer.Addresses {
		result, err := c.tryAddress(ctx, addr, peer.TransferPort, peer.MachineID, modelID, runtime, secret, progress)
		if err == nil {
			return result, nil
		}
		log.Printf("[transfer] address %s:%d for peer %s: %v", addr, peer.TransferPort, peer.MachineID, err)
	}
	return nil, fmt.Errorf("all addresses failed for peer %s", peer.MachineID)
}

func (c *Client) tryAddress(ctx context.Context, addr string, port int, peerMachineID, modelID, runtime, secret string, progress func(received, total int64)) (*PullResult, error) {
	if port <= 0 {
		port = TransferPort
	}
	token, err := CreateToken(secret, c.machineID, peerMachineID, modelID, runtime)
	if err != nil {
		return nil, fmt.Errorf("create token: %w", err)
	}

	baseURL := fmt.Sprintf("http://%s:%d", addr, port)
	u, err := url.Parse(baseURL + "/v1/transfer/blob")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("model", modelID)
	q.Set("runtime", runtime)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("peer returned HTTP %d", resp.StatusCode)
	}

	sourceMachine := resp.Header.Get("X-Transfer-Machine")
	format := strings.ToLower(strings.TrimSpace(resp.Header.Get("X-Transfer-Format")))

	if format == "tar" {
		return c.receiveTar(ctx, resp.Body, modelID, runtime, sourceMachine, progress)
	}
	return c.receiveBlob(ctx, resp.Body, modelID, runtime, sourceMachine, resp.ContentLength, progress)
}

// receiveBlob saves a single GGUF blob to the Ollama blob store.
func (c *Client) receiveBlob(ctx context.Context, body io.Reader, modelID, runtime, sourceMachine string, totalBytes int64, progress func(received, total int64)) (*PullResult, error) {
	blobsDir := filepath.Join(c.store.ollamaModelsDir, "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create blobs dir: %w", err)
	}

	tmpFile, err := os.CreateTemp(blobsDir, ".mantler-transfer-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		if _, err := os.Stat(tmpPath); err == nil {
			os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	reader := io.TeeReader(body, h)
	received, err := copyWithProgress(ctx, tmpFile, reader, totalBytes, progress)
	tmpFile.Close()
	if err != nil {
		return nil, fmt.Errorf("receive blob: %w", err)
	}

	digest := hex.EncodeToString(h.Sum(nil))
	finalPath := filepath.Join(blobsDir, "sha256-"+digest)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return nil, fmt.Errorf("finalize blob: %w", err)
	}

	log.Printf("[transfer] saved blob %s (%.1f MB) from %s", digest[:12], float64(received)/1e6, sourceMachine)
	return &PullResult{Digest: digest, Bytes: received, SourceMachineID: sourceMachine}, nil
}

// receiveTar extracts a tar archive into the HF cache directory.
func (c *Client) receiveTar(ctx context.Context, body io.Reader, modelID, runtime, sourceMachine string, progress func(received, total int64)) (*PullResult, error) {
	dirName := "models--" + strings.ReplaceAll(modelID, "/", "--")
	destDir := filepath.Join(c.store.hfCacheDir, dirName, "snapshots", "peer-transfer")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("create dest dir: %w", err)
	}

	h := sha256.New()
	tr := tar.NewReader(io.TeeReader(body, h))
	var totalBytes int64

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar read: %w", err)
		}
		cleanName := filepath.Clean(hdr.Name)
		if strings.HasPrefix(cleanName, "..") {
			continue // path traversal guard
		}
		target := filepath.Join(destDir, cleanName)
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		f, err := os.Create(target)
		if err != nil {
			return nil, err
		}
		n, err := io.Copy(f, tr)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("extract %s: %w", hdr.Name, err)
		}
		totalBytes += n
		if progress != nil {
			progress(totalBytes, 0)
		}
	}

	digest := hex.EncodeToString(h.Sum(nil))
	log.Printf("[transfer] extracted HF snapshot %s (%.1f MB) from %s", digest[:12], float64(totalBytes)/1e6, sourceMachine)
	return &PullResult{Digest: digest, Bytes: totalBytes, SourceMachineID: sourceMachine}, nil
}

func copyWithProgress(ctx context.Context, dst io.Writer, src io.Reader, totalBytes int64, progress func(received, total int64)) (int64, error) {
	buf := make([]byte, 256*1024) // 256 KB chunks
	var received int64
	for {
		if ctx.Err() != nil {
			return received, ctx.Err()
		}
		n, err := src.Read(buf)
		if n > 0 {
			written, werr := dst.Write(buf[:n])
			received += int64(written)
			if progress != nil {
				progress(received, totalBytes)
			}
			if werr != nil {
				return received, werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return received, err
		}
	}
	return received, nil
}
