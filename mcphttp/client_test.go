package mcphttp

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCapOutput(t *testing.T) {
	short := "hello"
	if got := capOutput(short); got != short {
		t.Fatalf("short text changed: %q", got)
	}
	long := strings.Repeat("界", maxToolOutput) // 3 bytes per rune
	got := capOutput(long)
	if !strings.Contains(got, "[output truncated:") {
		t.Fatal("missing truncation note")
	}
	body := got[:strings.Index(got, "\n\n[output truncated:")]
	if len(body) > maxToolOutput {
		t.Fatalf("body %d bytes exceeds cap %d", len(body), maxToolOutput)
	}
	if !utf8.ValidString(body) {
		t.Fatal("truncation split a UTF-8 rune")
	}
}
