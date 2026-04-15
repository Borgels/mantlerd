package transfer

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

const (
	// TransferPort is the dedicated port for the model transfer server.
	TransferPort = 7433
	// maxConcurrentTransfers limits simultaneous outbound model transfers.
	maxConcurrentTransfers = 2
)

// Server is an HTTP server that serves model files to authenticated peers.
type Server struct {
	machineID  string
	store      *Store
	secretFunc func() string // returns the current HMAC transfer secret
	httpServer *http.Server
	active     atomic.Int32 // current concurrent transfer count
	mu         sync.RWMutex
}

// NewServer creates a transfer server for the given machine.
// secretFunc is called per-request to obtain the current HMAC secret.
func NewServer(machineID string, store *Store, secretFunc func() string) *Server {
	s := &Server{
		machineID:  machineID,
		store:      store,
		secretFunc: secretFunc,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/transfer/manifest", s.handleManifest)
	mux.HandleFunc("/v1/transfer/blob", s.handleBlob)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	s.httpServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // no write timeout — large file transfers take time
		ReadTimeout:       30 * time.Second,
	}
	return s
}

// ListenAndServe starts the server, binding on all private interfaces so that
// peers can reach it regardless of which interface they share with this host.
// Returns when ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	// Bind on all interfaces (0.0.0.0) so the server is reachable on every
	// private network — including the high-speed QSFP/IB fabric — without
	// having to pick one. Clients are given the ranked address list via the
	// check-in payload and will try the fastest one first.
	addr := fmt.Sprintf(":%d", TransferPort)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("transfer server listen %s: %w", addr, err)
	}
	log.Printf("[transfer] listening on %s (machine %s)", ln.Addr(), s.machineID)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(shutdownCtx)
	}()

	if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// handleManifest returns the list of models this machine can serve.
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Require a valid token even for the manifest (prevents enumeration).
	if _, err := s.verifyRequest(r, "", ""); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	available := s.store.ListAvailable()
	entries := make([]types.TransferManifestEntry, 0, len(available))
	for _, m := range available {
		entries = append(entries, types.TransferManifestEntry{
			ModelID:        m.ModelID,
			Runtime:        m.Runtime,
			Digest:         m.Digest,
			ModelSizeBytes: m.Size,
			Ready:          true,
		})
	}
	manifest := types.TransferManifest{
		MachineID: s.machineID,
		Models:    entries,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(manifest)
}

// handleBlob streams the requested model file to the requester.
// Query params: model=<modelID>&runtime=<runtime>
func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	modelID := strings.TrimSpace(r.URL.Query().Get("model"))
	runtime := strings.TrimSpace(r.URL.Query().Get("runtime"))
	if modelID == "" {
		http.Error(w, "missing model param", http.StatusBadRequest)
		return
	}

	claims, err := s.verifyRequest(r, modelID, runtime)
	if err != nil {
		log.Printf("[transfer] auth failed for model %q from %s: %v", modelID, r.RemoteAddr, err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Rate limit concurrent transfers.
	if s.active.Add(1) > maxConcurrentTransfers {
		s.active.Add(-1)
		log.Printf("[transfer] max concurrent transfers reached, rejecting %q for %s", modelID, claims.RequesterMachineID)
		http.Error(w, "too many concurrent transfers", http.StatusServiceUnavailable)
		return
	}
	defer s.active.Add(-1)

	log.Printf("[transfer] serving model %q (runtime=%s) to machine %s", modelID, runtime, claims.RequesterMachineID)

	var modelFile *ModelFile
	switch strings.ToLower(runtime) {
	case "vllm", "tensorrt":
		modelFile, err = s.store.FindHFModel(modelID)
	default:
		modelFile, err = s.store.FindOllamaModel(modelID)
	}
	if err != nil {
		log.Printf("[transfer] locate model %q: %v", modelID, err)
		http.Error(w, "model not found", http.StatusNotFound)
		return
	}

	info, err := os.Stat(modelFile.Path)
	if err != nil {
		http.Error(w, "model file error", http.StatusInternalServerError)
		return
	}

	if info.IsDir() {
		// HF snapshot — stream as tar archive.
		s.streamDirectory(w, r, modelFile.Path, modelID)
		return
	}

	// Single file (Ollama GGUF blob) — stream directly with Range support.
	s.streamFile(w, r, modelFile.Path, modelFile.Digest)
}

// streamFile serves a single model file with HTTP Range support.
func (s *Server) streamFile(w http.ResponseWriter, r *http.Request, path, digest string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "open model file", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, "stat model file", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Transfer-Digest", "sha256:"+digest)
	w.Header().Set("X-Transfer-Machine", s.machineID)
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

// streamDirectory streams a directory as a tar archive.
func (s *Server) streamDirectory(w http.ResponseWriter, r *http.Request, dir, modelID string) {
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("X-Transfer-Machine", s.machineID)
	w.Header().Set("X-Transfer-Format", "tar")

	tw := tar.NewWriter(w)
	defer tw.Close()

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if r.Context().Err() != nil {
			return r.Context().Err()
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}

		hdr := &tar.Header{
			Name:    rel,
			Size:    info.Size(),
			Mode:    int64(info.Mode()),
			ModTime: info.ModTime(),
		}
		if info.IsDir() {
			hdr.Typeflag = tar.TypeDir
			hdr.Name += "/"
			hdr.Size = 0
			return tw.WriteHeader(hdr)
		}

		// Resolve symlinks (HF hub uses symlinks to the actual blobs).
		realPath := path
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return nil
			}
			realPath = resolved
			realInfo, err := os.Stat(realPath)
			if err != nil {
				return nil
			}
			hdr.Size = realInfo.Size()
			hdr.Typeflag = tar.TypeReg
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(realPath)
		if err != nil {
			return nil
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil && r.Context().Err() == nil {
		log.Printf("[transfer] tar stream error for %q: %v", modelID, err)
	}
}

// verifyRequest checks the Authorization token and returns the claims.
// Pass empty modelID/runtime to skip model-specific claim checks (manifest).
func (s *Server) verifyRequest(r *http.Request, modelID, runtime string) (*TokenClaims, error) {
	secret := s.secretFunc()
	if secret == "" {
		return nil, fmt.Errorf("transfer secret not yet received from server")
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == "" {
		return nil, fmt.Errorf("missing authorization header")
	}
	claims, err := VerifyToken(secret, token, s.machineID)
	if err != nil {
		return nil, err
	}
	if modelID != "" && claims.ModelID != modelID {
		return nil, fmt.Errorf("token model %q does not match request model %q", claims.ModelID, modelID)
	}
	return claims, nil
}

// RankedTransferAddresses returns all non-loopback private IPv4 addresses
// (RFC-1918 + link-local 169.254.0.0/16) found on up interfaces, sorted by
// negotiated link speed descending. High-speed fabrics — QSFP, InfiniBand,
// 10/25/100/200 GbE direct cross-connects — are therefore preferred over WiFi
// and slower Ethernet. Link-local addresses are included because direct
// node-to-node fabric connections (e.g. 200G QSFP cross-cables) often use
// 169.254.x.x before a routable range is configured.
//
// The speed is read from /sys/class/net/<iface>/speed on Linux; on other
// platforms it defaults to 0 so the ordering becomes OS-enumeration order.
//
// The list is exported so start.go can include it in the check-in payload,
// letting the mantler server hand peers the best reachable address.
func RankedTransferAddresses() []string {
	type candidate struct {
		ip    string
		speed int64 // Mbps; 0 = unknown
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var candidates []candidate
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		speed := ifaceSpeedMbps(iface.Name)
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || !isPrivateIP(ip) {
				continue
			}
			candidates = append(candidates, candidate{ip: ip.String(), speed: speed})
		}
	}

	// Stable sort: highest speed first.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].speed > candidates[j].speed
	})

	addrs := make([]string, len(candidates))
	for i, c := range candidates {
		addrs[i] = c.ip
	}
	return addrs
}

// ifaceSpeedMbps reads the negotiated link speed for a network interface in
// Mbps. For standard Ethernet (including RoCE and QSFP/DAC cables) it reads
// /sys/class/net/<name>/speed. For InfiniBand and Omni-Path IPoIB interfaces
// (ARPHRD type 32) that return -1 on that path, it falls back to the IB
// subsystem at /sys/class/infiniband/<dev>/ports/<port>/rate.
//
// NVLink is a GPU-to-GPU memory bus (not an IP-capable network interface)
// and therefore has no entry in /sys/class/net; it is irrelevant here.
func ifaceSpeedMbps(name string) int64 {
	base := filepath.Join("/sys/class/net", name)

	data, err := os.ReadFile(filepath.Join(base, "speed"))
	if err == nil {
		s := strings.TrimSpace(string(data))
		speed, err := strconv.ParseInt(s, 10, 64)
		if err == nil && speed > 0 {
			return speed
		}
		// -1 means the driver doesn't report speed via this path.
		// Fall through to the IB subsystem check below.
	}

	// Check if this is an InfiniBand / Omni-Path IPoIB interface:
	// ARPHRD_INFINIBAND = 32 (from linux/if_arp.h).
	typeData, err := os.ReadFile(filepath.Join(base, "type"))
	if err != nil {
		return 0
	}
	ifType := strings.TrimSpace(string(typeData))
	if ifType != "32" {
		return 0
	}
	return ibIfaceSpeedMbps(name)
}

// ibIfaceSpeedMbps resolves the link speed for an IPoIB / Omni-Path interface
// by navigating to the parent InfiniBand device via
// /sys/class/net/<iface>/device/infiniband/<dev>/ports/<port>/rate.
// The rate file contains strings like "200 Gb/s (4X HDR)" or "400 Gb/s".
func ibIfaceSpeedMbps(ifaceName string) int64 {
	ibDir := filepath.Join("/sys/class/net", ifaceName, "device", "infiniband")
	entries, err := os.ReadDir(ibDir)
	if err != nil || len(entries) == 0 {
		return 0
	}
	// There is typically exactly one IB device per interface.
	ibDev := entries[0].Name()
	portsDir := filepath.Join("/sys/class/infiniband", ibDev, "ports")
	ports, err := os.ReadDir(portsDir)
	if err != nil {
		return 0
	}
	var maxSpeed int64
	for _, port := range ports {
		rateFile := filepath.Join(portsDir, port.Name(), "rate")
		raw, err := os.ReadFile(rateFile)
		if err != nil {
			continue
		}
		if s := parseIBRateMbps(string(raw)); s > maxSpeed {
			maxSpeed = s
		}
	}
	return maxSpeed
}

// parseIBRateMbps parses InfiniBand port rate strings into Mbps.
//
// Known formats emitted by the kernel / vendor drivers:
//   "200 Gb/s (4X HDR)"   — HDR InfiniBand,  200 Gbps per port
//   "400 Gb/s (4X NDR)"   — NDR InfiniBand,  400 Gbps per port
//   "100 Gb/s (4X EDR)"   — EDR InfiniBand,  100 Gbps per port
//   "56 Gb/s (4X FDR)"    — FDR InfiniBand,   56 Gbps per port
//   "40 Gb/s (4X QDR)"    — QDR InfiniBand,   40 Gbps per port
//   "20 Gb/s (4X DDR)"    — DDR InfiniBand,   20 Gbps per port
//   "10 Gb/s (4X SDR)"    — SDR InfiniBand,   10 Gbps per port
//   "2.5 Gb/s (1X SDR)"   — narrow SDR link, 2.5 Gbps
//   "0 GB/sec"             — Mellanox: unpopulated/down port, treat as 0
//   "200 Gb/s"             — no parenthetical (some driver versions)
//
// We parse the leading decimal number and the Gb/s or GB/sec unit,
// converting to Mbps (×1000). GB/sec (bytes) ≈ ×8000 Mbps — but the
// Mellanox "0 GB/sec" case is always a down/zero port so the distinction
// doesn't matter in practice; we still multiply by 8000 for correctness.
func parseIBRateMbps(s string) int64 {
	s = strings.TrimSpace(s)
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return 0
	}
	val, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || val <= 0 {
		return 0
	}
	unit := strings.ToLower(fields[1])
	switch {
	case unit == "gb/s" || unit == "gb/s,":
		// "200 Gb/s" — Gbps, convert to Mbps
		return int64(val * 1000)
	case unit == "gb/sec":
		// Mellanox "0 GB/sec" (bytes/sec). ×8 to convert bytes→bits, then ×1000 for Gbps→Mbps.
		return int64(val * 8 * 1000)
	default:
		return 0
	}
}

func isPrivateIP(ip net.IP) bool {
	return ip[0] == 10 ||
		(ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) ||
		(ip[0] == 192 && ip[1] == 168) ||
		// Link-local (169.254.0.0/16) — used by direct QSFP/InfiniBand
		// cross-connects that haven't been assigned routable addresses.
		(ip[0] == 169 && ip[1] == 254)
}
