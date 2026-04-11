package main

import "testing"

func TestMaskURL(t *testing.T) {
	if got := maskURL(""); got != "(not set)" {
		t.Fatalf("expected empty URL to be masked as not set, got %q", got)
	}

	got := maskURL("https://user:secret@example.com/path?q=abc#frag")
	want := "https://example.com/..."
	if got != want {
		t.Fatalf("expected masked URL %q, got %q", want, got)
	}

	if got := maskURL("://bad-url"); got != "(invalid URL)" {
		t.Fatalf("expected invalid URL marker, got %q", got)
	}
}
