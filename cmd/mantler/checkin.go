package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Borgels/mantlerd/internal/client"
	"github.com/Borgels/mantlerd/internal/commands"
	"github.com/Borgels/mantlerd/internal/runtime"
	agenttools "github.com/Borgels/mantlerd/internal/tools"
	"github.com/Borgels/mantlerd/internal/trainer"
	"github.com/Borgels/mantlerd/internal/types"
	"github.com/spf13/cobra"
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
	trainerManager := trainer.NewManager()
	toolManager := agenttools.NewManager()
	runtimeManager.SetOutcomeReporter(outcomes.Add)
	executor := commands.NewExecutor(runtimeManager, trainerManager, toolManager, cfg, func(payload types.AckRequest) {
		sendInProgressAck(cl, payload)
	}, outcomes.Add)
	dispatcher := newCommandDispatcher(context.Background(), executor, cl, defaultLightCommandConcurrency)
	connectivityDetector := newConnectivityDetector()
	connectivityDetector.SetCloudflareTunnelHostname(cfg.CloudflareTunnelHostname)

	// Run check-in
	cycle := runCheckIn(
		context.Background(),
		cfg,
		cl,
		runtimeManager,
		trainerManager,
		toolManager,
		executor,
		outcomes,
		dispatcher,
		nil,
		connectivityDetector,
		time.Now(),
		true,
	)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer waitCancel()
	if !dispatcher.WaitForIdle(waitCtx) {
		log.Fatalf("timed out waiting for command completion")
	}
	if !cycle.success {
		log.Fatalf("check-in failed")
	}

	fmt.Println("Check-in completed successfully")
}
