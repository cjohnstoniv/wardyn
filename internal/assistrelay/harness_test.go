// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistrelay

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// builtinTools are the harness's own tool names; none may appear in a command line.
var builtinTools = []string{"Agent", "Bash", "Edit", "Glob", "Grep", "LS", "MultiEdit", "NotebookEdit", "Read",
	"Skill", "Task", "TodoWrite", "ToolSearch", "WebFetch", "WebSearch", "Workflow", "Write"}

func checkCommonFlags(t *testing.T, argv []string) {
	t.Helper()
	joined := " " + strings.Join(argv, " ") + " "
	for i, a := range argv {
		if a == "--tools" && (i+1 >= len(argv) || argv[i+1] != "") {
			t.Fatalf("--tools is not followed by an empty value: %q", argv)
		}
		if a == "--allowedTools" && (i+1 >= len(argv) || argv[i+1] != "mcp__wardyn__*") {
			t.Fatalf("--allowedTools value: %q", argv)
		}
		for _, b := range builtinTools {
			if a == b {
				t.Fatalf("argv names the built-in tool %q", b)
			}
		}
	}
	for _, flag := range []string{" --tools ", " --strict-mcp-config ", " --disable-slash-commands ", " --allowedTools ", " --verbose "} {
		if !strings.Contains(joined, flag) {
			t.Fatalf("argv lacks %s: %q", flag, argv)
		}
	}
}

func TestStreamArgs(t *testing.T) {
	got := StreamArgs("claude", "stub-model", "the prompt", "/ah/s/1/mcp.json")
	want := []string{"claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", "stub-model", "--system-prompt", "the prompt", "--tools", "", "--mcp-config", "/ah/s/1/mcp.json",
		"--strict-mcp-config", "--disable-slash-commands", "--allowedTools", "mcp__wardyn__*"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stream argv = %q\nwant %q", got, want)
	}
	checkCommonFlags(t, got)
}

func TestResumeArgs(t *testing.T) {
	base := []string{"claude", "-p", "--output-format", "stream-json", "--verbose",
		"--model", "stub-model", "--system-prompt", "the prompt", "--tools", "", "--mcp-config", "/ah/s/1/mcp.json",
		"--strict-mcp-config", "--disable-slash-commands", "--allowedTools", "mcp__wardyn__*"}
	first := ResumeArgs("claude", "stub-model", "the prompt", "/ah/s/1/mcp.json", "u-1", false)
	if want := append(append([]string(nil), base...), "--session-id", "u-1"); !reflect.DeepEqual(first, want) {
		t.Fatalf("first turn argv = %q\nwant %q", first, want)
	}
	later := ResumeArgs("claude", "stub-model", "the prompt", "/ah/s/1/mcp.json", "u-1", true)
	if want := append(append([]string(nil), base...), "--resume", "u-1"); !reflect.DeepEqual(later, want) {
		t.Fatalf("later turn argv = %q\nwant %q", later, want)
	}
	checkCommonFlags(t, first)
	checkCommonFlags(t, later)
}

// groupGone reports whether no process is left in group pgid.
func groupGone(pgid int) bool {
	return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

// TestExecLauncherStopsTheWholeTree starts a harness stand-in that ignores SIGTERM and leaves a child in its
// group, as the harness leaves its MCP server. Stop returns only once both are gone.
func TestExecLauncherStopsTheWholeTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child")
	script := `trap "" TERM; sleep 30 & echo $! > ` + pidFile + `; echo started; wait`
	proc, err := ExecLauncher{Grace: 300 * time.Millisecond}.Start(context.Background(),
		[]string{"/bin/sh", "-c", script}, os.Environ(), t.TempDir(), strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("started\n"))
	if _, err := io.ReadFull(proc.Stdout(), buf); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	child, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	pgid := proc.(*execProc).cmd.Process.Pid
	if gid, err := syscall.Getpgid(child); err != nil || gid != pgid {
		t.Fatalf("child group %d (%v), want the harness's own group %d", gid, err, pgid)
	}
	if err := proc.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !groupGone(pgid) {
		t.Fatal("Stop returned with a process of the tree still alive")
	}
	if err := proc.Stop(); err != nil {
		t.Fatalf("a second Stop: %v", err)
	}
}
