// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestGetRun_LoginRunStaysReadableWhileCreateSandboxBlocks is a CHARACTERIZATION
// test, and it is expected GREEN.
//
// handleGetRun is a plain store SELECT with no runner call, no lock and no
// dependence on dispatch, so a sign-in pane polling during a cold image pull can
// always READ the run; the pane's budget is a tick count that a 131-second pull
// with healthy reads never trips (login-start-wait.ts). With CreateSandbox held
// open, the launching member reads their own run over and over, and gets it.
//
// If this ever goes RED the cause is in-tree: the server has a real read bug,
// not a wording problem — stop and report rather than adjusting the test.
func TestGetRun_LoginRunStaysReadableWhileCreateSandboxBlocks(t *testing.T) {
	gr := &coldPullRunner{
		fakeRunner: &fakeRunner{},
		gate:       make(chan struct{}),
		entered:    make(chan struct{}),
	}
	released := false
	t.Cleanup(func() {
		if !released {
			close(gr.gate)
		}
	})
	// newSupersedeFixture, not perUserLoginSrvWithRunner: this test READS the run,
	// and that path projects a run's UI apps out of its audit trail — a query the
	// plain login double does not answer at all (a nil promoted method, i.e. a
	// panic, not the logged error handleGetRun tolerates). The store is otherwise
	// the same one.
	srv := newSupersedeFixture(t, nil, gr).srv
	mine := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login", mine, `{"provider":"aws"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("launch: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var launched struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &launched); err != nil {
		t.Fatalf("decode launch: %v (%s)", err, w.Body.String())
	}

	// THE PREMISE, ESTABLISHED (R1-F3): wait until the detached launch goroutine
	// is actually INSIDE CreateSandbox. Without this the ten reads below most
	// likely finish before dispatch ever reaches the runner, and the test would
	// prove nothing about reading a run mid-pull. What it still cannot speak for
	// is a real store: an in-memory double takes no locks, so this pins the
	// HANDLER's shape (no runner call on the read path), not PG's behaviour.
	select {
	case <-gr.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the detached launch never reached CreateSandbox within 5s — the reads below would not be mid-pull")
	}

	// Ten reads, the span the pane's first ticks cover, all while the sandbox is
	// still coming up.
	for i := 0; i < 10; i++ {
		r := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+launched.RunID, mine, "")
		if r.Code != http.StatusOK {
			t.Fatalf("read %d: code = %d, want 200 — the pane's poll reads this exact route; body=%s",
				i, r.Code, r.Body.String())
		}
		var got struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
			t.Fatalf("read %d: decode run: %v (%s)", i, err, r.Body.String())
		}
		if got.State != "PENDING" && got.State != "STARTING" {
			t.Fatalf("read %d: state = %q, want PENDING or STARTING while CreateSandbox blocks", i, got.State)
		}
	}

	// The ownership rule is the same one on the same route: somebody else's
	// sign-in is a 404, not a 403 — which is also the ONE way a healthy daemon
	// makes getRun fail for the pane (a roster edit mid-wait).
	theirs := ssoSession(t, "sub-other", "other@corp.example", oidc.RoleMember)
	r := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+launched.RunID, theirs, "")
	if r.Code != http.StatusNotFound {
		t.Fatalf("foreign read: code = %d, want 404; body=%s", r.Code, r.Body.String())
	}

	close(gr.gate)
	released = true
}
