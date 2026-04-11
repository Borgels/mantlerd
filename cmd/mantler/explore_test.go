package main

import "testing"

func TestSanitizeRuntimePlanPath(t *testing.T) {
	valid, err := sanitizeRuntimePlanPath("/etc/mantler/runtime.conf")
	if err != nil {
		t.Fatalf("expected /etc/mantler path to be valid: %v", err)
	}
	if valid != "/etc/mantler/runtime.conf" {
		t.Fatalf("unexpected sanitized path: %s", valid)
	}

	if _, err := sanitizeRuntimePlanPath("relative/path"); err == nil {
		t.Fatalf("expected relative path to fail")
	}
	if _, err := sanitizeRuntimePlanPath("/tmp/outside"); err == nil {
		t.Fatalf("expected path outside allowed roots to fail")
	}
}
