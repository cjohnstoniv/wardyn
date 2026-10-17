// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// TestReRegistrationSetsTheEarlierRunStateAside re-registers a computer whose state directory still
// holds the earlier registration's runs. Their proxies are stopped before anything moves; while one
// cannot be, nothing moves and the error names the remedy, which then works.
func TestReRegistrationSetsTheEarlierRunStateAside(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	dir, earlier := m.dir, m.runnerID
	live := createLocal(t, m, org)
	stopped := createLocal(t, m, org)
	stop := runnerwire.PendingAction{ID: uuid.New(), RunID: stopped, Ref: placement.RunnerRefPrefix(earlier.String()) + stopped.String(), Kind: types.RunnerActionStopProxy}
	if res, err := m.apply(t.Context(), stop); err != nil || res.Outcome == "" {
		t.Fatalf("stop: %+v %v", res, err)
	}
	for _, name := range []string{"spools", "output"} {
		if err := os.MkdirAll(filepath.Join(dir, name, live.String()), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	registered := uuid.New()
	if _, err := New(dir, registered, sub, uuid.UUID.String, noOutput, noErase); err == nil {
		t.Fatal("a new identity loaded the earlier registration's state")
	}

	sub.stopErr = errors.New("engine unreachable")
	err := SetAsidePrevious(t.Context(), dir, registered, sub, uuid.UUID.String)
	want := fmt.Sprintf("the run state in %s belongs to an earlier registration of this computer (runner %s), and 1 of its runs' proxies could not be stopped (engine unreachable): make the container engine or cluster reachable, then restart wardyn-runnerd, which stops them and moves that state into %s",
		dir, earlier, filepath.Join(dir, "previous"))
	if err == nil || err.Error() != want {
		t.Fatalf("remedy:\n got %v\nwant %s", err, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "offline", "state.json")); err != nil {
		t.Fatalf("state moved before its proxies were stopped: %v", err)
	}
	if moved, _ := filepath.Glob(filepath.Join(dir, "previous", "*")); len(moved) != 0 {
		t.Fatalf("state moved before its proxies were stopped: %v", moved)
	}

	// The remedy: the engine answers again and the runner restarts.
	sub.stopErr = nil
	if err := SetAsidePrevious(t.Context(), dir, registered, sub, uuid.UUID.String); err != nil {
		t.Fatal(err)
	}
	// One stop for the stop_proxy action, then the live run's proxy on each attempt; never the stopped one's.
	if sub.stops != 3 {
		t.Fatalf("proxy stops %d", sub.stops)
	}
	moved, _ := filepath.Glob(filepath.Join(dir, "previous", earlier.String()+"-*"))
	if len(moved) != 1 {
		t.Fatalf("set aside into %v", moved)
	}
	for _, name := range []string{"offline/state.json", "spools/" + live.String(), "output/" + live.String()} {
		if _, err := os.Stat(filepath.Join(moved[0], name)); err != nil {
			t.Errorf("%s was not set aside: %v", name, err)
		}
	}
	fresh, err := New(dir, registered, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatalf("the re-registered runner cannot start: %v", err)
	}
	defer fresh.Close()
	if len(fresh.state.Runs) != 0 {
		t.Fatalf("the earlier registration's runs came along: %v", fresh.state.Runs)
	}
	// Its own state is its own: nothing is stopped or moved again.
	if err := SetAsidePrevious(t.Context(), dir, registered, sub, uuid.UUID.String); err != nil || sub.stops != 3 {
		t.Fatalf("own state: %v, stops %d", err, sub.stops)
	}
	if again, _ := filepath.Glob(filepath.Join(dir, "previous", "*")); len(again) != 1 {
		t.Fatalf("own state set aside: %v", again)
	}
}
