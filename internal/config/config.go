package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	ServerURL string        `json:"serverUrl"`
	Token     string        `json:"token"`
	MachineID string        `json:"machineId"`
	Interval  time.Duration `json:"interval"`
	Insecure  bool          `json:"insecure"`
	LogLevel  string        `json:"logLevel"`
}

type FileConfig struct {
	ServerURL  string `json:"serverUrl"`
	Token      string `json:"token"`
	MachineID  string `json:"machineId"`
	IntervalMS int64  `json:"intervalMs"`
	Insecure   bool   `json:"insecure"`
	LogLevel   string `json:"logLevel"`
}

func DefaultConfigPath() string {
	if os.Geteuid() == 0 {
		const preferred = "/etc/mantler/agent.json"
		if _, err := os.Stat(preferred); err == nil {
			return preferred
		}
		const legacy = "/etc/clawcontrol/agent.json"
		if _, err := os.Stat(legacy); err == nil {
			return legacy
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
	legacy := filepath.Join(home, ".clawcontrol", "agent.json")
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return preferred
}

func Load(path string) (Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	var raw FileConfig
	if err := json.Unmarshal(content, &raw); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return Config{
		ServerURL: raw.ServerURL,
		Token:     raw.Token,
		MachineID: raw.MachineID,
		Interval:  time.Duration(raw.IntervalMS) * time.Millisecond,
		Insecure:  raw.Insecure,
		LogLevel:  raw.LogLevel,
	}, nil
}

func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	raw := FileConfig{
		ServerURL:  cfg.ServerURL,
		Token:      cfg.Token,
		MachineID:  cfg.MachineID,
		IntervalMS: cfg.Interval.Milliseconds(),
		Insecure:   cfg.Insecure,
		LogLevel:   cfg.LogLevel,
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
