package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type launchdServiceManager struct{}

func (m *launchdServiceManager) Install(name string, execStart string, env map[string]string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	label := launchdLabel(name)
	plistPath := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("create launch agents directory: %w", err)
	}
	logDir := filepath.Join(home, ".mantler")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	args := splitCommandArgs(execStart)
	if len(args) == 0 {
		return fmt.Errorf("invalid launchd execStart")
	}

	plist := []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`,
		`<plist version="1.0">`,
		`<dict>`,
		`  <key>Label</key>`,
		`  <string>` + xmlEscape(label) + `</string>`,
		`  <key>ProgramArguments</key>`,
		`  <array>`,
	}
	for _, arg := range args {
		plist = append(plist, `    <string>`+xmlEscape(arg)+`</string>`)
	}
	plist = append(plist,
		`  </array>`,
		`  <key>RunAtLoad</key>`,
		`  <true/>`,
		`  <key>KeepAlive</key>`,
		`  <true/>`,
	)
	if len(env) > 0 {
		plist = append(plist, `  <key>EnvironmentVariables</key>`, `  <dict>`)
		for key, value := range env {
			plist = append(plist, `    <key>`+xmlEscape(key)+`</key>`)
			plist = append(plist, `    <string>`+xmlEscape(value)+`</string>`)
		}
		plist = append(plist, `  </dict>`)
	}
	plist = append(plist,
		`  <key>StandardOutPath</key>`,
		`  <string>`+xmlEscape(filepath.Join(logDir, name+".log"))+`</string>`,
		`  <key>StandardErrorPath</key>`,
		`  <string>`+xmlEscape(filepath.Join(logDir, name+".err.log"))+`</string>`,
		`</dict>`,
		`</plist>`,
		"",
	)
	if err := os.WriteFile(plistPath, []byte(strings.Join(plist, "\n")), 0o644); err != nil {
		return fmt.Errorf("write launchd plist: %w", err)
	}
	return nil
}

func (m *launchdServiceManager) Start(name string) error {
	label := launchdLabel(name)
	plistPath, err := launchdPlistPath(name)
	if err != nil {
		return err
	}
	uid := fmt.Sprintf("%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", "gui/"+uid, plistPath).Run()
	if output, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap %s: %w (%s)", label, err, strings.TrimSpace(string(output)))
	}
	_ = exec.Command("launchctl", "enable", "gui/"+uid+"/"+label).Run()
	return nil
}

func (m *launchdServiceManager) Stop(name string) error {
	plistPath, err := launchdPlistPath(name)
	if err != nil {
		return err
	}
	uid := fmt.Sprintf("%d", os.Getuid())
	_ = exec.Command("launchctl", "disable", "gui/"+uid+"/"+launchdLabel(name)).Run()
	if output, err := exec.Command("launchctl", "bootout", "gui/"+uid, plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootout %s: %w (%s)", name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (m *launchdServiceManager) Restart(name string) error {
	label := launchdLabel(name)
	uid := fmt.Sprintf("%d", os.Getuid())
	if output, err := exec.Command("launchctl", "kickstart", "-k", "gui/"+uid+"/"+label).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart %s: %w (%s)", label, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (m *launchdServiceManager) IsActive(name string) (bool, error) {
	label := launchdLabel(name)
	uid := fmt.Sprintf("%d", os.Getuid())
	cmd := exec.Command("launchctl", "print", "gui/"+uid+"/"+label)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(string(output)) != "", nil
}

func (m *launchdServiceManager) Uninstall(name string) error {
	_ = m.Stop(name)
	plistPath, err := launchdPlistPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove launchd plist: %w", err)
	}
	return nil
}

func (m *launchdServiceManager) Logs(name string, _ int) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	logPath := filepath.Join(home, ".mantler", name+".err.log")
	content, readErr := os.ReadFile(logPath)
	if readErr != nil {
		return "", readErr
	}
	return string(content), nil
}

func launchdLabel(name string) string {
	return "com.mantler." + strings.TrimSpace(name)
}

func launchdPlistPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel(name)+".plist"), nil
}

func splitCommandArgs(value string) []string {
	parts := strings.Fields(strings.TrimSpace(value))
	if len(parts) == 0 {
		return nil
	}
	return parts
}

func xmlEscape(value string) string {
	replaced := strings.ReplaceAll(value, "&", "&amp;")
	replaced = strings.ReplaceAll(replaced, "<", "&lt;")
	replaced = strings.ReplaceAll(replaced, ">", "&gt;")
	replaced = strings.ReplaceAll(replaced, `"`, "&quot;")
	return strings.ReplaceAll(replaced, "'", "&apos;")
}
