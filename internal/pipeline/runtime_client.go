package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Borgels/mantlerd/internal/runtimeport"
)

const (
	defaultLocalRuntime = "ollama"
	maxResponseBodySize = 1 << 20
)

func callLocalChatCompletion(ctx context.Context, runtimeName string, payload map[string]any) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("runtime_request_invalid")
	}
	defer zeroBytes(body)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		localRuntimeURL(runtimeName)+"/v1/chat/completions",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, errors.New("runtime_request_invalid")
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")

	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("runtime_unavailable: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return nil, errors.New("runtime_read_failed")
	}
	defer zeroBytes(raw)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("runtime_completion_failed: %d", resp.StatusCode)
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, errors.New("runtime_response_invalid")
	}
	return parsed, nil
}

func localRuntimeURL(runtimeName string) string {
	return fmt.Sprintf("http://127.0.0.1:%d", runtimeport.Resolve(runtimeName))
}

func stringFromAny(value any) string {
	typed, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(typed)
}

