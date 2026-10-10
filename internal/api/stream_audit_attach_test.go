// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func assertRunnerAudit(t *testing.T, ev types.AuditEvent, relay, principal string) {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["relay"] != relay || ev.Actor != principal {
		t.Fatalf("event=%+v data=%s", ev, ev.Data)
	}
}

func TestWebAttachAuditTagsReadyAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "ready"
		if fail {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			rn := &scriptedRunner{attach: func(context.Context, int, runner.AttachOptions) (runner.Session, error) {
				if fail {
					return nil, io.ErrClosedPipe
				}
				return newCountingShellSession(), nil
			}}
			srv, rec, run := scriptedServer(t, rn)
			relay := "runner:" + uuid.NewString()
			run.SandboxRef = relay + "/sandbox"
			st := srv.cfg.Store.(*touchCountingStore)
			st.mu.Lock()
			st.runs[run.ID] = run
			st.mu.Unlock()
			ts := httptest.NewServer(panicFails(t, srv.Handler()))
			defer ts.Close()
			conn := dialAttach(t, ts, srv, run.ID, holderOwner, "")
			readAttachMode(t, conn)
			if fail {
				_, _, _ = conn.Read(t.Context())
			}
			waitFor(t, "web attach outcome audit", func() bool {
				for _, ev := range rec.snapshot() {
					if ev.Action != "session.attach" {
						continue
					}
					var data map[string]any
					if json.Unmarshal(ev.Data, &data) != nil {
						continue
					}
					if (fail && ev.Outcome == "failure") || (!fail && data["state"] == "ready") {
						assertRunnerAudit(t, ev, relay, holderOwner)
						return true
					}
				}
				return false
			})
			_ = conn.CloseNow()
		})
	}
}

func TestAttachLifecycleAuditKeepsRunnerOnPromotionTakeoverAndRecording(t *testing.T) {
	rec := &sshTestRecorder{}
	casts, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{Audit: rec, RecordingStore: casts})
	relay := "runner:" + uuid.NewString()
	run := types.AgentRun{ID: uuid.New(), SandboxRef: relay + "/sandbox"}
	writer := &attachHolder{principal: "alice", actorType: types.ActorHuman, sandboxRef: run.SandboxRef}
	observer := &attachHolder{principal: "alice", actorType: types.ActorHuman, sandboxRef: run.SandboxRef}
	_, release := srv.registerAttachHolder(run.ID, writer)
	_, releaseObserver := srv.registerAttachHolder(run.ID, observer)
	defer releaseAttach(releaseObserver)
	if announce := release(); announce != nil {
		announce()
	} else {
		t.Fatal("observer was not promoted")
	}
	srv.recordTakeover(t.Context(), run.ID, run.SandboxRef, types.ActorHuman, "alice", writer, "")
	tee, finish := srv.newSessionRecorder(run, "session", runner.AttachOptions{})
	if tee == nil {
		t.Fatal("missing recorder")
	}
	if _, err = tee.Write([]byte("safe output\n")); err != nil {
		t.Fatal(err)
	}
	finish(t.Context(), types.ActorHuman, "alice")
	seen := map[string]bool{}
	for _, ev := range rec.snapshot() {
		assertRunnerAudit(t, ev, relay, "alice")
		seen[ev.Action] = true
	}
	for _, action := range []string{"session.promote", "session.takeover", "session.recording.write"} {
		if !seen[action] {
			t.Fatalf("missing %s", action)
		}
	}
}
