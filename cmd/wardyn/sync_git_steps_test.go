// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/foldersync"
)

// A refused history does not stop --pull-commits, which needs no send; the refusal still comes back after it.
func TestRunGitSteps_ARefusedSendStillPullsThenReturnsTheRefusal(t *testing.T) {
	refusal := &foldersync.HistoryDeniedError{Branch: "b", Path: "old.pem", Pattern: "*.pem"}
	other := errors.New("sandbox unreachable")
	for _, c := range []struct {
		name       string
		send, pull error
		noSend     bool
		noPull     bool
		wantErr    error
		wantPulled bool
	}{
		{"send refused, pull ok", fmt.Errorf("wrapped: %w", refusal), nil, false, false, refusal, true},
		{"send refused, pull fails", refusal, other, false, false, other, true},
		{"send fails otherwise: no pull", other, nil, false, false, other, false},
		{"send ok, pull ok", nil, nil, false, false, nil, true},
		{"send refused, no --pull-commits", refusal, nil, false, true, refusal, false},
		{"--pull-commits alone", nil, nil, true, false, nil, true},
	} {
		pulled := false
		send, pull := func() error { return c.send }, func() error { pulled = true; return c.pull }
		if c.noSend {
			send = nil
		}
		if c.noPull {
			pull = nil
		}
		err := runGitSteps(send, pull)
		if !errors.Is(err, c.wantErr) && err != c.wantErr || pulled != c.wantPulled {
			t.Errorf("%s: err %v pulled %v; want %v and %v", c.name, err, pulled, c.wantErr, c.wantPulled)
		}
	}
}

// Under --watch a refused history is a warning and the loop goes on; without it, and for any other error, the
// error stops the command.
func TestKeepWatchingAfterRefusal(t *testing.T) {
	refusal := &foldersync.HistoryDeniedError{Branch: "b", Path: "old.pem", Pattern: "*.pem"}
	other := errors.New("sandbox unreachable")
	for _, c := range []struct {
		name     string
		watch    bool
		err      error
		wantErr  error
		wantWarn bool
	}{
		{"watch, refused", true, refusal, nil, true},
		{"no watch, refused", false, refusal, refusal, false},
		{"watch, other error", true, other, other, false},
		{"watch, no error", true, nil, nil, false},
	} {
		var w bytes.Buffer
		err := keepWatchingAfterRefusal(&w, c.watch, c.err)
		if err != c.wantErr || strings.Contains(w.String(), "old.pem") != c.wantWarn {
			t.Errorf("%s: err %v warning %q; want %v and warning=%v", c.name, err, w.String(), c.wantErr, c.wantWarn)
		}
	}
}
