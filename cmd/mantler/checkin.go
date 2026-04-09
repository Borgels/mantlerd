package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Borgels/mantlerd/internal/client"
	"github.com/Borgels/mantlerd/internal/commands"
	"github.com/Borgels/mantlerd/internal/runtime"
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
	runtimeManager.SetOutcomeReporter(outcomes.Add)
	executor := commands.NewExecutor(runtimeManager, trainerManager, cfg, func(payload types.AckRequest) {
		sendInProgressAck(cl, payload)
	}, outcomes.Add)

	// Run check-in
	runCheckIn(context.Background(), cfg, cl, runtimeManager, trainerManager, executor, outcomes, time.Now())

	fmt.Println("Check-in completed successfully")
}
