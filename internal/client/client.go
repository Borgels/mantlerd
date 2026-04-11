package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	agenteval "github.com/Borgels/mantlerd/internal/eval"
	"github.com/Borgels/mantlerd/internal/types"
)

type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func New(serverURL, token string, insecure bool) (*Client, error) {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("invalid server url: %w", err)
	}
	switch parsed.Scheme {
	case "https":
		// secure by default
	case "http":
		if !insecure {
			// Hardened fallback for local/private deployments: avoid crash-looping
			// when installer/config missed insecure mode.
			fmt.Fprintf(os.Stderr, "warning: non-HTTPS server detected; enabling insecure mode automatically\n")
			insecure = true
		}
	default:
		return nil, fmt.Errorf("unsupported server url scheme: %s", parsed.Scheme)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}

	return &Client{
		baseURL: strings.TrimRight(serverURL, "/"),
		token:   token,
		httpClient: &http.Client{
			Transport: transport,
		},
	}, nil
}

func (c *Client) Checkin(ctx context.Context, payload types.CheckinRequest) (types.CheckinResponse, error) {
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return types.CheckinResponse{}, fmt.Errorf("marshal checkin payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/agent/checkin", bytes.NewReader(reqBody))
	if err != nil {
		return types.CheckinResponse{}, fmt.Errorf("create checkin request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return types.CheckinResponse{}, fmt.Errorf("checkin request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return types.CheckinResponse{}, fmt.Errorf("checkin failed (%d): %s", resp.StatusCode, string(body))
	}

	var envelope struct {
		Data types.CheckinResponse `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return types.CheckinResponse{}, fmt.Errorf("decode checkin response: %w", err)
	}
	return envelope.Data, nil
}

func (c *Client) Ack(ctx context.Context, payload types.AckRequest) error {
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal ack payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/agent/ack", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create ack request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ack request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ack failed (%d): %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *Client) Recommend(ctx context.Context, q types.RecommendQuery) (*types.RecommendResponse, error) {
	params := url.Values{}
	if strings.TrimSpace(q.MachineID) != "" {
		params.Set("machineId", strings.TrimSpace(q.MachineID))
	}
	if strings.TrimSpace(q.HardwareClass) != "" {
		params.Set("hardwareClass", strings.TrimSpace(q.HardwareClass))
	}
	if strings.TrimSpace(q.Runtime) != "" {
		params.Set("runtime", strings.TrimSpace(q.Runtime))
	}
	if strings.TrimSpace(q.ModelID) != "" {
		params.Set("modelId", strings.TrimSpace(q.ModelID))
	}
	if strings.TrimSpace(q.Backend) != "" {
		params.Set("backend", strings.TrimSpace(q.Backend))
	}
	if strings.TrimSpace(q.Harness) != "" {
		params.Set("harness", strings.TrimSpace(q.Harness))
	}
	if strings.TrimSpace(q.Orchestrator) != "" {
		params.Set("orchestrator", strings.TrimSpace(q.Orchestrator))
	}
	if strings.TrimSpace(q.Role) != "" {
		params.Set("role", strings.TrimSpace(q.Role))
	}
	if strings.TrimSpace(q.Workload) != "" {
		params.Set("workload", strings.TrimSpace(q.Workload))
	}
	if q.Limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", q.Limit))
	}
	urlWithQuery := c.baseURL + "/api/recommendations"
	if encoded := params.Encode(); encoded != "" {
		urlWithQuery += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlWithQuery, nil)
	if err != nil {
		return nil, fmt.Errorf("create recommendations request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("recommendations request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("recommendations failed (%d): %s", resp.StatusCode, string(body))
	}
	var envelope struct {
		Data types.RecommendResponse `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode recommendations response: %w", err)
	}
	return &envelope.Data, nil
}

func (c *Client) Explore(ctx context.Context, q types.ExploreQuery) (*types.ExploreResponse, error) {
	reqBody, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("marshal explore payload: %w", err)
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.baseURL+"/api/agent/explore",
		bytes.NewReader(reqBody),
	)
	if err != nil {
		return nil, fmt.Errorf("create explore request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("explore request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusNotFound && strings.Contains(strings.ToLower(string(body)), "<!doctype html>") {
			return nil, fmt.Errorf(
				"explore failed (404): /api/agent/explore is not available on %s; deploy backend routes first",
				c.baseURL,
			)
		}
		return nil, fmt.Errorf("explore failed (%d): %s", resp.StatusCode, compactHTTPErrorBody(body))
	}

	var envelope struct {
		Data types.ExploreResponse `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode explore response: %w", err)
	}
	return &envelope.Data, nil
}

func compactHTTPErrorBody(body []byte) string {
	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return "<empty response body>"
	}
	if strings.Contains(strings.ToLower(raw), "<html") {
		return "HTML error page returned"
	}
	unescaped := html.UnescapeString(raw)
	if len(unescaped) > 500 {
		return strings.TrimSpace(unescaped[:500]) + "..."
	}
	return unescaped
}

func (c *Client) GetScore(ctx context.Context, fingerprint string) (*types.ScoreResponse, error) {
	target := strings.TrimSpace(fingerprint)
	if target == "" {
		return nil, fmt.Errorf("fingerprint is required")
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		c.baseURL+"/api/agent/score/"+url.PathEscape(target),
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("create score request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("score request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("score request failed (%d): %s", resp.StatusCode, string(body))
	}

	var envelope struct {
		Data struct {
			Score *types.ScoreResponse `json:"score"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode score response: %w", err)
	}
	if envelope.Data.Score == nil {
		return nil, fmt.Errorf("score not found for fingerprint %s", target)
	}
	return envelope.Data.Score, nil
}

func (c *Client) GetEvalPrompts(
	ctx context.Context,
	workload string,
	profile string,
	benchmarkSuiteID string,
) ([]agenteval.Prompt, string, error) {
	params := url.Values{}
	if strings.TrimSpace(workload) != "" {
		params.Set("workload", strings.TrimSpace(workload))
	}
	if strings.TrimSpace(profile) != "" {
		params.Set("profile", strings.TrimSpace(profile))
	}
	if strings.TrimSpace(benchmarkSuiteID) != "" {
		params.Set("benchmarkSuiteId", strings.TrimSpace(benchmarkSuiteID))
	}
	urlWithQuery := c.baseURL + "/api/eval/prompts"
	if encoded := params.Encode(); encoded != "" {
		urlWithQuery += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlWithQuery, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create eval prompts request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("eval prompts request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("eval prompts failed (%d): %s", resp.StatusCode, string(body))
	}
	var envelope struct {
		Data struct {
			Prompts          []agenteval.Prompt `json:"prompts"`
			EvalSessionToken string             `json:"evalSessionToken"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, "", fmt.Errorf("decode eval prompts response: %w", err)
	}
	if len(envelope.Data.Prompts) == 0 {
		return nil, "", fmt.Errorf("eval prompts response was empty")
	}
	return envelope.Data.Prompts, strings.TrimSpace(envelope.Data.EvalSessionToken), nil
}

func Retry[T any](ctx context.Context, attempts int, fn func() (T, error)) (T, error) {
	var zero T
	var err error
	backoff := time.Second
	for i := 0; i < attempts; i++ {
		var result T
		result, err = fn()
		if err == nil {
			return result, nil
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
	return zero, err
}
