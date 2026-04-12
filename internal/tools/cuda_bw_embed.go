package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const embeddedCudaBandwidthSource = `#include <cuda_runtime.h>
#include <stdio.h>

int main() {
    const size_t SIZE = 256 * 1024 * 1024;
    char *d_src = NULL;
    char *d_dst = NULL;
    cudaEvent_t start, stop;

    if (cudaMalloc((void**)&d_src, SIZE) != cudaSuccess) return 1;
    if (cudaMalloc((void**)&d_dst, SIZE) != cudaSuccess) return 1;
    if (cudaMemset(d_src, 1, SIZE) != cudaSuccess) return 1;

    if (cudaEventCreate(&start) != cudaSuccess) return 1;
    if (cudaEventCreate(&stop) != cudaSuccess) return 1;
    if (cudaMemcpy(d_dst, d_src, SIZE, cudaMemcpyDeviceToDevice) != cudaSuccess) return 1;

    const int ITERS = 20;
    if (cudaEventRecord(start) != cudaSuccess) return 1;
    for (int i = 0; i < ITERS; i++) {
        if (cudaMemcpy(d_dst, d_src, SIZE, cudaMemcpyDeviceToDevice) != cudaSuccess) return 1;
    }
    if (cudaEventRecord(stop) != cudaSuccess) return 1;
    if (cudaEventSynchronize(stop) != cudaSuccess) return 1;

    float ms = 0.0f;
    if (cudaEventElapsedTime(&ms, start, stop) != cudaSuccess) return 1;
    double gbps = ((double)SIZE * ITERS / (1024.0 * 1024.0 * 1024.0)) / (ms / 1000.0);
    printf("BANDWIDTH_GBPS=%.2f\n", gbps);

    cudaFree(d_src);
    cudaFree(d_dst);
    cudaEventDestroy(start);
    cudaEventDestroy(stop);
    return 0;
}
`

var (
	embeddedCudaBandwidthRegex = regexp.MustCompile(`BANDWIDTH_GBPS=([0-9]+(?:\.[0-9]+)?)`)
	cudaBenchmarkBuildMu       sync.Mutex
)

func measureWithEmbeddedCudaBenchmark() float64 {
	nvccBinary := resolveNvccBinary()
	if nvccBinary == "" {
		return 0
	}

	binaryPath, err := ensureEmbeddedCudaBenchmarkBinary(nvccBinary)
	if err != nil {
		return 0
	}

	output, err := commandOutput(2*time.Minute, binaryPath)
	if err != nil {
		return 0
	}

	return parseEmbeddedCudaBandwidth(output)
}

func ensureEmbeddedCudaBenchmarkBinary(nvccBinary string) (string, error) {
	cudaBenchmarkBuildMu.Lock()
	defer cudaBenchmarkBuildMu.Unlock()

	cacheRoot, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(cacheRoot) == "" {
		cacheRoot = os.TempDir()
	}

	targetDir := filepath.Join(cacheRoot, "mantler")
	binaryPath := filepath.Join(targetDir, "cuda-bw-test")
	stampPath := binaryPath + ".sha256"
	expectedFingerprint := embeddedCudaBenchmarkFingerprint()

	if binaryLooksUsable(binaryPath) {
		stamp, err := os.ReadFile(stampPath)
		if err == nil && strings.TrimSpace(string(stamp)) == expectedFingerprint {
			return binaryPath, nil
		}
	}

	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", fmt.Errorf("create benchmark cache dir: %w", err)
	}

	tempDir, err := os.MkdirTemp("", "mantler-cuda-bw-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	sourcePath := filepath.Join(tempDir, "cuda_bw_test.cu")
	tempBinaryPath := filepath.Join(tempDir, "cuda-bw-test")

	if err := os.WriteFile(sourcePath, []byte(embeddedCudaBandwidthSource), 0o644); err != nil {
		return "", fmt.Errorf("write cuda source: %w", err)
	}

	_, err = commandOutput(90*time.Second, nvccBinary, "-O2", sourcePath, "-o", tempBinaryPath)
	if err != nil {
		return "", fmt.Errorf("compile cuda benchmark: %w", err)
	}

	if err := os.Chmod(tempBinaryPath, 0o755); err != nil {
		return "", fmt.Errorf("mark benchmark executable: %w", err)
	}

	if err := os.Rename(tempBinaryPath, binaryPath); err != nil {
		return "", fmt.Errorf("move benchmark binary into cache: %w", err)
	}

	if err := os.WriteFile(stampPath, []byte(expectedFingerprint), 0o644); err != nil {
		return "", fmt.Errorf("write benchmark fingerprint: %w", err)
	}

	return binaryPath, nil
}

func resolveNvccBinary() string {
	if commandExists("nvcc") {
		return "nvcc"
	}
	candidates := []string{
		"/usr/local/cuda/bin/nvcc",
		"/opt/cuda/bin/nvcc",
	}
	for _, candidate := range candidates {
		if binaryLooksUsable(candidate) {
			return candidate
		}
	}
	return ""
}

func binaryLooksUsable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}

func embeddedCudaBenchmarkFingerprint() string {
	sum := sha256.Sum256([]byte(embeddedCudaBandwidthSource))
	return hex.EncodeToString(sum[:])
}

func parseEmbeddedCudaBandwidth(output string) float64 {
	match := embeddedCudaBandwidthRegex.FindStringSubmatch(output)
	if len(match) == 2 {
		value, err := strconv.ParseFloat(match[1], 64)
		if err == nil && value > 0 && value <= maxReasonableBandwidthGBps {
			return value
		}
	}
	return parseMaxBandwidthFromText(output)
}
