package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Borgels/clawcontrol-agent/internal/client"
	"github.com/Borgels/clawcontrol-agent/internal/commands"
	"github.com/Borgels/clawcontrol-agent/internal/config"
	"github.com/Borgels/clawcontrol-agent/internal/discovery"
	"github.com/Borgels/clawcontrol-agent/internal/runtime"
	"github.com/Borgels/clawcontrol-agent/internal/types"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the clawcontrol agent daemon",
	Long: `Start the clawcontrol agent daemon which performs periodic check-ins
to the ClawControl server, reports machine metadata, and executes commands.`,
	Run: runStart,
}

func init() {
	rootCmd.AddCommand(startCmd)
}

func runStart(cmd *cobra.Command, args []string) {
	// Load configuration
	cfg := loadConfig()

	// Create API client
	cl, err := client.New(cfg.ServerURL, cfg.Token, cfg.Insecure)
	if err != nil {
		log.Fatalf("create api client: %v", err)
	}

	// Create runtime manager and executor
	runtimeManager := runtime.NewManager()
	executor := commands.NewExecutor(runtimeManager, cfg, func(commandID string, details string) {
		sendInProgressAck(cl, commandID, details)
	})

	// Set up signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Run initial check-in
	runCheckIn(cfg, cl, runtimeManager, executor)

	// Start ticker for periodic check-ins
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down agent...")
			return
		case <-ticker.C:
			runCheckIn(cfg, cl, runtimeManager, executor)
		}
	}
}

func loadConfig() config.Config {
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

func runCheckIn(cfg config.Config, cl *client.Client, runtimeManager *runtime.Manager, executor *commands.Executor) {
	cachedDesired := loadCachedDesiredConfig()

	report := discovery.Collect()
	installedRuntimeNames := runtimeManager.InstalledRuntimes()
	installedRuntimeTypes := toRuntimeTypes(installedRuntimeNames)
	readyRuntimeNames := runtimeManager.ReadyRuntimes()
	runtimeStatus := types.RuntimeNotInstalled
	runtimeType := types.RuntimeType("")
	runtimeVersion := ""
	if len(installedRuntimeNames) > 0 {
		runtimeStatus = types.RuntimeFailed
		runtimeType = types.RuntimeType(installedRuntimeNames[0])
		runtimeVersion = runtimeManager.RuntimeVersion(installedRuntimeNames[0])
	}
	if len(readyRuntimeNames) > 0 {
		runtimeStatus = types.RuntimeReady
		runtimeType = types.RuntimeType(readyRuntimeNames[0])
		runtimeVersion = runtimeManager.RuntimeVersion(readyRuntimeNames[0])
	}

	payload := types.CheckinRequest{
		MachineID:             cfg.MachineID,
		Hostname:              report.Hostname,
		Addresses:             report.Addresses,
		HardwareSummary:       report.HardwareSummary,
		AgentVersion:          agentVersion,
		RuntimeStatus:         runtimeStatus,
		RuntimeType:           runtimeType,
		RuntimeVersion:        runtimeVersion,
		RuntimeVersions:       runtimeManager.RuntimeVersions(),
		InstalledRuntimeTypes: installedRuntimeTypes,
		InstalledModels:       toInstalledModels(runtimeManager),
	}

	resp, err := client.Retry(context.Background(), 3, func() (types.CheckinResponse, error) {
		return cl.Checkin(context.Background(), payload)
	})
	if err != nil {
		log.Printf("checkin error: %v", err)
		enforceDesiredConfig(runtimeManager, cachedDesired)
		return
	}

	if err := saveCachedDesiredConfig(resp.DesiredConfig); err != nil {
		log.Printf("failed to persist desired config cache: %v", err)
	}
	enforceDesiredConfig(runtimeManager, resp.DesiredConfig)

	// Execute commands
	for _, command := range resp.Commands {
		details, err := executor.Execute(command)
		status := "success"
		if err != nil {
			status = "failed"
			details = err.Error()
			log.Printf("command %s (%s) failed: %v", command.ID, command.Type, err)
		} else {
			log.Printf("command %s (%s) completed", command.ID, command.Type)
		}
		ackErr := ackCommandWithRetry(cl, types.AckRequest{
			CommandID: command.ID,
			Status:    status,
			Details:   details,
		})
		if ackErr != nil {
			log.Printf("ack failed for %s: %v", command.ID, ackErr)
		}
	}
}

func sendInProgressAck(cl *client.Client, commandID string, details string) {
	if commandID == "" || details == "" {
		return
	}
	err := cl.Ack(context.Background(), types.AckRequest{
		CommandID: commandID,
		Status:    "in_progress",
		Details:   details,
	})
	if err != nil {
		log.Printf("progress ack failed for %s: %v", commandID, err)
	}
}
