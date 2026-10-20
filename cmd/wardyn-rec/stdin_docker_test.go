// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// stdinInContainerEnv switches TestRec_TypedInputIsNotRecorded to its in-container half;
// stdinPaneEnv to the half that runs wardyn-rec inside the tmux pane.
const (
	stdinInContainerEnv = "WARDYN_REC_STDIN_IN_CONTAINER"
	stdinPaneEnv        = "WARDYN_REC_STDIN_PANE"
	stdinTyped          = "typed-into-the-pane-7f3a"
	stdinDoneMarker     = "typed-input-not-recorded:"
)

// TestRec_TypedInputIsNotRecorded runs wardyn-rec under the real asciinema in a tmux session named
// wardyn, as the boot seed does, and types a marker into the pane the way an attached client does,
// at a no-echo prompt. The agent must receive it, and the cast must not hold it.
func TestRec_TypedInputIsNotRecorded(t *testing.T) {
	switch {
	case os.Getenv(stdinPaneEnv) == "1":
		stdinPane(t)
		return
	case os.Getenv(stdinInContainerEnv) == "1":
		stdinInContainer(t)
		return
	}
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping the real-asciinema typed-input check")
	}
	bin := filepath.Join(t.TempDir(), "wardyn-rec.test")
	build := exec.Command("go", "test", "-c", "-tags", "docker", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the test binary: %v\n%s", err, out)
	}
	out, err := exec.Command("docker", "run", "--rm", "--network", "none",
		"-v", bin+":/tmp/wardyn-rec.test:ro", "-e", stdinInContainerEnv+"=1",
		"--entrypoint", "/tmp/wardyn-rec.test", tailAgentImage,
		"-test.run", "^TestRec_TypedInputIsNotRecorded$", "-test.v", "-test.count=1").CombinedOutput()
	t.Logf("in-container run:\n%s", out)
	if err != nil {
		t.Fatalf("in-container run: %v", err)
	}
	if !strings.Contains(string(out), stdinDoneMarker) {
		t.Fatal("the in-container half never reached its cast check")
	}
}

// stdinInContainer starts the pane, types into it, and reads the cast.
func stdinInContainer(t *testing.T) {
	for _, tool := range []string{"asciinema", "tmux"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("no %s in %s: %v", tool, tailAgentImage, err)
		}
	}
	castDir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pane := fmt.Sprintf("%s=1 WARDYN_REC_STDIN_CAST_DIR=%s %s -test.run '^TestRec_TypedInputIsNotRecorded$'",
		stdinPaneEnv, castDir, self)
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", "wardyn", "-x", "100", "-y", "30", pane+"; touch "+filepath.Join(castDir, "pane-done")+"; sleep 60").CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-server").Run() })
	ready := filepath.Join(castDir, "prompting")
	waitForFile(t, ready)
	time.Sleep(500 * time.Millisecond) // the prompt is up; give read(1) its terminal
	if out, err := exec.Command("tmux", "send-keys", "-t", "wardyn", stdinTyped, "Enter").CombinedOutput(); err != nil {
		t.Fatalf("tmux send-keys: %v\n%s", err, out)
	}
	waitForFile(t, filepath.Join(castDir, "pane-done")) // wardyn-rec returned, so asciinema has closed the cast
	cast, err := os.ReadFile(castFile(castDir, "run-typed-input"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cast), fmt.Sprintf("agent-read-%d-chars", len(stdinTyped))) {
		t.Fatalf("the agent never got what was typed; cast:\n%s", cast)
	}
	if strings.Contains(string(cast), stdinTyped) {
		t.Fatalf("what was typed into the pane is in the cast:\n%s", cast)
	}
	fmt.Println(stdinDoneMarker, len(cast), "bytes")
}

// stdinPane is wardyn-rec in the pane: the agent waits at a no-echo prompt and says how much it read.
func stdinPane(t *testing.T) {
	dir := os.Getenv("WARDYN_REC_STDIN_CAST_DIR")
	agent := `touch '` + filepath.Join(dir, "prompting") + `'; read -r -s p; echo; echo "agent-read-${#p}-chars"`
	if err := run([]string{"-cast-dir", dir, "-run", "run-typed-input", "--", "bash", "-c", agent}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}
