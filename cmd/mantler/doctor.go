package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Borgels/mantlerd/internal/client"
	"github.com/Borgels/mantlerd/internal/config"
	"github.com/Borgels/mantlerd/internal/discovery"
	"github.com/Borgels/mantlerd/internal/runtime"
	"github.com/Borgels/mantlerd/internal/types"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Run diagnostic checks",
	Long: `Run diagnostic checks to verify the mantler daemon setup.

This command checks:
- Configuration file and settings
- Server connectivity and authentication
- Runtime availability and status
- System permissions
- Network connectivity

Use this command to troubleshoot issues with the agent.`,
	Run: runDoctor,
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

func runDoctor(cmd *cobra.Command, args []string) {
	fmt.Println("Running diagnostics...")
	fmt.Println()

	allPassed := true
	fileCfg := config.Config{}

	// Check 1: Configuration file
	fmt.Print("✓ Checking configuration... ")
	configPath := cfgFile
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		fmt.Println("✗")
		fmt.Printf("  Config file not found: %s\n", configPath)
		fmt.Println("  Run: mantler config set server <url>")
		fmt.Println("       mantler config set token <token>")
		fmt.Println("       mantler config set machine <id>")
		allPassed = false
	} else {
		cfg, err := config.Load(configPath)
		if err != nil {
			fmt.Println("✗")
			fmt.Printf("  Error reading config: %v\n", err)
			allPassed = false
		} else {
			fileCfg = cfg
			fmt.Println("✓")
			fmt.Printf("  Config file: %s\n", configPath)
			if cfg.ServerURL != "" {
				fmt.Printf("  Server: %s\n", maskURL(cfg.ServerURL))
			}
		}
	}
	fmt.Println()

	// Check 2: Required configuration values
	fmt.Print("✓ Checking required settings... ")
	cfg := mergeDoctorConfig(fileCfg, loadConfigFromViper())
	missing := []string{}

	if cfg.ServerURL == "" {
		missing = append(missing, "server URL (--server)")
	}
	if cfg.Token == "" {
		missing = append(missing, "token (--token)")
	}
	if cfg.MachineID == "" {
		missing = append(missing, "machine ID (--machine)")
	}

	if len(missing) > 0 {
		fmt.Println("✗")
		fmt.Printf("  Missing required settings:\n")
		for _, m := range missing {
			fmt.Printf("    - %s\n", m)
		}
		allPassed = false
	} else {
		fmt.Println("✓")
	}
	fmt.Println()

	// Check 3: Server connectivity
	fmt.Print("✓ Checking server connectivity... ")
	if cfg.ServerURL == "" {
		fmt.Println("⊘")
		fmt.Println("  Skipped: server URL not configured")
	} else {
		client := &http.Client{Timeout: 10 * time.Second}
		if cfg.Insecure {
			// Allow skipping TLS verification for insecure mode
			// Note: In production, you'd want to configure the transport properly
		}

		// Try to connect to the server
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, "GET", cfg.ServerURL+"/health", nil)
		if err != nil {
			fmt.Println("✗")
			fmt.Printf("  Error creating request: %v\n", err)
			allPassed = false
		} else {
			resp, err := client.Do(req)
			if err != nil {
				fmt.Println("✗")
				fmt.Printf("  Cannot connect to server: %v\n", err)
				if strings.HasPrefix(cfg.ServerURL, "http://") {
					fmt.Println("  Note: Using HTTP (insecure). Consider using HTTPS.")
				}
				allPassed = false
			} else {
				defer resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					fmt.Println("✓")
					fmt.Printf("  Server is reachable (status: %d)\n", resp.StatusCode)
				} else {
					fmt.Println("✗")
					fmt.Printf("  Server returned status: %d\n", resp.StatusCode)
					allPassed = false
				}
			}
		}
	}
	fmt.Println()

	// Check 4: Authentication
	fmt.Print("✓ Checking authentication... ")
	if cfg.Token == "" || cfg.ServerURL == "" {
		fmt.Println("⊘")
		fmt.Println("  Skipped: token or server not configured")
	} else {
		cl, err := client.New(cfg.ServerURL, cfg.Token, cfg.Insecure)
		if err != nil {
			fmt.Println("✗")
			fmt.Printf("  Error creating client: %v\n", err)
			allPassed = false
		} else {
			// Try a simple check-in to verify auth
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			report := discovery.Collect()
			_, err = cl.Checkin(ctx, types.CheckinRequest{
				MachineID:       cfg.MachineID,
				Hostname:        report.Hostname,
				Addresses:       report.Addresses,
				OS:              report.OS,
				CPUArch:         report.CPUArch,
				GPUVendor:       report.GPUVendor,
				HardwareSummary: report.HardwareSummary,
				AgentVersion:    agentVersion,
			})

			if err != nil {
				fmt.Println("✗")
				if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "403") {
					fmt.Println("  Authentication failed: invalid token or unauthorized")
				} else {
					fmt.Printf("  Error: %v\n", err)
				}
				allPassed = false
			} else {
				fmt.Println("✓")
				fmt.Println("  Authentication successful")
			}
		}
	}
	fmt.Println()

	// Check 5: Runtime availability
	fmt.Print("✓ Checking runtimes... ")
	manager := runtime.NewManager()
	installedRuntimes := manager.InstalledRuntimes()
	readyRuntimes := manager.ReadyRuntimes()

	if len(installedRuntimes) == 0 {
		fmt.Println("✗")
		fmt.Println("  No runtimes installed")
		fmt.Println("  Install a runtime: mantler runtime install <runtime>")
		fmt.Println("  Supported: ollama, llamacpp, vllm, tensorrt, quantcpp, mlxserver")
		allPassed = false
	} else {
		fmt.Println("✓")
		fmt.Printf("  Installed runtimes: %s\n", strings.Join(installedRuntimes, ", "))
		if len(readyRuntimes) > 0 {
			fmt.Printf("  Ready runtimes: %s\n", strings.Join(readyRuntimes, ", "))
		} else {
			fmt.Println("  Warning: No runtimes are ready")
		}
	}
	fmt.Println()

	// Check 6: System permissions
	fmt.Print("✓ Checking permissions... ")
	configDir := filepath.Dir(configPath)
	if strings.TrimSpace(configDir) == "" || configDir == "." {
		configDir = filepath.Dir(config.DefaultConfigPath())
	}

	// Check if config directory is writable
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		fmt.Println("✗")
		fmt.Printf("  Cannot create config directory: %v\n", err)
		allPassed = false
	} else {
		// Try to write a test file
		testFile := configDir + "/.write_test"
		if err := os.WriteFile(testFile, []byte("test"), 0o600); err != nil {
			fmt.Println("✗")
			fmt.Printf("  Cannot write to config directory: %v\n", err)
			allPassed = false
		} else {
			os.Remove(testFile)
			fmt.Println("✓")
			fmt.Printf("  Config directory is writable: %s\n", configDir)

			exePath, err := resolveExecutablePath()
			if err != nil {
				fmt.Printf("  Warning: cannot resolve executable path: %v\n", err)
			} else if err := ensureWritableTarget(exePath); err != nil {
				fmt.Printf("  Warning: self-update may fail: %v\n", err)
				allPassed = false
			} else {
				fmt.Printf("  Update target is writable: %s\n", filepath.Dir(exePath))
			}

			if runtimeDirErr := verifyWritableDir("/etc/mantler/runtimes"); runtimeDirErr != nil {
				fmt.Printf("  Warning: runtime state directory is not writable (/etc/mantler/runtimes): %v\n", runtimeDirErr)
				allPassed = false
			} else {
				fmt.Println("  Runtime state directory is writable: /etc/mantler/runtimes")
			}

			if cacheErr := verifyWritableDir("/var/cache/huggingface"); cacheErr != nil {
				fmt.Printf("  Warning: HuggingFace cache directory is not writable (/var/cache/huggingface): %v\n", cacheErr)
				allPassed = false
			} else {
				fmt.Println("  HuggingFace cache directory is writable: /var/cache/huggingface")
			}

			if groups, err := currentUserGroups(); err == nil {
				if !strings.Contains(groups, " docker ") {
					fmt.Println("  Warning: current user is not in docker group; Docker runtimes may fail.")
					allPassed = false
				} else {
					fmt.Println("  Docker group membership detected.")
				}
			} else {
				fmt.Printf("  Warning: unable to determine group membership: %v\n", err)
			}

			if _, err := exec.LookPath("nvidia-smi"); err != nil {
				fmt.Println("  Warning: nvidia-smi not found; GPU runtime checks are limited.")
			} else if err := exec.Command("nvidia-smi", "-L").Run(); err != nil {
				fmt.Printf("  Warning: nvidia-smi failed: %v\n", err)
				allPassed = false
			} else {
				fmt.Println("  NVIDIA driver appears healthy.")
			}
		}
	}
	fmt.Println()

	// Check 7: System information
	fmt.Print("✓ Checking system information... ")
	report := discovery.Collect()
	fmt.Println("✓")
	fmt.Printf("  Hostname: %s\n", report.Hostname)
	if len(report.Addresses) > 0 {
		fmt.Printf("  Addresses: %s\n", strings.Join(report.Addresses, ", "))
	}
	if report.HardwareSummary != "" {
		fmt.Printf("  Hardware: %s\n", report.HardwareSummary)
	}
	fmt.Println()

	// Summary
	fmt.Println("================================")
	if allPassed {
		fmt.Println("✓ All checks passed!")
		fmt.Println()
		fmt.Println("The agent is ready to run.")
		fmt.Println("Start with: mantler start")
	} else {
		fmt.Println("✗ Some checks failed")
		fmt.Println()
		fmt.Println("Fix the issues above and run 'mantler doctor' again.")
	}
}

func loadConfigFromViper() config.Config {
	intervalDuration, _ := time.ParseDuration(viper.GetString("interval"))
	if intervalDuration == 0 {
		intervalDuration = 30 * time.Second
	}

	return config.Config{
		ServerURL: viper.GetString("server"),
		Token:     viper.GetString("token"),
		MachineID: viper.GetString("machine"),
		Interval:  intervalDuration,
		Insecure:  viper.GetBool("insecure"),
		LogLevel:  viper.GetString("log-level"),
	}
}

func mergeDoctorConfig(fileCfg config.Config, viperCfg config.Config) config.Config {
	merged := fileCfg
	if viperCfg.ServerURL != "" {
		merged.ServerURL = viperCfg.ServerURL
	}
	if viperCfg.Token != "" {
		merged.Token = viperCfg.Token
	}
	if viperCfg.MachineID != "" {
		merged.MachineID = viperCfg.MachineID
	}
	if viperCfg.Insecure {
		merged.Insecure = true
	}
	return merged
}

func maskURL(raw string) string {
	if raw == "" {
		return "(not set)"
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "(invalid URL)"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	if parsed.Path != "" && parsed.Path != "/" {
		parsed.Path = "/..."
	}
	return parsed.String()
}

func verifyWritableDir(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if info.Mode().Perm()&0o200 == 0 {
		// Fast-fail with a clearer error before write probe.
		return fmt.Errorf("%s is not writable", path)
	}
	testFile := filepath.Join(path, ".doctor-write-test")
	if err := os.WriteFile(testFile, []byte("ok"), 0o600); err != nil {
		return err
	}
	_ = os.Remove(testFile)
	return nil
}

func currentUserGroups() (string, error) {
	out, err := exec.Command("id", "-nG").Output()
	if err != nil {
		return "", err
	}
	normalized := " " + strings.TrimSpace(string(out)) + " "
	return normalized, nil
}
