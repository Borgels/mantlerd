package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Borgels/mantlerd/internal/relay"
	"github.com/Borgels/mantlerd/internal/types"
)

const connectivityCacheTTL = 2 * time.Minute

type ConnectivityDetector struct {
	mu                        sync.Mutex
	expiresAt                 time.Time
	cached                    *types.MachineConnectivity
	cloudflareTunnelHostname  string
}

func newConnectivityDetector() *ConnectivityDetector {
	return &ConnectivityDetector{}
}

func (d *ConnectivityDetector) SetCloudflareTunnelHostname(hostname string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	next := strings.TrimSpace(hostname)
	if d.cloudflareTunnelHostname == next {
		return
	}
	d.cloudflareTunnelHostname = next
	d.cached = nil
	d.expiresAt = time.Time{}
}

func (d *ConnectivityDetector) Detect(
	ctx context.Context,
	runtime types.RuntimeType,
	relayClient *relay.Client,
) *types.MachineConnectivity {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.cached != nil && time.Now().Before(d.expiresAt) {
		copy := *d.cached
		if copy.Kind == "relay" && relayClient != nil {
			copy.RelayConnected = relayClient.Connected()
			copy.RelayLatencyMs = relayClient.RelayLatencyMs()
		}
		return &copy
	}

	if d.cloudflareTunnelHostname != "" {
		d.cached = &types.MachineConnectivity{
			Kind:           "cftunnel",
			TunnelHostname: d.cloudflareTunnelHostname,
		}
		d.expiresAt = time.Now().Add(connectivityCacheTTL)
		copy := *d.cached
		return &copy
	}

	if tailscaleIP := detectTailscaleAddress(); tailscaleIP != "" {
		d.cached = &types.MachineConnectivity{
			Kind:        "tailscale",
			TailscaleIP: tailscaleIP,
		}
		d.expiresAt = time.Now().Add(connectivityCacheTTL)
		copy := *d.cached
		return &copy
	}

	if publicIP := detectPublicIP(ctx); publicIP != "" {
		d.cached = &types.MachineConnectivity{
			Kind:       "direct",
			Address:    publicIP,
			Port:       runtimeDefaultPort(runtime),
			TLSEnabled: false,
		}
		d.expiresAt = time.Now().Add(connectivityCacheTTL)
		copy := *d.cached
		return &copy
	}

	relayConnected := false
	relayLatencyMs := int64(0)
	if relayClient != nil {
		relayConnected = relayClient.Connected()
		relayLatencyMs = relayClient.RelayLatencyMs()
	}
	d.cached = &types.MachineConnectivity{
		Kind:           "relay",
		RelayConnected: relayConnected,
		RelayLatencyMs: relayLatencyMs,
	}
	d.expiresAt = time.Now().Add(connectivityCacheTTL)
	copy := *d.cached
	return &copy
}

func detectTailscaleAddress() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range interfaces {
		if strings.TrimSpace(iface.Name) == "" || !strings.Contains(strings.ToLower(iface.Name), "tailscale") {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addresses {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil {
				continue
			}
			if ip[0] == 100 {
				return ip.String()
			}
		}
	}
	return ""
}

func detectPublicIP(ctx context.Context) string {
	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return ""
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(string(body))
	if net.ParseIP(value) == nil {
		return ""
	}
	return value
}

func runtimeDefaultPort(runtime types.RuntimeType) int {
	switch runtime {
	case types.RuntimeVLLM, types.RuntimeTensorRT:
		return 8000
	case types.RuntimeLlamaCpp:
		return 1234
	case types.RuntimeQuantCPP, types.RuntimeMLX:
		return 8080
	case types.RuntimeOllama:
		fallthrough
	default:
		return 11434
	}
}
