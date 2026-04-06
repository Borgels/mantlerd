package main

import (
	"fmt"
	"os"
	"time"

	"github.com/Borgels/clawcontrol-agent/internal/config"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage mantler daemon configuration",
	Long: `Manage mantler daemon configuration.

This command provides subcommands for viewing and modifying the agent configuration.`,
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Display current configuration",
	Long: `Display the current mantler daemon configuration.

Shows configuration loaded from config file and flags.`,
	Run: runConfigShow,
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a configuration value",
	Long: `Set a configuration value in the config file.

Valid keys:
  server     - Mantler server URL
  token      - Machine registration token
  machine    - Machine ID
  interval   - Check-in interval (e.g., 30s, 1m, 5m)
  insecure   - Allow non-HTTPS server (true/false)
  log-level  - Log level (debug, info, warn, error)

Examples:
  mantler config set server https://control.example.com
  mantler config set interval 1m
  mantler config set insecure true`,
	Args: cobra.ExactArgs(2),
	Run:  runConfigSet,
}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Show config file path",
	Long: `Show the path to the configuration file.

The path depends on whether running as root or as a regular user:
- Root: /etc/mantler/agent.json
- User: ~/.mantler/agent.json`,
	Run: runConfigPath,
}

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.AddCommand(configShowCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configPathCmd)
}

func runConfigShow(cmd *cobra.Command, args []string) {
	// Get config file path
	configPath := cfgFile
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}

	fmt.Printf("Config file: %s\n\n", configPath)

	// Check if config file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		fmt.Println("Note: Config file does not exist yet.")
		fmt.Println("Configuration will be persisted on first run.")
		return
	}

	// Load config from file
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Display config values
	fmt.Printf("Server URL:    %s\n", cfg.ServerURL)
	fmt.Printf("Machine ID:    %s\n", cfg.MachineID)
	fmt.Printf("Token:         %s\n", maskToken(cfg.Token))
	fmt.Printf("Interval:      %s\n", cfg.Interval)
	fmt.Printf("Insecure:      %v\n", cfg.Insecure)
	fmt.Printf("Log Level:     %s\n", cfg.LogLevel)
}

func runConfigSet(cmd *cobra.Command, args []string) {
	key := args[0]
	value := args[1]

	// Get config file path
	configPath := cfgFile
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}

	// Load existing config or create new one
	var cfg config.Config
	existingCfg, err := config.Load(configPath)
	if err == nil {
		cfg = existingCfg
	}

	// Set the specified key
	switch key {
	case "server":
		cfg.ServerURL = value
	case "token":
		cfg.Token = value
	case "machine":
		cfg.MachineID = value
	case "interval":
		// Parse duration
		duration, err := parseDuration(value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid interval format: %v\n", err)
			fmt.Fprintln(os.Stderr, "Use format like: 30s, 1m, 5m")
			os.Exit(1)
		}
		cfg.Interval = duration
	case "insecure":
		if value == "true" || value == "1" || value == "yes" {
			cfg.Insecure = true
		} else if value == "false" || value == "0" || value == "no" {
			cfg.Insecure = false
		} else {
			fmt.Fprintf(os.Stderr, "Error: invalid value for insecure: %s (use true/false)\n", value)
			os.Exit(1)
		}
	case "log-level", "loglevel", "log_level":
		if value != "debug" && value != "info" && value != "warn" && value != "error" {
			fmt.Fprintf(os.Stderr, "Error: invalid log level: %s (use debug/info/warn/error)\n", value)
			os.Exit(1)
		}
		cfg.LogLevel = value
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown configuration key: %s\n", key)
		fmt.Fprintln(os.Stderr, "Valid keys: server, token, machine, interval, insecure, log-level")
		os.Exit(1)
	}

	// Save config
	if err := config.Save(configPath, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Set %s = %s\n", key, value)
	fmt.Printf("Configuration saved to: %s\n", configPath)
}

func runConfigPath(cmd *cobra.Command, args []string) {
	configPath := cfgFile
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}
	fmt.Println(configPath)
}

func maskToken(token string) string {
	if token == "" {
		return "(not set)"
	}
	if len(token) <= 8 {
		return "***"
	}
	return token[:4] + "..." + token[len(token)-4:]
}

func parseDuration(s string) (time.Duration, error) {
	// Parse duration string like "30s", "1m", "5m"
	return time.ParseDuration(s)
}
