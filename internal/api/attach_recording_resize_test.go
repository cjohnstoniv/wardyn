// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A resize of a recorded session's PTY lands in its cast as an asciicast v2
// "r" event, after the output teed before it and before the output after it,
// so the replay redraws at the new size where the terminal did.
func TestNewSessionRecorder_RecordsResizesInOrder(t *testing.T) {
	store, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	srv := New(Config{RecordingStore: store, Audit: &recRecorder{}, AdminToken: adminToken})
	runID := uuid.New()
	tee, finish := srv.newSessionRecorder(types.AgentRun{ID: runID}, "resize", runner.AttachOptions{Cols: 80, Rows: 24})
	h := &attachHolder{}
	h.keepCast(tee)

	_, _ = tee.Write([]byte("before\r\n"))
	h.recordResize(132, 43)
	_, _ = tee.Write([]byte("after\r\n"))
	(*attachHolder)(nil).recordResize(1, 1) // a lane with no holder records nothing, and does not panic
	finish(context.Background(), types.ActorHuman, "alice@example.com")

	rc, err := store.OpenCast(context.Background(), recording.CastKey(runID.String(), "resize"))
	if err != nil {
		t.Fatalf("OpenCast: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	var got []string
	for _, ln := range strings.Split(strings.TrimSpace(string(body)), "\n")[1:] {
		var ev []any
		if err := json.Unmarshal([]byte(ln), &ev); err != nil || len(ev) != 3 {
			t.Fatalf("event %q: %v", ln, err)
		}
		got = append(got, ev[1].(string)+":"+ev[2].(string))
	}
	if want := "o:before\r\n|r:132x43|o:after\r\n"; strings.Join(got, "|") != want {
		t.Fatalf("cast events = %q, want %q", strings.Join(got, "|"), want)
	}
}
