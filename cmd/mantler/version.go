package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number",
	Long:  `Print the version number of mantlerd.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("mantlerd %s\n", agentVersion)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
