//go:build !linux

package localsocket

func chownSocket(_ string) {}
