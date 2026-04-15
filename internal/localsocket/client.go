package localsocket

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// dialTransport returns an HTTP transport that connects over the Unix socket.
func dialTransport() *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, "unix", SocketPath())
		},
	}
}

func newClient() *http.Client {
	return &http.Client{Transport: dialTransport(), Timeout: 0}
}

// baseURL is the base URL used for HTTP requests over the socket.
// The host part is ignored by the custom dialer.
const baseURL = "http://mantlerd"

// PullModel sends a pull request to the daemon and streams progress events to
// onProgress.  Returns nil on success or an error if the daemon reports failure.
func PullModel(ctx context.Context, modelID, runtime string, onProgress func(ProgressEvent)) error {
	body, _ := json.Marshal(PullRequest{ModelID: modelID, Runtime: runtime})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/model/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := newClient().Do(req)
	if err != nil {
		return fmt.Errorf("connect to daemon socket: %w", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev ProgressEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Error != "" {
			return fmt.Errorf("%s", ev.Error)
		}
		if onProgress != nil {
			onProgress(ev)
		}
		if ev.Done {
			return nil
		}
	}
	return scanner.Err()
}

// StartModel requests the daemon to start a model on a runtime.
func StartModel(ctx context.Context, modelID, runtime string) error {
	return postJSON(ctx, "/model/start", StartRequest{ModelID: modelID, Runtime: runtime})
}

// StopModel requests the daemon to stop a model.
func StopModel(ctx context.Context, modelID, runtime string) error {
	return postJSON(ctx, "/model/stop", StopRequest{ModelID: modelID, Runtime: runtime})
}

// InstallRuntime requests the daemon to install a runtime.
func InstallRuntime(ctx context.Context, name string) error {
	return postJSON(ctx, "/runtime/install", RuntimeRequest{Name: name})
}

// RestartRuntime requests the daemon to restart a runtime service.
func RestartRuntime(ctx context.Context, name string) error {
	return postJSON(ctx, "/runtime/restart", RuntimeRequest{Name: name})
}

func postJSON(ctx context.Context, path string, payload any) error {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	cl := &http.Client{Transport: dialTransport(), Timeout: 5 * time.Minute}
	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("connect to daemon socket: %w", err)
	}
	defer resp.Body.Close()

	var result Response
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("%s", result.Error)
	}
	return nil
}
