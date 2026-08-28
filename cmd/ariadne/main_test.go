package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Imported bash history has no timestamps. The synthetic spread must place
// earlier lines further in the past and the last line near now — otherwise
// every imported command ranks at maximum decay no matter when it ran.
func TestParseHistorySpread(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".bash_history")
	lines := []string{
		"old command one",
		"middle command",
		"recent command three",
	}
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o600); err != nil {
		t.Fatal(err)
	}

	evs, err := parseHistory(path, "testhost")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != len(lines) {
		t.Fatalf("got %d events, want %d", len(evs), len(lines))
	}

	now := time.Now().Unix()
	window := int64(90 * 24 * 3600)
	// The oldest line must be within the window's tail, the newest near now.
	if evs[0].TS > now-window/2 {
		t.Errorf("first line ts=%d, want > %d seconds old (window tail)", evs[0].TS, window/2)
	}
	if evs[len(evs)-1].TS < now-24*3600 {
		t.Errorf("last line ts=%d, want within the last day", evs[len(evs)-1].TS)
	}
	// Monotonic: later lines are never older than earlier ones.
	for i := 1; i < len(evs); i++ {
		if evs[i].TS < evs[i-1].TS {
			t.Errorf("line %d ts=%d older than line %d ts=%d", i, evs[i].TS, i-1, evs[i-1].TS)
		}
	}
}

// zsh extended-history timestamps are real and must be preserved untouched.
func TestParseHistoryZshTimestamps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".zsh_history")
	old := time.Now().Add(-10 * 24 * time.Hour).Unix()
	content := ": " + strconv.FormatInt(old, 10) + ":0;ls -la\n: " + strconv.FormatInt(time.Now().Unix(), 10) + ":1;git status\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	evs, err := parseHistory(path, "testhost")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}
	if evs[0].TS != old {
		t.Errorf("zsh ts not preserved: got %d, want %d", evs[0].TS, old)
	}
	if evs[1].Raw != "git status" {
		t.Errorf("second command = %q, want git status", evs[1].Raw)
	}
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}
