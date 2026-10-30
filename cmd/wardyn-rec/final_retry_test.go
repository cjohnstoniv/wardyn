// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// exitRefusalServer refuses the first refusals PUTs as refuse says (a status
// code, or 0 to drop the connection unanswered), then stores the body.
type exitRefusalServer struct {
	mu       sync.Mutex
	attempts int
	stored   string
	srv      *httptest.Server
}

func newExitRefusalServer(t *testing.T, refusals, refuse int) *exitRefusalServer {
	es := &exitRefusalServer{}
	es.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		es.mu.Lock()
		es.attempts++
		n := es.attempts
		if n > refusals {
			es.stored = string(b)
		}
		es.mu.Unlock()
		switch {
		case n > refusals:
			w.WriteHeader(http.StatusNoContent)
		case refuse == 0:
			dropConn(t, w)
		default:
			w.WriteHeader(refuse)
		}
	}))
	t.Cleanup(es.srv.Close)
	return es
}

// shortRunArgs is a short command run's recorder: under a stub asciinema that
// records the command's output as one event, or under the plain log when cast
// is false.
func shortRunArgs(t *testing.T, url string, cast bool) (args []string, recording string) {
	dir := t.TempDir()
	stub := "definitely-not-a-real-binary-xyz"
	recording = filepath.Join(dir, "run-short.log")
	if cast {
		stub = filepath.Join(dir, "asciinema")
		script := "#!/bin/sh\n# rec -q -c <cmd> <cast>\nout=$(sh -c \"$4\")\nprintf '%s' '" + tailHeader +
			"' > \"$5\"\nprintf '[0.1,\"o\",\"%s\"]\\n' \"$out\" >> \"$5\"\n"
		if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		recording = filepath.Join(dir, "run-short.cast")
	}
	return []string{"-cast-dir", dir, "-run", "run-short", "-asciinema", stub,
		"-upload-url", url + "/rec", "--", "echo", "short-run-marker"}, recording
}

func fastFinalRetry(t *testing.T) {
	orig := finalRetryWait
	finalRetryWait = 10 * time.Millisecond
	t.Cleanup(func() { finalRetryWait = orig })
}

// TestRun_ShortRunDeliversThroughARefusalThatMayPass: a run that ends before its
// first part has one delivery, at exit. A refusal there that may pass (the run's
// proxy unable to record the request yet, the relay to the organisation down, no
// answer at all) is retried, so the command's output is stored as its recording.
func TestRun_ShortRunDeliversThroughARefusalThatMayPass(t *testing.T) {
	fastFinalRetry(t)
	for _, c := range []struct {
		name   string
		cast   bool
		refuse int
	}{
		{"cast after 503", true, http.StatusServiceUnavailable},
		{"cast after 502", true, http.StatusBadGateway},
		{"cast after no answer", true, 0},
		{"log after 503", false, http.StatusServiceUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			es := newExitRefusalServer(t, 2, c.refuse)
			args, recording := shortRunArgs(t, es.srv.URL, c.cast)
			if err := run(args); err != nil {
				t.Fatalf("run: %v", err)
			}
			file, err := os.ReadFile(recording)
			if err != nil {
				t.Fatal(err)
			}
			es.mu.Lock()
			defer es.mu.Unlock()
			if es.attempts != 3 || es.stored != string(file) || !strings.Contains(es.stored, "short-run-marker") {
				t.Fatalf("attempts %d, stored %q; want the recording %q stored on the third attempt", es.attempts, es.stored, file)
			}
		})
	}
}

// TestRun_ShortRunDoesNotRetryAFinalRefusal: a refusal that answers the same
// again (here the organisation keeping no recordings) is not retried, so the
// run's exit is not held for it.
func TestRun_ShortRunDoesNotRetryAFinalRefusal(t *testing.T) {
	fastFinalRetry(t)
	es := newExitRefusalServer(t, 1, http.StatusConflict)
	args, _ := shortRunArgs(t, es.srv.URL, true)
	if err := run(args); err != nil {
		t.Fatalf("a refused recording must stay non-fatal: %v", err)
	}
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.attempts != 1 {
		t.Fatalf("attempts %d, want 1", es.attempts)
	}
}
