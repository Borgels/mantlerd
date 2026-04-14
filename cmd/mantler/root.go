package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	cfgFile          string
	serverURL        string
	relayURL         string
	cloudflareTunnelHostname string
	token            string
	machineID        string
	interval         string
	insecure         bool
	logLevel         string
	cloudProvisioned bool
)

var rootCmd = &cobra.Command{
	Use:   "mantler",
	Short: "Mantler daemon CLI for machine management",
	Long: `Mantler daemon is a lightweight machine agent for Mantler.

It performs periodic authenticated check-ins, reports machine metadata,
pulls pending commands, and executes allowlisted commands.`,
}

func init() {
	cobra.OnInitialize(initConfig)

	// Persistent flags (available to all subcommands)
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "config file (default is /etc/mantler/agent.json or ~/.mantler/agent.json)")
	rootCmd.PersistentFlags().StringVarP(&serverURL, "server", "s", "", "Mantler server URL")
	rootCmd.PersistentFlags().StringVar(&relayURL, "relay-url", "", "Relay WebSocket URL override")
	rootCmd.PersistentFlags().StringVar(&cloudflareTunnelHostname, "cloudflare-tunnel-hostname", "", "Cloudflare tunnel hostname for encrypted ingress")
	rootCmd.PersistentFlags().StringVarP(&token, "token", "t", "", "Machine registration token")
	rootCmd.PersistentFlags().StringVarP(&machineID, "machine", "m", "", "Machine ID")
	rootCmd.PersistentFlags().StringVarP(&interval, "interval", "i", "30s", "Check-in interval")
	rootCmd.PersistentFlags().BoolVarP(&insecure, "insecure", "k", false, "Allow non-HTTPS server")
	rootCmd.PersistentFlags().StringVarP(&logLevel, "log-level", "l", "info", "Log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().BoolVar(&cloudProvisioned, "cloud-provisioned", false, "Mark this agent as cloud provisioned")

	// Bind flags to viper
	viper.BindPFlag("server", rootCmd.PersistentFlags().Lookup("server"))
	viper.BindPFlag("relay-url", rootCmd.PersistentFlags().Lookup("relay-url"))
	viper.BindPFlag("cloudflare-tunnel-hostname", rootCmd.PersistentFlags().Lookup("cloudflare-tunnel-hostname"))
	viper.BindPFlag("token", rootCmd.PersistentFlags().Lookup("token"))
	viper.BindPFlag("machine", rootCmd.PersistentFlags().Lookup("machine"))
	viper.BindPFlag("interval", rootCmd.PersistentFlags().Lookup("interval"))
	viper.BindPFlag("insecure", rootCmd.PersistentFlags().Lookup("insecure"))
	viper.BindPFlag("log-level", rootCmd.PersistentFlags().Lookup("log-level"))
	viper.BindPFlag("cloud-provisioned", rootCmd.PersistentFlags().Lookup("cloud-provisioned"))
}

func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		// Check if running as root
		if os.Geteuid() == 0 {
			viper.SetConfigFile("/etc/mantler/agent.json")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error getting home directory: %v\n", err)
				os.Exit(1)
			}
			viper.AddConfigPath(home + "/.mantler")
			viper.SetConfigName("agent")
			viper.SetConfigType("json")
		}
	}

	viper.AutomaticEnv()

	// Read config file if it exists (don't error if it doesn't)
	viper.ReadInConfig()
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
