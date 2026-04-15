package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Agent version - set at build time via -ldflags
var agentVersion = "0.5.0"

func main() {
	// Execute cobra CLI
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// init adds a default command when no subcommand is provided
func init() {
	// If no subcommand is provided, show help
	rootCmd.Run = func(cmd *cobra.Command, args []string) {
		// Show help when no subcommand is given
		cmd.Help()
	}
}
