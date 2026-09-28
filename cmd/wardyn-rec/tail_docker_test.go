// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// tailInContainerEnv switches TestTailUpload_RealAsciinema to its in-container
// half: the test binary re-runs itself inside the agent image, where the real
// asciinema is.
const tailInContainerEnv = "WARDYN_REC_TAIL_IN_CONTAINER"

// tailAgentImage carries the asciinema wardyn-rec execs; nightly's
// docker-tagged-live job builds it.
const tailAgentImage = "wardyn/agent-claude-code:local"

// tailJoinedMarker is printed by the in-container half once the join matched,
// so a -test.run that matches nothing (exit 0, no test run) cannot pass.
const tailJoinedMarker = "tail-upload-joined-parts:"

// TestTailUpload_RealAsciinema cuts a real asciinema cast into parts at a
// small part size and a short interval, while it records (the agent waits for
// the first stored part before its last line), with one part's connection dropped
// mid-body and another's answer lost, and stores them through the control
// plane's own store and join (recording.FSStore, recording.OpenJoined). The
// joined cast must equal the cast file byte for byte: every part boundary
// falls on a line asciinema really wrote, and no event is lost or repeated.
func TestTailUpload_RealAsciinema(t *testing.T) {
	if os.Getenv(tailInContainerEnv) == "1" {
		tailUploadInContainer(t)
		return
	}
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping the real-asciinema tail upload")
	}
	bin := filepath.Join(t.TempDir(), "wardyn-rec.test")
	build := exec.Command("go", "test", "-c", "-tags", "docker", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the test binary: %v\n%s", err, out)
	}
	out, err := exec.Command("docker", "run", "--rm", "--network", "none",
		"-v", bin+":/tmp/wardyn-rec.test:ro", "-e", tailInContainerEnv+"=1",
		"--entrypoint", "/tmp/wardyn-rec.test", tailAgentImage,
		"-test.run", "^TestTailUpload_RealAsciinema$", "-test.v", "-test.count=1").CombinedOutput()
	t.Logf("in-container run:\n%s", out)
	if err != nil {
		t.Fatalf("in-container run: %v", err)
	}
	if !strings.Contains(string(out), tailJoinedMarker) {
		t.Fatal("the in-container half never reached its join check")
	}
}

func tailUploadInContainer(t *testing.T) {
	if _, err := exec.LookPath("asciinema"); err != nil {
		t.Fatalf("no asciinema in %s: %v", tailAgentImage, err)
	}
	shrinkParts(t, 700)
	origInterval, origRetry := partInterval, tailRetry
	partInterval, tailRetry = 150*time.Millisecond, 0
	t.Cleanup(func() { partInterval, tailRetry = origInterval, origRetry })

	store, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-real-asciinema"
	castDir := t.TempDir()
	stored := filepath.Join(castDir, "a-part-is-stored")
	var (
		mu       sync.Mutex
		attempts = map[string]int{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts[r.URL.Path]++
		first := attempts[r.URL.Path] == 1
		mu.Unlock()
		if first && r.URL.Path == "/rec/parts/3" {
			_, _ = io.ReadFull(r.Body, make([]byte, r.ContentLength/2))
			dropConn(t, w)
			return
		}
		var err error
		if r.URL.Path == "/rec" {
			err = store.SaveCast(r.Context(), runID, r.Body)
		} else {
			n, ok := strings.CutPrefix(r.URL.Path, "/rec/parts/")
			part, perr := strconv.Atoi(n)
			if !ok || perr != nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			err = store.SaveCastNamed(r.Context(), runID, recording.PartSuffix(part), r.Body)
		}
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = os.WriteFile(stored, nil, 0o600)
		if first && r.URL.Path == "/rec/parts/5" {
			dropConn(t, w)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	agent := `for i in $(seq 1 120); do printf 'line %03d h\303\251llo w\303\266rld\n' "$i"; sleep 0.02; done
n=0; while [ ! -f '` + stored + `' ] && [ $n -lt 200 ]; do sleep 0.05; n=$((n+1)); done
[ -f '` + stored + `' ] && echo stored-while-recording`
	if err := run([]string{
		"-cast-dir", castDir, "-run", runID, "-upload-url", srv.URL + "/rec", "--", "sh", "-c", agent,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	file, err := os.ReadFile(castFile(castDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := recording.OpenJoined(context.Background(), store, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	joined, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(file), "stored-while-recording") {
		t.Fatal("no part was stored while asciinema was recording")
	}
	if string(joined) != string(file) {
		t.Fatalf("joined parts (%d bytes) differ from the cast file (%d bytes)\njoined:\n%s\nfile:\n%s",
			len(joined), len(file), joined, file)
	}
	mu.Lock()
	defer mu.Unlock()
	parts := len(attempts)
	for n := 1; n <= parts; n++ {
		key := runID
		if n > 1 {
			key = recording.CastKey(runID, recording.PartSuffix(n))
		}
		rc, err := store.OpenCast(context.Background(), key)
		if err != nil {
			t.Fatalf("part %d: %v", n, err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		for i, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
			if !json.Valid([]byte(l)) {
				t.Fatalf("part %d line %d is not a whole cast line: %q", n, i+1, l)
			}
		}
	}
	if parts < 6 || attempts["/rec/parts/3"] != 2 || attempts["/rec/parts/5"] != 2 {
		t.Fatalf("parts %d, attempts %v; want at least 6 parts, parts 3 and 5 each sent twice", parts, attempts)
	}
	fmt.Println(tailJoinedMarker, parts, "parts,", len(file), "bytes")
}
