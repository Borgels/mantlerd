package main

import (
	"context"
	"flag"
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
)

const agentVersion = "0.1.0"

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
	executor := commands.NewExecutor(runtimeManager)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	runOnce := func() {
		report := discovery.Collect()
		payload := types.CheckinRequest{
			MachineID:       cfg.MachineID,
			Hostname:        report.Hostname,
			Addresses:       report.Addresses,
			HardwareSummary: report.HardwareSummary,
			AgentVersion:    agentVersion,
			InstalledRuntimeTypes: toRuntimeTypes(runtimeManager.InstalledRuntimes()),
		}

		resp, err := client.Retry(ctx, 3, func() (types.CheckinResponse, error) {
			return cl.Checkin(ctx, payload)
		})
		if err != nil {
			log.Printf("checkin error: %v", err)
			return
		}

		for _, runtimeType := range resp.DesiredConfig.Runtimes {
			if err := runtimeManager.EnsureRuntime(string(runtimeType)); err != nil {
				log.Printf("failed to ensure runtime %s: %v", runtimeType, err)
			}
		}

		for _, command := range resp.Commands {
			err := executor.Execute(command)
			status := "success"
			details := ""
			if err != nil {
				status = "failed"
				details = err.Error()
				log.Printf("command %s (%s) failed: %v", command.ID, command.Type, err)
			} else {
				log.Printf("command %s (%s) completed", command.ID, command.Type)
			}
			ackErr := cl.Ack(ctx, types.AckRequest{
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

func toRuntimeTypes(values []string) []types.RuntimeType {
	result := make([]types.RuntimeType, 0, len(values))
	for _, value := range values {
		result = append(result, types.RuntimeType(value))
	}
	return result
}
