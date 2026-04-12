package tools

import "errors"

type DiagnosticResult struct {
	MemoryBandwidthGBps float64
	Detail              string
	Source              string
}

var ErrNotImplemented = errors.New("tool lifecycle automation not implemented")

type Driver interface {
	Name() string
	Install() error
	Uninstall() error
	IsInstalled() bool
	IsReady() bool
	Version() string
	RunDiagnostic(level string) (DiagnosticResult, error)
	Configure(config map[string]any) error
}
