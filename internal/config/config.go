package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	ServerURL        string                 `json:"serverUrl"`
	RelayURL         string                 `json:"relayUrl,omitempty"`
	CloudflareTunnelHostname string         `json:"cloudflareTunnelHostname,omitempty"`
	Token            string                 `json:"token"`
	MachineID        string                 `json:"machineId"`
	Interval         time.Duration          `json:"interval"`
	Insecure         bool                   `json:"insecure"`
	LogLevel         string                 `json:"logLevel"`
	CloudProvisioned bool                   `json:"cloudProvisioned"`
	Origin           map[string]interface{} `json:"origin,omitempty"`
}

type FileConfig struct {
	ServerURL        string                 `json:"serverUrl"`
	RelayURL         string                 `json:"relayUrl,omitempty"`
	CloudflareTunnelHostname string         `json:"cloudflareTunnelHostname,omitempty"`
	Token            string                 `json:"token"`
	MachineID        string                 `json:"machineId"`
	IntervalMS       int64                  `json:"intervalMs"`
	Insecure         bool                   `json:"insecure"`
	LogLevel         string                 `json:"logLevel"`
	CloudProvisioned bool                   `json:"cloudProvisioned"`
	Origin           map[string]interface{} `json:"origin,omitempty"`
}

func DefaultConfigPath() string {
	if os.Geteuid() == 0 {
		const preferred = "/etc/mantler/agent.json"
		if _, err := os.Stat(preferred); err == nil {
			return preferred
		}
		return preferred
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "./agent.json"
	}
	preferred := filepath.Join(home, ".mantler", "agent.json")
	if _, err := os.Stat(preferred); err == nil {
		return preferred
	}
	return preferred
}

func Load(path string) (Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if fallbackPath, ok := userConfigFallbackPath(path); ok {
				content, err = os.ReadFile(fallbackPath)
			}
		}
		if err != nil {
			return Config{}, err
		}
	}

	var raw FileConfig
	if err := json.Unmarshal(content, &raw); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return Config{
		ServerURL:        raw.ServerURL,
		RelayURL:         raw.RelayURL,
		CloudflareTunnelHostname: raw.CloudflareTunnelHostname,
		Token:            raw.Token,
		MachineID:        raw.MachineID,
		Interval:         time.Duration(raw.IntervalMS) * time.Millisecond,
		Insecure:         raw.Insecure,
		LogLevel:         raw.LogLevel,
		CloudProvisioned: raw.CloudProvisioned,
		Origin:           raw.Origin,
	}, nil
}

func userConfigFallbackPath(path string) (string, bool) {
	normalized := filepath.ToSlash(strings.TrimSpace(path))
	if strings.HasSuffix(normalized, "/.mantler/agent.json") {
		return "/etc/mantler/agent.json", true
	}
	return "", false
}

func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	raw := FileConfig{
		ServerURL:        cfg.ServerURL,
		RelayURL:         cfg.RelayURL,
		CloudflareTunnelHostname: cfg.CloudflareTunnelHostname,
		Token:            cfg.Token,
		MachineID:        cfg.MachineID,
		IntervalMS:       cfg.Interval.Milliseconds(),
		Insecure:         cfg.Insecure,
		LogLevel:         cfg.LogLevel,
		CloudProvisioned: cfg.CloudProvisioned,
		Origin:           cfg.Origin,
	}
	content, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	content = append(content, '\n')

	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func Merge(fileCfg Config, flagsCfg Config) Config {
	merged := fileCfg
	if flagsCfg.ServerURL != "" {
		merged.ServerURL = flagsCfg.ServerURL
	}
	if flagsCfg.Token != "" {
		merged.Token = flagsCfg.Token
	}
	if flagsCfg.RelayURL != "" {
		merged.RelayURL = flagsCfg.RelayURL
	}
	if flagsCfg.CloudflareTunnelHostname != "" {
		merged.CloudflareTunnelHostname = flagsCfg.CloudflareTunnelHostname
	}
	if flagsCfg.MachineID != "" {
		merged.MachineID = flagsCfg.MachineID
	}
	if flagsCfg.Interval > 0 {
		merged.Interval = flagsCfg.Interval
	}
	if flagsCfg.LogLevel != "" {
		merged.LogLevel = flagsCfg.LogLevel
	}
	if flagsCfg.Insecure {
		merged.Insecure = true
	}
	if flagsCfg.CloudProvisioned {
		merged.CloudProvisioned = true
	}
	if flagsCfg.Origin != nil {
		merged.Origin = flagsCfg.Origin
	}
	return merged
}

func Validate(cfg Config) error {
	if cfg.ServerURL == "" {
		return errors.New("server URL is required")
	}
	if cfg.Token == "" {
		return errors.New("token is required")
	}
	if cfg.MachineID == "" {
		return errors.New("machine ID is required")
	}
	if cfg.Interval <= 0 {
		return errors.New("interval must be greater than zero")
	}
	if cfg.LogLevel == "" {
		return errors.New("log level is required")
	}
	return nil
}
