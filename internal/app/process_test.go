package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Exercise the subprocess and JSONL boundary, not just the JSON decoder.
func TestCLIRunnerRequiresCompleteTurn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell fixture; production Windows build checked separately")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cases := []struct {
		name, body string
		wantError  bool
	}{
		{"complete", "echo '{\"type\":\"thread.started\",\"thread_id\":\"session-test\"}'\necho '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"Working\"}}'\necho '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"Final document\"}}'\necho '{\"type\":\"turn.completed\"}'\n", false},
		{"truncated", "echo '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"Partial answer\"}}'\n", true},
		{"failure", "echo 'PRIVATE_CREDENTIAL' >&2\nexit 1\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\n"+tc.body), 0700); err != nil {
				t.Fatal(err)
			}
			result, err := (CLIRunner{}).Run(context.Background(), AgentInput{Agent: "codex", Directory: dir, Prompt: "test"}, func(string, string) {})
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE_CREDENTIAL") {
				t.Fatal("raw stderr exposed")
			}
			if !tc.wantError && (result.Text != "Final document" || result.Session != "session-test") {
				t.Fatalf("bad result: %+v", result)
			}
		})
	}
}

func TestCLIRunnerCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell fixture")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := (CLIRunner{}).Run(ctx, AgentInput{Agent: "codex", Directory: dir, Prompt: "test"}, func(string, string) {})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("process did not stop promptly: %v", err)
	}
}
