package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Borgels/clawcontrol-agent/internal/client"
	"github.com/Borgels/clawcontrol-agent/internal/commands"
	"github.com/Borgels/clawcontrol-agent/internal/config"
	"github.com/Borgels/clawcontrol-agent/internal/discovery"
	"github.com/Borgels/clawcontrol-agent/internal/runtime"
	"github.com/Borgels/clawcontrol-agent/internal/types"
	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the mantler daemon agent",
	Long: `Start the mantler daemon agent which performs periodic check-ins
to the Mantler server, reports machine metadata, and executes commands.`,
	Run: runStart,
}

func init() {
	rootCmd.AddCommand(startCmd)
}

func runStart(cmd *cobra.Command, args []string) {
	// Load configuration
	cfg := loadConfig(cmd)

	// Create API client
	cl, err := client.New(cfg.ServerURL, cfg.Token, cfg.Insecure)
	if err != nil {
		log.Fatalf("create api client: %v", err)
	}

	// Create runtime manager and executor
	runtimeManager := runtime.NewManager()
	executor := commands.NewExecutor(runtimeManager, cfg, func(payload types.AckRequest) {
		sendInProgressAck(cl, payload)
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

func loadConfig(cmd *cobra.Command) config.Config {
	configPath := cfgFile
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}

	fileCfg := config.Config{}
	loadedCfg, err := config.Load(configPath)
	if err == nil {
		fileCfg = loadedCfg
	} else if !os.IsNotExist(err) {
		log.Fatalf("load config: %v", err)
	}

	flagsCfg := config.Config{}
	if cmd.Flags().Changed("server") {
		flagsCfg.ServerURL = serverURL
	}
	if cmd.Flags().Changed("token") {
		flagsCfg.Token = token
	}
	if cmd.Flags().Changed("machine") {
		flagsCfg.MachineID = machineID
	}
	if cmd.Flags().Changed("interval") {
		intervalDuration, parseErr := time.ParseDuration(interval)
		if parseErr != nil {
			log.Fatalf("invalid interval duration: %v", parseErr)
		}
		flagsCfg.Interval = intervalDuration
	}
	if cmd.Flags().Changed("insecure") {
		flagsCfg.Insecure = insecure
	}
	if cmd.Flags().Changed("log-level") {
		flagsCfg.LogLevel = logLevel
	}

	cfg := config.Merge(fileCfg, flagsCfg)
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}

	// Validate
	if err := config.Validate(cfg); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	// Persist config
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
	runtimeStatuses := buildRuntimeStatuses(installedRuntimeNames, readyRuntimeNames)
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
		MachineID:              cfg.MachineID,
		Hostname:               report.Hostname,
		Addresses:              report.Addresses,
		HardwareSummary:        report.HardwareSummary,
		RAMTotalMB:             report.RAMTotalMB,
		GPUs:                   toProtocolGPUInfo(report.GPUs),
		AgentVersion:           agentVersion,
		RuntimeStatus:          runtimeStatus,
		RuntimeStatuses:        runtimeStatuses,
		RuntimeType:            runtimeType,
		RuntimeVersion:         runtimeVersion,
		RuntimeVersions:        runtimeManager.RuntimeVersions(),
		InstalledRuntimeTypes:  installedRuntimeTypes,
		InstalledModels:        toInstalledModels(runtimeManager),
		InstalledHarnesses:     toInstalledHarnesses(cachedDesired),
		InstalledOrchestrators: toInstalledOrchestrators(cachedDesired),
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
	desiredHarnesses := toInstalledHarnesses(resp.DesiredConfig)
	desiredOrchestrators := toInstalledOrchestrators(resp.DesiredConfig)
	if harnessReportsDiffer(payload.InstalledHarnesses, desiredHarnesses) || orchestratorReportsDiffer(payload.InstalledOrchestrators, desiredOrchestrators) {
		refreshPayload := payload
		refreshPayload.InstalledHarnesses = desiredHarnesses
		refreshPayload.InstalledOrchestrators = desiredOrchestrators
		refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer refreshCancel()
		if _, err := cl.Checkin(refreshCtx, refreshPayload); err != nil {
			log.Printf("follow-up harness/orchestrator refresh checkin failed: %v", err)
		}
	}
	enforceDesiredConfig(runtimeManager, resp.DesiredConfig)
	reconcileStaleModels(runtimeManager, resp.DesiredConfig, executor.ActiveManifestModelIDs(cfg.MachineID))

	// Execute commands
	for _, command := range resp.Commands {
		result, err := executor.Execute(command)
		status := "success"
		if err != nil {
			status = "failed"
			if strings.TrimSpace(result.Details) == "" {
				result.Details = err.Error()
			}
			log.Printf("command %s (%s) failed: %v", command.ID, command.Type, err)
		} else {
			log.Printf("command %s (%s) completed", command.ID, command.Type)
		}
		ackErr := ackCommandWithRetry(cl, types.AckRequest{
			CommandID:     command.ID,
			Status:        status,
			Details:       result.Details,
			ResultPayload: result.ResultPayload,
		})
		if ackErr != nil {
			log.Printf("ack failed for %s: %v", command.ID, ackErr)
		}
	}
}

func toProtocolGPUInfo(values []discovery.GPUInfo) []types.GPUInfo {
	if len(values) == 0 {
		return nil
	}
	result := make([]types.GPUInfo, 0, len(values))
	for _, value := range values {
		name := strings.TrimSpace(value.Name)
		if name == "" {
			continue
		}
		result = append(result, types.GPUInfo{
			Name:              name,
			MemoryTotalMB:     value.MemoryTotalMB,
			MemoryUsedMB:      value.MemoryUsedMB,
			MemoryFreeMB:      value.MemoryFreeMB,
			Architecture:      strings.TrimSpace(value.Architecture),
			ComputeCapability: strings.TrimSpace(value.ComputeCapability),
			UnifiedMemory:     value.UnifiedMemory,
		})
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func buildRuntimeStatuses(installedRuntimeNames []string, readyRuntimeNames []string) map[types.RuntimeType]types.RuntimeStatus {
	statuses := make(map[types.RuntimeType]types.RuntimeStatus, len(installedRuntimeNames))
	for _, runtimeName := range installedRuntimeNames {
		statuses[types.RuntimeType(runtimeName)] = types.RuntimeFailed
	}
	for _, runtimeName := range readyRuntimeNames {
		statuses[types.RuntimeType(runtimeName)] = types.RuntimeReady
	}
	if len(statuses) == 0 {
		return nil
	}
	return statuses
}

func sendInProgressAck(cl *client.Client, payload types.AckRequest) {
	if payload.CommandID == "" || (payload.Details == "" && len(payload.StreamEvents) == 0) {
		return
	}
	payload.Status = "in_progress"
	err := cl.Ack(context.Background(), payload)
	if err != nil {
		log.Printf("progress ack failed for %s: %v", payload.CommandID, err)
	}
}
