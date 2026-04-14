package relay

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Borgels/mantlerd/internal/pipeline"
	"github.com/Borgels/mantlerd/internal/runtimeport"
	"github.com/Borgels/mantlerd/internal/types"
	"nhooyr.io/websocket"
)

const (
	defaultRelayPath       = "/api/agent/relay/ws"
	defaultRequestTimeout  = 45 * time.Second
	relayPingInterval      = 30 * time.Second
	relayReconnectCooldown = 2 * time.Second
)

type RuntimeInventory interface {
	ReadyRuntimes() []string
	InstalledRuntimes() []string
}

type Config struct {
	ServerURL string
	RelayURL  string
	Token     string
	MachineID string
	Insecure  bool
}

type Client struct {
	cfg              Config
	relayURL         string
	runtimes         RuntimeInventory
	pipelineHandler  *pipeline.Handler
	connected        atomic.Bool
	lastRelayLatency atomic.Int64
	writeMu          sync.Mutex
}

type envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type helloPayload struct {
	MachineID string `json:"machineId"`
	AgentID   string `json:"agentId,omitempty"`
	Version   string `json:"version,omitempty"`
}

type pingPayload struct {
	SentAt string `json:"sentAt"`
}

type proxyRequest struct {
	RequestID  string            `json:"requestId"`
	Method     string            `json:"method,omitempty"`
	Path       string            `json:"path,omitempty"`
	Runtime    string            `json:"runtime,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	BodyBase64 string            `json:"bodyBase64,omitempty"`
	TimeoutMs  int               `json:"timeoutMs,omitempty"`
}

type pipelineStageRequest struct {
	RequestID  string `json:"requestId"`
	TimeoutMs  int    `json:"timeoutMs,omitempty"`
	BodyBase64 string `json:"bodyBase64,omitempty"`
}

type proxyResponseStart struct {
	RequestID string            `json:"requestId"`
	Status    int               `json:"status"`
	Headers   map[string]string `json:"headers,omitempty"`
}

type proxyResponseChunk struct {
	RequestID   string `json:"requestId"`
	ChunkBase64 string `json:"chunkBase64"`
}

type proxyResponseEnd struct {
	RequestID string `json:"requestId"`
}

type proxyResponseError struct {
	RequestID string `json:"requestId"`
	Message   string `json:"message"`
}

func New(cfg Config, runtimes RuntimeInventory) (*Client, error) {
	relayURL, err := resolveRelayURL(cfg.ServerURL, cfg.RelayURL)
	if err != nil {
		return nil, err
	}
	pipelineHandler, err := pipeline.NewHandler()
	if err != nil {
		return nil, fmt.Errorf("init pipeline handler: %w", err)
	}
	return &Client{
		cfg:             cfg,
		relayURL:        relayURL,
		runtimes:        runtimes,
		pipelineHandler: pipelineHandler,
	}, nil
}

func (c *Client) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := c.runOnce(ctx); err != nil && ctx.Err() == nil {
			time.Sleep(relayReconnectCooldown)
		}
	}
}

func (c *Client) Connected() bool {
	return c.connected.Load()
}

func (c *Client) RelayLatencyMs() int64 {
	value := c.lastRelayLatency.Load()
	if value <= 0 {
		return 0
	}
	return value
}

func (c *Client) runOnce(ctx context.Context) error {
	conn, err := c.connect(ctx)
	if err != nil {
		c.connected.Store(false)
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "relay reconnect")
	c.connected.Store(true)

	if err := c.send(conn, envelope{
		Type: "relay_hello",
		Payload: mustJSON(helloPayload{
			MachineID: c.cfg.MachineID,
		}),
	}); err != nil {
		c.connected.Store(false)
		return err
	}

	pingTicker := time.NewTicker(relayPingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.connected.Store(false)
			return ctx.Err()
		case <-pingTicker.C:
			sentAt := time.Now().UTC().Format(time.RFC3339Nano)
			if err := c.send(conn, envelope{
				Type: "relay_ping",
				Payload: mustJSON(pingPayload{
					SentAt: sentAt,
				}),
			}); err != nil {
				c.connected.Store(false)
				return err
			}
		default:
			readCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
			err := c.readAndHandle(readCtx, conn)
			cancel()
			if err != nil {
				if readCtx.Err() == context.DeadlineExceeded {
					continue
				}
				c.connected.Store(false)
				return err
			}
		}
	}
}

func (c *Client) connect(ctx context.Context) (*websocket.Conn, error) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+strings.TrimSpace(c.cfg.Token))
	headers.Set("X-Machine-ID", strings.TrimSpace(c.cfg.MachineID))
	options := &websocket.DialOptions{
		HTTPHeader: headers,
	}
	if c.cfg.Insecure {
		options.HTTPClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
			Timeout: 30 * time.Second,
		}
	}
	conn, _, err := websocket.Dial(ctx, c.relayURL, options)
	if err != nil {
		return nil, fmt.Errorf("dial relay websocket: %w", err)
	}
	return conn, nil
}

func (c *Client) readAndHandle(ctx context.Context, conn *websocket.Conn) error {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return err
	}

	var msg envelope
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil
	}
	switch msg.Type {
	case "relay_ping":
		var ping pingPayload
		_ = json.Unmarshal(msg.Payload, &ping)
		return c.send(conn, envelope{Type: "relay_pong", Payload: mustJSON(ping)})
	case "relay_pong":
		var pong pingPayload
		if err := json.Unmarshal(msg.Payload, &pong); err != nil {
			return nil
		}
		if sentAt, err := time.Parse(time.RFC3339Nano, pong.SentAt); err == nil {
			c.lastRelayLatency.Store(max(0, time.Since(sentAt).Milliseconds()))
		}
		return nil
	case "proxy_request":
		var req proxyRequest
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			return nil
		}
		go c.handleProxyRequest(context.Background(), conn, req)
		return nil
	case "pipeline_stage_request":
		var req pipelineStageRequest
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			return nil
		}
		go c.handlePipelineStageRequest(context.Background(), conn, req)
		return nil
	default:
		return nil
	}
}

func (c *Client) handlePipelineStageRequest(ctx context.Context, conn *websocket.Conn, req pipelineStageRequest) {
	if strings.TrimSpace(req.RequestID) == "" {
		return
	}
	if c.pipelineHandler == nil {
		_ = c.sendProxyError(conn, req.RequestID, fmt.Errorf("pipeline handler unavailable"))
		return
	}
	timeout := 120 * time.Second
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	decoded, err := base64.StdEncoding.DecodeString(req.BodyBase64)
	if err != nil {
		_ = c.sendProxyError(conn, req.RequestID, fmt.Errorf("decode request body: %w", err))
		return
	}
	var stageEnvelope types.StageEnvelope
	if err := json.Unmarshal(decoded, &stageEnvelope); err != nil {
		_ = c.sendProxyError(conn, req.RequestID, fmt.Errorf("invalid stage envelope: %w", err))
		return
	}
	responsePayload, err := c.pipelineHandler.HandleStageRequest(requestCtx, stageEnvelope)
	if err != nil {
		_ = c.sendProxyError(conn, req.RequestID, err)
		return
	}
	rawResponse, err := json.Marshal(responsePayload)
	if err != nil {
		_ = c.sendProxyError(conn, req.RequestID, fmt.Errorf("marshal stage response: %w", err))
		return
	}
	if err := c.send(conn, envelope{
		Type: "proxy_response_start",
		Payload: mustJSON(proxyResponseStart{
			RequestID: req.RequestID,
			Status:    http.StatusOK,
			Headers: map[string]string{
				"content-type": "application/json",
			},
		}),
	}); err != nil {
		return
	}
	if len(rawResponse) > 0 {
		if err := c.send(conn, envelope{
			Type: "proxy_response_chunk",
			Payload: mustJSON(proxyResponseChunk{
				RequestID:   req.RequestID,
				ChunkBase64: base64.StdEncoding.EncodeToString(rawResponse),
			}),
		}); err != nil {
			return
		}
	}
	_ = c.send(conn, envelope{
		Type: "proxy_response_end",
		Payload: mustJSON(proxyResponseEnd{
			RequestID: req.RequestID,
		}),
	})
}

func (c *Client) handleProxyRequest(ctx context.Context, conn *websocket.Conn, req proxyRequest) {
	if strings.TrimSpace(req.RequestID) == "" {
		return
	}

	timeout := defaultRequestTimeout
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	localURL, err := c.localURLForRequest(req)
	if err != nil {
		_ = c.sendProxyError(conn, req.RequestID, err)
		return
	}
	method := strings.TrimSpace(req.Method)
	if method == "" {
		method = http.MethodPost
	}
	var body io.Reader
	if strings.TrimSpace(req.BodyBase64) != "" {
		decoded, decodeErr := base64.StdEncoding.DecodeString(req.BodyBase64)
		if decodeErr != nil {
			_ = c.sendProxyError(conn, req.RequestID, fmt.Errorf("decode request body: %w", decodeErr))
			return
		}
		body = strings.NewReader(string(decoded))
	}

	upstreamReq, err := http.NewRequestWithContext(requestCtx, method, localURL, body)
	if err != nil {
		_ = c.sendProxyError(conn, req.RequestID, err)
		return
	}
	for key, value := range req.Headers {
		lowerKey := strings.ToLower(strings.TrimSpace(key))
		if lowerKey == "host" || lowerKey == "content-length" || lowerKey == "connection" {
			continue
		}
		upstreamReq.Header.Set(key, value)
	}

	httpClient := &http.Client{}
	upstreamResp, err := httpClient.Do(upstreamReq)
	if err != nil {
		_ = c.sendProxyError(conn, req.RequestID, err)
		return
	}
	defer upstreamResp.Body.Close()

	respHeaders := map[string]string{}
	for key, values := range upstreamResp.Header {
		if len(values) == 0 {
			continue
		}
		respHeaders[key] = strings.Join(values, ", ")
	}

	if err := c.send(conn, envelope{
		Type: "proxy_response_start",
		Payload: mustJSON(proxyResponseStart{
			RequestID: req.RequestID,
			Status:    upstreamResp.StatusCode,
			Headers:   respHeaders,
		}),
	}); err != nil {
		return
	}

	buffer := make([]byte, 32*1024)
	for {
		readCount, readErr := upstreamResp.Body.Read(buffer)
		if readCount > 0 {
			chunk := base64.StdEncoding.EncodeToString(buffer[:readCount])
			if err := c.send(conn, envelope{
				Type: "proxy_response_chunk",
				Payload: mustJSON(proxyResponseChunk{
					RequestID:   req.RequestID,
					ChunkBase64: chunk,
				}),
			}); err != nil {
				return
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			_ = c.sendProxyError(conn, req.RequestID, readErr)
			return
		}
	}

	_ = c.send(conn, envelope{
		Type: "proxy_response_end",
		Payload: mustJSON(proxyResponseEnd{
			RequestID: req.RequestID,
		}),
	})
}

func (c *Client) localURLForRequest(req proxyRequest) (string, error) {
	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = "/v1/chat/completions"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	runtimeName := strings.ToLower(strings.TrimSpace(req.Runtime))
	if runtimeName == "" {
		ready := c.runtimes.ReadyRuntimes()
		if len(ready) > 0 {
			runtimeName = strings.ToLower(strings.TrimSpace(ready[0]))
		}
	}
	if runtimeName == "" {
		installed := c.runtimes.InstalledRuntimes()
		if len(installed) > 0 {
			runtimeName = strings.ToLower(strings.TrimSpace(installed[0]))
		}
	}
	if runtimeName == "" {
		runtimeName = "ollama"
	}

	port := runtimeport.Resolve(runtimeName)
	targetURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	return targetURL, nil
}

func resolveRelayURL(serverURL string, explicitRelayURL string) (string, error) {
	trimmedRelay := strings.TrimSpace(explicitRelayURL)
	if trimmedRelay != "" {
		parsedRelay, err := url.Parse(trimmedRelay)
		if err != nil {
			return "", fmt.Errorf("invalid relay url: %w", err)
		}
		if parsedRelay.Scheme != "ws" && parsedRelay.Scheme != "wss" {
			return "", fmt.Errorf("relay url must use ws:// or wss://")
		}
		return parsedRelay.String(), nil
	}

	parsedServer, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil {
		return "", fmt.Errorf("invalid server url: %w", err)
	}
	switch parsedServer.Scheme {
	case "https":
		parsedServer.Scheme = "wss"
	case "http":
		parsedServer.Scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported server url scheme: %s", parsedServer.Scheme)
	}
	parsedServer.Path = defaultRelayPath
	parsedServer.RawQuery = ""
	parsedServer.Fragment = ""
	return parsedServer.String(), nil
}

func (c *Client) sendProxyError(conn *websocket.Conn, requestID string, err error) error {
	return c.send(conn, envelope{
		Type: "proxy_response_error",
		Payload: mustJSON(proxyResponseError{
			RequestID: requestID,
			Message:   err.Error(),
		}),
	})
}

func (c *Client) send(conn *websocket.Conn, msg envelope) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	writeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, payload)
}

func mustJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
