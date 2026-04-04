package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Borgels/clawcontrol-agent/internal/client"
	"github.com/Borgels/clawcontrol-agent/internal/commands"
	"github.com/Borgels/clawcontrol-agent/internal/config"
	"github.com/Borgels/clawcontrol-agent/internal/discovery"
	"github.com/Borgels/clawcontrol-agent/internal/runtime"
	"github.com/Borgels/clawcontrol-agent/internal/types"
)

var agentVersion = "0.1.14"
const desiredConfigCachePath = "/etc/clawcontrol/desired-config.json"

func main() {
	cfgPath := flag.String("config", config.DefaultConfigPath(), "Path to config file")
	server := flag.String("server", "", "ClawControl server URL")
	token := flag.String("token", "", "Machine registration token")
	machineID := flag.String("machine", "", "Machine ID")
	interval := flag.Duration("interval", 30*time.Second, "Check-in interval")
	insecure := flag.Bool("insecure", false, "Allow non-HTTPS server")
	logLevel := flag.String("log-level", "info", "Log level")
	once := flag.Bool("once", false, "Run one check-in and exit")
	flag.Parse()

	flagCfg := config.Config{
		ServerURL: *server,
		Token:     *token,
		MachineID: *machineID,
		Interval:  *interval,
		Insecure:  *insecure,
		LogLevel:  *logLevel,
	}

	fileCfg, err := config.Load(*cfgPath)
	if err != nil {
		fileCfg = config.Config{
			Interval:  30 * time.Second,
			LogLevel:  "info",
			Insecure:  false,
			ServerURL: "",
			Token:     "",
			MachineID: "",
		}
	}
	cfg := config.Merge(fileCfg, flagCfg)
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if err := config.Validate(cfg); err != nil {
		log.Fatalf("invalid config: %v", err)
	}
	if err := config.Save(*cfgPath, cfg); err != nil {
		log.Fatalf("persist config: %v", err)
	}

	cl, err := client.New(cfg.ServerURL, cfg.Token, cfg.Insecure)
	if err != nil {
		log.Fatalf("create api client: %v", err)
	}
	runtimeManager := runtime.NewManager()
	executor := commands.NewExecutor(runtimeManager, cfg, func(commandID string, details string) {
		if strings.TrimSpace(commandID) == "" || strings.TrimSpace(details) == "" {
			return
		}
		ackErr := cl.Ack(context.Background(), types.AckRequest{
			CommandID: commandID,
			Status:    "in_progress",
			Details:   details,
		})
		if ackErr != nil {
			log.Printf("progress ack failed for %s: %v", commandID, ackErr)
		}
	})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	runOnce := func() {
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

		resp, err := client.Retry(ctx, 3, func() (types.CheckinResponse, error) {
			return cl.Checkin(ctx, payload)
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

	if *once {
		runOnce()
		return
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	runOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

func enforceDesiredConfig(runtimeManager *runtime.Manager, desired types.DesiredConfig) {
	for _, runtimeType := range desired.Runtimes {
		if err := runtimeManager.EnsureRuntime(string(runtimeType)); err != nil {
			log.Printf("failed to ensure runtime %s: %v", runtimeType, err)
		}
	}

	modelsHandled := map[string]bool{}
	for _, target := range desired.ModelTargets {
		modelsHandled[target.ModelID] = true
		flags := target.FeatureFlags
		if err := runtimeManager.EnsureModelWithRuntime(target.ModelID, string(target.Runtime), &flags); err != nil {
			log.Printf("failed to ensure model target %s: %v", target.ModelID, err)
		}
	}
	for _, modelID := range desired.Models {
		if modelsHandled[modelID] {
			continue
		}
		if err := runtimeManager.EnsureModelWithFlags(modelID, nil); err != nil {
			log.Printf("failed to ensure model %s: %v", modelID, err)
		}
	}
}

func loadCachedDesiredConfig() types.DesiredConfig {
	raw, err := os.ReadFile(desiredConfigCachePath)
	if err != nil {
		return types.DesiredConfig{}
	}
	var desired types.DesiredConfig
	if err := json.Unmarshal(raw, &desired); err != nil {
		log.Printf("failed to parse desired config cache: %v", err)
		return types.DesiredConfig{}
	}
	return desired
}

func saveCachedDesiredConfig(desired types.DesiredConfig) error {
	if err := os.MkdirAll(filepath.Dir(desiredConfigCachePath), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(desired, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(desiredConfigCachePath, append(payload, '\n'), 0o600)
}

func toRuntimeTypes(values []string) []types.RuntimeType {
	result := make([]types.RuntimeType, 0, len(values))
	for _, value := range values {
		result = append(result, types.RuntimeType(value))
	}
	return result
}

func toInstalledModels(runtimeManager *runtime.Manager) []types.InstalledModel {
	result := make([]types.InstalledModel, 0)
	seen := map[string]struct{}{}
	type installedModelsProvider interface {
		InstalledModels() []types.InstalledModel
	}
	for _, runtimeName := range runtimeManager.InstalledRuntimes() {
		driver, err := runtimeManager.DriverFor(runtimeName)
		if err != nil {
			continue
		}
		if provider, ok := driver.(installedModelsProvider); ok {
			for _, model := range provider.InstalledModels() {
				modelID := strings.TrimSpace(model.ModelID)
				if modelID == "" {
					continue
				}
				key := runtimeName + "::" + modelID
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}
				result = append(result, model)
			}
			continue
		}
		for _, modelID := range driver.ListModels() {
			modelID = strings.TrimSpace(modelID)
			if modelID == "" {
				continue
			}
			key := runtimeName + "::" + modelID
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, types.InstalledModel{
				ModelID: modelID,
				Runtime: types.RuntimeType(runtimeName),
				Status:  types.ModelReady,
			})
		}
	}
	return result
}

func ackCommandWithRetry(cl *client.Client, payload types.AckRequest) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := cl.Ack(ackCtx, payload)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(300 * time.Millisecond)
	}
	return lastErr
}
