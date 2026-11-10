// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package folders

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/srvclock"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// Two passes for one run at once: one starts, the other is refused as busy (retryable), and a pass
// for another run is admitted. Once the first ends, the run takes a pass again.
func TestStart_RefusesASecondPassForTheSameRun(t *testing.T) {
	h := home(t)
	app := filepath.Join(h, "code", "app")
	sub := wire(t, newServer(t, h))
	run := uuid.New()
	args := runnerwire.SyncFolderArgs{RunID: run, Path: app, RemoteDir: "/home/agent/work"}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		passes []remote.FolderPass
		errs   []error
	)
	for range 2 {
		wg.Go(func() {
			p, err := sub.SyncFolder(context.Background(), args)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			passes = append(passes, p)
		})
	}
	wg.Wait()
	if len(passes) != 1 || len(errs) != 1 || !errors.Is(errs[0], runner.ErrFolderPassBusy) {
		t.Fatalf("started %d, refused %v: want one pass and one folder_pass_busy", len(passes), errs)
	}
	var werr *runnerwire.Error
	if !errors.As(errs[0], &werr) || werr.Code != runnerwire.CodeFolderPassBusy || werr.Message != "a pass for this run is still running; try again" {
		t.Errorf("refusal = %v", errs[0])
	}

	other := runPass(t, sub, newSandbox(t), "/home/agent/work", runnerwire.SyncFolderArgs{RunID: uuid.New(), Path: app, RemoteDir: "/home/agent/work"})
	if other.Error != "" {
		t.Errorf("another run's pass = %+v", other)
	}

	// The held pass ends; the run takes a pass again.
	srv := newSandbox(t).serve(passes[0].Stream, "/home/agent/work")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if res, err := passes[0].Wait(ctx); err != nil || res.Error != "" {
		t.Fatalf("held pass = %+v, %v", res, err)
	}
	_ = srv.Close()
	if res := runPass(t, sub, newSandbox(t), "/home/agent/work", args); res.Error != "" {
		t.Errorf("the run's next pass = %+v", res)
	}
}

// A pass refused before it starts frees its run at once.
func TestStart_ARefusedPassFreesItsRun(t *testing.T) {
	h := home(t)
	app := filepath.Join(h, "code", "app")
	sub := wire(t, newServer(t, h))
	run := uuid.New()
	if _, err := sub.SyncFolder(context.Background(), runnerwire.SyncFolderArgs{RunID: run, Path: app, RemoteDir: "/etc"}); !refused(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if res := runPass(t, sub, newSandbox(t), "/home/agent/work", runnerwire.SyncFolderArgs{RunID: run, Path: app, RemoteDir: "/home/agent/work"}); res.Error != "" {
		t.Errorf("pass after a refusal = %+v", res)
	}
}

// helloClock is a hello clock read from a hello whose server time is skew behind this runner's.
func helloClock(t *testing.T, skew time.Duration) (*srvclock.Clock, time.Time) {
	t.Helper()
	c := srvclock.NewClock(nil)
	server := time.Now().Add(-skew)
	if err := c.Hello(runnerwire.Hello{ServerTime: server}, time.Now()); err != nil {
		t.Fatal(err)
	}
	return c, server
}

func TestPassEnd_BoundsAPassByTheServersDeadlineOrTheOldRule(t *testing.T) {
	s := newServer(t, home(t))
	near := func(end time.Time, want time.Duration) bool {
		left := time.Until(end)
		return !end.IsZero() && left > want-time.Minute && left <= want
	}
	end := func(args runnerwire.SyncFolderArgs) time.Time {
		t.Helper()
		e, err := s.passEnd(args)
		if err != nil {
			t.Fatalf("passEnd(%+v): %v", args, err)
		}
		return e
	}

	// An older server names no deadline: a seed pass has no limit, a pass back has backPassLimit.
	if e := end(runnerwire.SyncFolderArgs{}); !e.IsZero() {
		t.Error("a seed pass from an older server has a limit")
	}
	if e := end(runnerwire.SyncFolderArgs{Pull: true}); !near(e, backPassLimit) {
		t.Errorf("a pass back from an older server ends in %v, want %v", time.Until(e), backPassLimit)
	}

	// The server's deadline is its own wall time: read through the hello clock, a runner whose clock
	// runs an hour ahead still gets the full ten minutes, on a seed pass too.
	clock, server := helloClock(t, time.Hour)
	s.Clock = clock
	for _, pull := range []bool{false, true} {
		if e := end(runnerwire.SyncFolderArgs{Pull: pull, DeadlineUnixMs: server.Add(10 * time.Minute).UnixMilli()}); !near(e, 10*time.Minute) {
			t.Errorf("pull=%v: a skewed runner's pass ends in %v, want ten minutes", pull, time.Until(e))
		}
	}
	// A deadline the server has already passed is refused at once.
	if _, err := s.passEnd(runnerwire.SyncFolderArgs{DeadlineUnixMs: server.Add(-time.Second).UnixMilli()}); err == nil ||
		!strings.Contains(err.Error(), "the pass deadline has passed") {
		t.Errorf("a passed deadline: err = %v", err)
	}

	// No clock reading yet: the deadline reads as absent and the old rule applies.
	s.Clock = srvclock.NewClock(nil)
	if e := end(runnerwire.SyncFolderArgs{DeadlineUnixMs: time.Now().Add(time.Second).UnixMilli()}); !e.IsZero() {
		t.Error("with no clock reading, a seed pass has a limit")
	}
	if e := end(runnerwire.SyncFolderArgs{Pull: true, DeadlineUnixMs: time.Now().Add(time.Second).UnixMilli()}); !near(e, backPassLimit) {
		t.Errorf("with no clock reading, a pass back ends in %v, want %v", time.Until(e), backPassLimit)
	}
}

// Over the wire, with the back limit made instant: an older server's seed pass (no deadline) still
// completes, and so does a seed with a deadline on a runner whose clock is skewed ahead; an older
// server's pass back is cut.
func TestSyncFolder_AnOlderServersSeedIsNotCutAtTheBackLimit(t *testing.T) {
	old := backPassLimit
	backPassLimit = time.Nanosecond
	t.Cleanup(func() { backPassLimit = old })
	h := home(t)
	app := filepath.Join(h, "code", "app")
	s := newServer(t, h)
	sub := wire(t, s)

	seed := runPass(t, sub, newSandbox(t), "/home/agent/work", runnerwire.SyncFolderArgs{RunID: uuid.New(), Path: app, RemoteDir: "/home/agent/work"})
	if seed.Error != "" || seed.Pushed != 2 {
		t.Errorf("older server's seed = %+v, want it complete", seed)
	}
	clock, server := helloClock(t, time.Hour)
	s.Clock = clock
	skewed := runPass(t, sub, newSandbox(t), "/home/agent/work", runnerwire.SyncFolderArgs{
		RunID: uuid.New(), Path: app, RemoteDir: "/home/agent/work", DeadlineUnixMs: server.Add(10 * time.Minute).UnixMilli()})
	if skewed.Error != "" || skewed.Pushed != 2 {
		t.Errorf("skewed runner's seed = %+v, want it complete", skewed)
	}
	s.Clock = nil
	back := runPass(t, sub, newSandbox(t), "/home/agent/work", runnerwire.SyncFolderArgs{RunID: uuid.New(), Path: app, RemoteDir: "/home/agent/work", Pull: true})
	if back.Error == "" {
		t.Errorf("older server's pass back = %+v, want it cut at the back limit", back)
	}
}

// An unreadable record no longer refuses every pass: it is moved aside, named in the pass, and the
// pass compares every file.
func TestSyncFolder_AnUnreadableRecordIsMovedAsideAndNamed(t *testing.T) {
	h := home(t)
	app := filepath.Join(h, "code", "app")
	stateDir := t.TempDir()
	s, err := New(context.Background(), []string{h}, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	sub := wire(t, s)
	run := uuid.New()
	put(t, filepath.Join(stateDir, "folders", run.String()+".json"), `{"files":`)
	res := runPass(t, sub, newSandbox(t), "/home/agent/work", runnerwire.SyncFolderArgs{RunID: run, Path: app, RemoteDir: "/home/agent/work"})
	if res.Error != "" || !strings.HasPrefix(res.Recovered, run.String()+".json.unreadable-") {
		t.Fatalf("pass = %+v, want it complete and the record named", res)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "folders", res.Recovered)); err != nil {
		t.Errorf("the moved-aside record: %v", err)
	}
}
