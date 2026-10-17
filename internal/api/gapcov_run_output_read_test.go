// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

func gapCovReadOutput(t *testing.T, f *outputFixture, query string) (int, string, runOutputResponse) {
	t.Helper()
	w := do(t, f.srv, http.MethodGet, "/api/v1/runs/"+f.run.ID.String()+"/output"+query, adminToken, "")
	var got runOutputResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode %q: %v", w.Body, err)
		}
	}
	return w.Code, errorReason(w), got
}

// A stored final row longer than the requested tail is cut to its last bytes and
// marked truncated.
func TestGapCovReadOutputCutsAStoredRowToTheRequestedTail(t *testing.T) {
	f := newOutputFixture(t)
	at := time.Now()
	f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, Source: "stdout", Output: []byte("0123456789"), CapturedAt: &at}

	code, _, got := gapCovReadOutput(t, f, "?tail=4")
	if code != http.StatusOK || got.Output != "6789" || !got.Truncated || !got.Complete {
		t.Fatalf("read = %d %+v, want the last 4 bytes, truncated and complete", code, got)
	}
	code, _, got = gapCovReadOutput(t, f, "")
	if code != http.StatusOK || got.Output != "0123456789" || got.Truncated {
		t.Fatalf("whole read = %d %+v, want all the bytes, not truncated", code, got)
	}
}

// A store that cannot read the row answers a server error that names no cause.
func TestGapCovReadOutputStoreFailureIsAServerError(t *testing.T) {
	var gs *gapCovOutStore
	f := newOutputFixture(t, func(c *Config) {
		gs = &gapCovOutStore{miscCovOutStore: newMiscCovOutStore(c.Store.(*memRunOutputs))}
		c.Store = gs
	})
	gs.getOutErr = errors.New("gapcov: secret-internal-detail")
	w := do(t, f.srv, http.MethodGet, "/api/v1/runs/"+f.run.ID.String()+"/output", adminToken, "")
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "secret-internal-detail") {
		t.Fatalf("status %d body %s, want a 500 that does not carry the cause", w.Code, w.Body)
	}
}

// A running run whose masking corpus cannot be proven whole is refused 503 with
// the mask_state_unavailable reason.
func TestGapCovReadOutputOfAnUncoveredRunIsRefused(t *testing.T) {
	f := newOutputFixture(t, func(c *Config) {
		pool, _ := miscCovClosedPool(t)
		c.MaskRegistry = secretmask.NewRegistry()
		c.MaskManifests = maskmanifest.New(pool, nil, c.MaskRegistry)
	})
	code, reason, _ := gapCovReadOutput(t, f, "")
	if code != http.StatusServiceUnavailable || reason != string(authz.ReasonMaskStateUnavailable) {
		t.Fatalf("read = %d %q, want 503 %s", code, reason, authz.ReasonMaskStateUnavailable)
	}
}

// With no tail here, a replica's live chunks answer the read.
func TestGapCovReadOutputFallsBackToTheSharedChunks(t *testing.T) {
	f, cs := miscCovOutFixture(t)
	cs.readRes = store.RunOutputChunks{Found: true, Bytes: []byte("from another replica\n"), Truncated: true}

	code, _, got := gapCovReadOutput(t, f, "")
	if code != http.StatusOK || got.Output != "from another replica\n" || !got.Truncated || got.Complete {
		t.Fatalf("read = %d %+v, want the chunks, truncated and not complete", code, got)
	}
	if len(cs.reads) != 1 {
		t.Errorf("the chunk store was read %d time(s), want once", len(cs.reads))
	}
}

// A pending row with no tail here and no chunks anywhere is "still being captured", a 409 the caller
// can retry.
func TestGapCovReadOutputOfAPendingRowIsStillBeingCaptured(t *testing.T) {
	f := newOutputFixture(t)
	f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, Source: "stdout", ClaimedAt: time.Now()}

	code, reason, _ := gapCovReadOutput(t, f, "")
	if code != http.StatusConflict || reason != reasonRunOutputNotKept {
		t.Fatalf("read = %d %q, want 409 %s", code, reason, reasonRunOutputNotKept)
	}
}

// Failing to record that a capture is owed does not stop the tail from opening.
func TestGapCovOpenExecOutputKeepsTheTailWhenThePendingRowCannotBeWritten(t *testing.T) {
	var gs *gapCovOutStore
	f := newOutputFixture(t, func(c *Config) {
		gs = &gapCovOutStore{miscCovOutStore: newMiscCovOutStore(c.Store.(*memRunOutputs))}
		c.Store = gs
	})
	gs.insertErr = errors.New("gapcov: insert refused")
	logs := miscCovCaptureLogs(t)

	if f.srv.openExecOutput(f.run, false) == nil || f.srv.tailFor(f.run.ID) == nil {
		t.Fatal("no tail was opened after a failed pending-row write")
	}
	if _, ok := logs.find("could not record that a run's output capture is owed"); !ok {
		t.Error("the failed write was not logged")
	}
}
