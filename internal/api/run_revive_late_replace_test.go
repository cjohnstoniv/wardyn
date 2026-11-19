// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A superseded revive (A) whose replace still reaches the runner after a newer edit (B) removed a host
// and confirmed, on the second replica: A's new proxy is handed a token B's lineage cut already cut off,
// so its first renewal is refused, and a proxy that stays closed until that renewal lands
// (the sidecar's first-renew gate) never serves A's older config. A itself stops nothing and loses
// nothing: its lost mark is fenced on its generation, which B moved on.

// lateReplace records, after the run's first (B's, unless the test says otherwise) confirmed replace,
// the config each later replace was handed: A's.
type lateReplace struct {
	mu   sync.Mutex
	aCfg []byte
}

func (l *lateReplace) record(cfg []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.aCfg = slices.Clone(cfg)
}

func (l *lateReplace) cfg(t *testing.T) []byte {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.aCfg == nil {
		t.Fatal("A's replace never reached the runner")
	}
	return l.aCfg
}

// handedRunToken is the run token in a handed-over proxy config.
func handedRunToken(t *testing.T, cfg []byte) string {
	t.Helper()
	var c struct {
		RunToken string `json:"run_token"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil || c.RunToken == "" {
		t.Fatalf("the handed-over config has no run token: %v", err)
	}
	return c.RunToken
}

// assertARefusedAndBStands: A's token renews to 401 or 503; A stopped no proxy and lost nothing; B's
// start is enforced; and the stored config is still B's, X denied.
func (f *fenceFixture) assertARefusedAndBStands(t *testing.T, aCfg []byte, stops int, storedAfterB []byte) {
	t.Helper()
	if w := renew(t, f.reviveFixture, handedRunToken(t, aCfg)); w.Code != http.StatusUnauthorized && w.Code != http.StatusServiceUnavailable {
		t.Errorf("A's token renewal: %d %s, want 401 or 503", w.Code, w.Body.String())
	}
	if n := f.lr.proxyStopCount(); n != stops {
		t.Errorf("proxy stops = %d, want %d: A must stop no proxy", n, stops)
	}
	if lostAt, reason := f.st.lost(); lostAt != nil {
		t.Errorf("lost at %v (%s): A must not lose a run B took over", lostAt, reason)
	}
	if p := f.active(t); p.State != types.ProxyStartEnforced {
		t.Errorf("start = %d %s, want B's enforced", p.Generation, p.State)
	}
	if got := f.storedConfig(); !slices.Equal(got, storedAfterB) {
		t.Error("the stored config is no longer B's")
	}
	denied(t, "the stored config", f.storedPolicy(t))
}

// A pauses after its advance and before its replace; B removes X, replaces and confirms; then A
// resumes (its lost lock not seen yet) and its replace reaches the runner.
func TestRevive_ReplaceAfterNewerConfirmedEdit_HandsARefusedToken(t *testing.T) {
	f := newFenceFixture(t)
	stops := f.lr.proxyStopCount()
	var late lateReplace
	replaces := 0
	f.rr.onReplace = func(cfg []byte) {
		replaces++
		if replaces == 1 { // B's
			if code, _ := postLoaded(t, f.b, f.run.ID, f.tokenJTI(t, cfg), hashHex(cfg)); code != http.StatusAccepted {
				t.Errorf("B's proxy report: %d, want 202 (held)", code)
			}
			return
		}
		late.record(cfg)
	}
	var b *httptest.ResponseRecorder
	var storedAfterB []byte
	f.is.afterAdvance = func(int) {
		b = f.editOnB(t)
		storedAfterB = f.storedConfig()
	}
	a := f.reviveOnA(t)
	if b == nil || b.Code != http.StatusOK {
		t.Fatalf("B never ran or did not confirm: %v", b)
	}
	if a.Code == http.StatusOK {
		t.Errorf("A: 200, want a refusal: its start was superseded")
	}
	f.assertARefusedAndBStands(t, late.cfg(t), stops, storedAfterB)
}

// A's replace is in flight (past the point a driver removes the old proxy) and blocks there; B removes X,
// replaces and confirms; then A's replace is released and completes.
func TestRevive_InFlightReplaceAcrossNewerConfirmedEdit_HandsARefusedToken(t *testing.T) {
	f := newFenceFixture(t)
	stops := f.lr.proxyStopCount()
	var late lateReplace
	inA, release := make(chan struct{}), make(chan struct{})
	replaces := 0
	f.rr.onReplace = func(cfg []byte) {
		replaces++
		if replaces == 1 { // A's: held after its remove until B is done
			late.record(cfg)
			close(inA)
			<-release
			return
		}
		if code, _ := postLoaded(t, f.b, f.run.ID, f.tokenJTI(t, cfg), hashHex(cfg)); code != http.StatusAccepted {
			t.Errorf("B's proxy report: %d, want 202 (held)", code)
		}
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.reviveOnA(t) }()
	select {
	case <-inA:
	case <-time.After(10 * time.Second):
		t.Fatal("A's replace never started")
	}
	b := f.editOnB(t)
	storedAfterB := f.storedConfig()
	close(release)
	var a *httptest.ResponseRecorder
	select {
	case a = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("A never finished")
	}
	if b.Code != http.StatusOK {
		t.Fatalf("B: %d %s, want 200", b.Code, b.Body.String())
	}
	if a.Code == http.StatusOK {
		t.Errorf("A: 200, want a refusal: its start was superseded")
	}
	f.assertARefusedAndBStands(t, late.cfg(t), stops, storedAfterB)
}
