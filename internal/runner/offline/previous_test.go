// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
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
	moved, _ := filepath.Glob(filepath.Join(dir, "previous", earlier.String()))
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

// earlierRegistration leaves m's state, with one live run that has spools and output, for a new
// registration of the same computer, and returns the state directory, the live run and both ids.
func earlierRegistration(t *testing.T) (*localSub, string, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	m, sub, _, org := managerFixture(t)
	live := createLocal(t, m, org)
	for _, name := range []string{"spools", "output"} {
		if err := os.MkdirAll(filepath.Join(m.dir, name, live.String()), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	return sub, m.dir, live, m.runnerID, uuid.New()
}

// TestReRegistrationRetryKeepsOneDirectory fails the move after spools/ has gone: the retry at the
// next start finishes it in the same previous/<runner id>, so one registration's state stays together.
func TestReRegistrationRetryKeepsOneDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root renames a directory it cannot write")
	}
	sub, dir, live, earlier, registered := earlierRegistration(t)
	// A directory moved to another parent needs write permission on itself, for its .. entry.
	if err := os.Chmod(filepath.Join(dir, "output"), 0500); err != nil {
		t.Fatal(err)
	}
	if err := SetAsidePrevious(t.Context(), dir, registered, sub, uuid.UUID.String); err == nil {
		t.Fatal("the move of an unwritable output/ succeeded")
	}
	if err := os.Chmod(filepath.Join(dir, "output"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := SetAsidePrevious(t.Context(), dir, registered, sub, uuid.UUID.String); err != nil {
		t.Fatal(err)
	}
	moved, _ := filepath.Glob(filepath.Join(dir, "previous", "*"))
	if len(moved) != 1 || moved[0] != filepath.Join(dir, "previous", earlier.String()) {
		t.Fatalf("set aside into %v", moved)
	}
	for _, name := range []string{"offline/state.json", "spools/" + live.String(), "output/" + live.String()} {
		if _, err := os.Stat(filepath.Join(moved[0], name)); err != nil {
			t.Errorf("%s was not set aside with the rest: %v", name, err)
		}
	}
}

// TestReRegistrationBesideAFullerOlderSetAside restores a complete older set-aside of the same
// earlier registration: the state moves into a fresh sibling, and the older one is left as it was.
func TestReRegistrationBesideAFullerOlderSetAside(t *testing.T) {
	sub, dir, live, earlier, registered := earlierRegistration(t)
	older := filepath.Join(dir, "previous", earlier.String())
	for _, name := range []string{"spools", "output", "offline"} {
		if err := os.MkdirAll(filepath.Join(older, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(older, name, "older"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := SetAsidePrevious(t.Context(), dir, registered, sub, uuid.UUID.String); err != nil {
		t.Fatalf("a restored older set-aside blocks the start: %v", err)
	}
	for _, name := range []string{"spools", "output", "offline"} {
		if _, err := os.Stat(filepath.Join(older, name, "older")); err != nil {
			t.Errorf("the older set-aside's %s changed: %v", name, err)
		}
	}
	fresh, _ := filepath.Glob(filepath.Join(dir, "previous", earlier.String()+"-*"))
	if len(fresh) != 1 {
		t.Fatalf("fresh siblings %v", fresh)
	}
	for _, name := range []string{"offline/state.json", "spools/" + live.String(), "output/" + live.String()} {
		if _, err := os.Stat(filepath.Join(fresh[0], name)); err != nil {
			t.Errorf("%s was not set aside into the sibling: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "offline")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the earlier registration's offline state stayed in place: %v", err)
	}
}

// TestReRegistrationLeavesAnUnprovenContainerAlone sets aside a run whose container the substrate
// cannot prove the earlier registration created: its proxy is not stopped, and the state still moves.
func TestReRegistrationLeavesAnUnprovenContainerAlone(t *testing.T) {
	sub, dir, _, earlier, registered := earlierRegistration(t)
	sub.foreign = fmt.Errorf("labelled for another runner: %w", runner.ErrNotRunnerSandbox)
	if err := SetAsidePrevious(t.Context(), dir, registered, sub, uuid.UUID.String); err != nil {
		t.Fatal(err)
	}
	if sub.stops != 0 {
		t.Fatalf("stopped the proxy of a container it could not prove the earlier registration created: %d stops", sub.stops)
	}
	if sub.provedFor != earlier.String() {
		t.Fatalf("proved against runner %q, want the earlier registration %s", sub.provedFor, earlier)
	}
	if _, err := os.Stat(filepath.Join(dir, "previous", earlier.String(), "offline", "state.json")); err != nil {
		t.Fatalf("the state was not set aside: %v", err)
	}
}

// TestReRegistrationSetAsideIsBoundedOnAHungEngine re-registers with two earlier runs while the engine
// never answers: each run's call has its own bound, the first that runs out ends the pass (the next
// would wait the same way), the start error names the container, and nothing moves.
func TestReRegistrationSetAsideIsBoundedOnAHungEngine(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	dir := m.dir
	createLocal(t, m, org)
	createLocal(t, m, org)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	defer func(b time.Duration) { setAsideCallBound = b }(setAsideCallBound)
	setAsideCallBound = 50 * time.Millisecond
	sub.hung = make(chan struct{}, 2)
	err := SetAsidePrevious(t.Context(), dir, uuid.New(), sub, uuid.UUID.String)
	if err == nil || !strings.Contains(err.Error(), "the container engine did not answer within 50ms for ") {
		t.Fatalf("SetAsidePrevious = %v, want the bounded refusal naming the container", err)
	}
	if n := len(sub.hung); n != 1 {
		t.Fatalf("the pass waited on %d containers, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "offline", "state.json")); err != nil {
		t.Fatalf("state moved while its proxies were not stopped: %v", err)
	}
}

// lateStop answers the proxy stop successfully, but only once its bound has run out.
type lateStop struct{ *localSub }

func (lateStop) StopProxy(ctx context.Context, _ string) error {
	<-ctx.Done()
	return nil
}

// TestReRegistrationStopAnsweredAtTheBoundEdgeSucceeds stops a proxy whose success arrives with the
// call's bound already spent: the stop worked, so the start neither fails (wrapping a nil error) nor
// refuses to move the state.
func TestReRegistrationStopAnsweredAtTheBoundEdgeSucceeds(t *testing.T) {
	sub, dir, _, earlier, registered := earlierRegistration(t)
	defer func(b time.Duration) { setAsideCallBound = b }(setAsideCallBound)
	setAsideCallBound = 50 * time.Millisecond
	if err := SetAsidePrevious(t.Context(), dir, registered, lateStop{sub}, uuid.UUID.String); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "previous", earlier.String(), "offline", "state.json")); err != nil {
		t.Fatalf("the state was not set aside: %v", err)
	}
}
