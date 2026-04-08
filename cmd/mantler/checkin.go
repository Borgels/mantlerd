package main

import (
	"fmt"
	"log"
	"time"

	"github.com/Borgels/mantlerd/internal/client"
	"github.com/Borgels/mantlerd/internal/commands"
	"github.com/Borgels/mantlerd/internal/config"
	"github.com/Borgels/mantlerd/internal/runtime"
	"github.com/Borgels/mantlerd/internal/types"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var checkinCmd = &cobra.Command{
	Use:   "checkin",
	Short: "Perform a single check-in and exit",
	Long: `Perform a single check-in to the Mantler server.

This command will:
- Report machine metadata (hostname, addresses, hardware summary)
- Pull pending commands from Mantler
- Execute any pending commands
- Acknowledge command results
- Exit after completion (does not start daemon)`,
	Run: runCheckin,
}

func init() {
	rootCmd.AddCommand(checkinCmd)
}

func runCheckin(cmd *cobra.Command, args []string) {
	// Load configuration
	cfg := loadConfig(cmd)

	// Create API client
	cl, err := client.New(cfg.ServerURL, cfg.Token, cfg.Insecure)
	if err != nil {
		log.Fatalf("create api client: %v", err)
	}

	// Create runtime manager and executor
	outcomes := &outcomeBuffer{}
	runtimeManager := runtime.NewManager()
	runtimeManager.SetOutcomeReporter(outcomes.Add)
	executor := commands.NewExecutor(runtimeManager, cfg, func(payload types.AckRequest) {
		sendInProgressAck(cl, payload)
	}, outcomes.Add)

	// Run check-in
	runCheckIn(cfg, cl, runtimeManager, executor, outcomes)

	fmt.Println("Check-in completed successfully")
}

// loadConfigForCheckin loads configuration for checkin command
func loadConfigForCheckin() config.Config {
	// Build config from viper and flags
	intervalDuration, err := time.ParseDuration(viper.GetString("interval"))
	if err != nil {
		log.Fatalf("invalid interval duration: %v", err)
	}

	cfg := config.Config{
		ServerURL: viper.GetString("server"),
		Token:     viper.GetString("token"),
		MachineID: viper.GetString("machine"),
		Interval:  intervalDuration,
		Insecure:  viper.GetBool("insecure"),
		LogLevel:  viper.GetString("log-level"),
	}

	// Apply flag values if set (flags override config file)
	if serverURL != "" {
		cfg.ServerURL = serverURL
	}
	if token != "" {
		cfg.Token = token
	}
	if machineID != "" {
		cfg.MachineID = machineID
	}
	if insecure {
		cfg.Insecure = insecure
	}
	if logLevel != "" {
		cfg.LogLevel = logLevel
	}

	// Validate
	if err := config.Validate(cfg); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	// Persist config
	configPath := cfgFile
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}
	if err := config.Save(configPath, cfg); err != nil {
		log.Fatalf("persist config: %v", err)
	}

	return cfg
}
