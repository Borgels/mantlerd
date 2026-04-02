package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Borgels/clawcontrol-agent/internal/types"
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
	if parsed.Scheme != "https" && !insecure {
		return nil, fmt.Errorf("refusing non-https server without --insecure")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}

	return &Client{
		baseURL: strings.TrimRight(serverURL, "/"),
		token:   token,
		httpClient: &http.Client{
			Timeout:   15 * time.Second,
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
