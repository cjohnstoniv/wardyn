// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDriveShareProbeDoesNotStackBehindAStrandedOne is F295.
//
// F269 gave GET /me the launch door's own bind DECISION so the console would
// stop offering a mount the create path refuses. What came with it is that a
// DISPLAY READ ON A TIMER now performs the share probe: up to two uncancellable
// filesystem syscalls, each bounded at five seconds, on a path the operator
// mounted. The syscall takes no context, so every probe the bound gives up on
// leaves a thread in the kernel until the mount answers — and on a hard mount
// that is never. One console left open on a blackholed share therefore stranded
// two threads per poll, forever, while the member watched a ten-second first
// paint; neither the refusal metric nor the WARN fires on the /me path, so none
// of it was observable.
//
// Asking again cannot produce a different answer while the first ask is still
// outstanding, so a subject already known to be overdue is now refused from
// memory. These pin the two halves that makes true: nothing new is started, and
// nothing is waited on.
func TestDriveShareProbeDoesNotStackBehindAStrandedOne(t *testing.T) {
	srv := New(Config{Store: &driveStore{}, RunnerTarget: "docker"})

	t.Run("an overdue subject is answered from memory, starting no second syscall", func(t *testing.T) {
		key := "home:" + t.TempDir()
		// The state a hung mount leaves: a probe thread started longer ago than
		// the bound and still not back.
		driveShareProbes.Store(key, time.Now().Add(-time.Minute))
		t.Cleanup(func() { driveShareProbes.Delete(key) })

		ran := make(chan struct{}, 1)
		start := time.Now()
		err, ok := srv.driveShareProbe(context.Background(), key, func() error {
			ran <- struct{}{}
			return nil
		})

		if ok {
			t.Error("the probe reported an ANSWER while the previous syscall on this subject is still " +
				"outstanding — the share is demonstrably not answering, and claiming otherwise would let a " +
				"launch mount a share nothing has confirmed")
		}
		if err != nil {
			t.Errorf("probe err = %v, want nil — an unanswered probe is not a refusal reason", err)
		}
		select {
		case <-ran:
			t.Error("a SECOND uncancellable syscall was started behind the stranded one: this is the " +
				"accumulation itself — a /me poll on a console timer strands two more threads every few " +
				"seconds, for as long as the tab stays open")
		default:
		}
		if elapsed := time.Since(start); elapsed >= driveShareProbeTimeout {
			t.Errorf("the read waited %v behind a probe already known to be overdue, want it to return at "+
				"once — the ten-second first paint is the member-visible half of this", elapsed)
		}
	})

	// THE CONTROL, and it is the one that decides whether this is a fix or a
	// blanket refusal: a subject nobody is waiting on is probed normally, and a
	// probe a concurrent request legitimately has IN FLIGHT (started, not yet
	// overdue) does not short-circuit either. Without this the "fix" would turn
	// ordinary concurrency on a healthy share into share_unreachable.
	t.Run("a subject with no overdue probe is still asked", func(t *testing.T) {
		key := "home:" + t.TempDir()
		ran := make(chan struct{}, 1)
		err, ok := srv.driveShareProbe(context.Background(), key, func() error {
			ran <- struct{}{}
			return nil
		})
		if !ok || err != nil {
			t.Fatalf("probe = (%v, %v), want an answer — a share that answers must still be asked", err, ok)
		}
		select {
		case <-ran:
		default:
			t.Error("the check never ran: the short-circuit fired for a subject nobody was waiting on")
		}
		if _, still := driveShareProbes.Load(key); still {
			t.Error("a probe that ANSWERED left its subject marked outstanding — the mark is removed by the " +
				"probe goroutine itself, so a share that comes back is asked again rather than refused forever")
		}
	})

	t.Run("a probe still inside its bound does not short-circuit a second reader", func(t *testing.T) {
		key := "home:" + t.TempDir()
		driveShareProbes.Store(key, time.Now()) // in flight, not yet overdue
		t.Cleanup(func() { driveShareProbes.Delete(key) })
		ran := make(chan struct{}, 1)
		if _, ok := srv.driveShareProbe(context.Background(), key, func() error {
			ran <- struct{}{}
			return nil
		}); !ok {
			t.Error("a reader was refused because ANOTHER request had a probe in flight and inside its bound — " +
				"that is ordinary concurrency on a healthy share, not a strand")
		}
		select {
		case <-ran:
		default:
			t.Error("the check never ran for an in-flight, not-yet-overdue subject")
		}
	})

	// …AND THAT SECOND READER MUST NOT RETIRE THE FIRST PROBE'S MARK on its way
	// out. Only the caller whose LoadOrStore stored the entry owns it. A loser
	// that deleted would hand the map amnesia about a syscall still outstanding:
	// if that first probe then strands, the next reader finds no mark and starts
	// a thread behind it — the stacking this whole mechanism exists to stop,
	// re-introduced by the concurrency it deliberately allows.
	t.Run("a concurrent reader does not retire the outstanding probe's mark", func(t *testing.T) {
		key := "home:" + t.TempDir()
		hang := make(chan struct{})
		t.Cleanup(func() { close(hang) })
		started := make(chan struct{})

		// The OWNER: it stores the mark, then its syscall never returns.
		go func() {
			srv.driveShareProbe(context.Background(), key, func() error {
				close(started)
				<-hang
				return nil
			})
		}()
		<-started
		owned, ok := driveShareProbes.Load(key)
		if !ok {
			t.Fatal("the owning probe left no mark")
		}
		t.Cleanup(func() { driveShareProbes.Delete(key) })

		// The LOSER: finds a probe in flight and inside its bound, so it asks
		// too — and then returns.
		if _, ok := srv.driveShareProbe(context.Background(), key, func() error { return nil }); !ok {
			t.Fatal("the concurrent reader was refused although the outstanding probe is not yet overdue")
		}

		still, present := driveShareProbes.Load(key)
		if !present {
			t.Fatal("a concurrent reader DELETED the mark of a probe that is still outstanding: the next " +
				"reader will start a second uncancellable syscall behind the first, which is the accumulation " +
				"F295 removes")
		}
		if still != owned {
			t.Errorf("the mark changed from %v to %v — a loser must not reset the clock either, or a queue of "+
				"readers keeps the oldest outstanding syscall looking young forever", owned, still)
		}

		// …and once that outstanding probe IS overdue, readers are refused from
		// memory, which is only possible because the mark survived above.
		driveShareProbes.Store(key, time.Now().Add(-time.Minute))
		ran := make(chan struct{}, 1)
		if _, ok := srv.driveShareProbe(context.Background(), key, func() error {
			ran <- struct{}{}
			return nil
		}); ok {
			t.Error("a reader was given an ANSWER while the owning syscall is overdue and outstanding")
		}
		select {
		case <-ran:
			t.Error("a syscall was started behind the overdue owner")
		default:
		}
	})
}

// …and the route the finding is actually about. A GET /me for a host_path
// allocation whose share has already stranded a probe answers from memory: it
// does not stat, and it reports the SAME state the launch door reports for the
// same subject, which is the agreement F269 exists to hold.
//
// The home directory here EXISTS, so a /me that reached the filesystem would
// offer the drive. That it does not is the whole observation.
func TestMeAnswersFromMemoryWhileAShareProbeIsStranded(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bob"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := driveFixture(func(d *types.UserDrive) {
		d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateSub, root
		d.Writable = true
	})
	st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
	srv, _ := driveShareServer(st, []string{root})
	ctx := withOIDCGroups(operatorCtx("bob", "bob@corp.example", oidc.RoleMember), nil)

	// The control first: with nothing stranded, this member's drive binds and
	// /me offers it. A test that only asserted the withheld case would pass on a
	// /me that withholds everything.
	if ud, _, reason := meDriveBody(t, srv, ctx); ud == nil || reason != "" {
		t.Fatalf("control: /me withheld a drive that binds (user_drive=%v reason=%q)", ud, reason)
	}

	key := "home:" + filepath.Join(root, "bob")
	driveShareProbes.Store(key, time.Now().Add(-time.Minute))
	t.Cleanup(func() { driveShareProbes.Delete(key) })

	start := time.Now()
	ud, denied, reason := meDriveBody(t, srv, ctx)
	if elapsed := time.Since(start); elapsed >= driveShareProbeTimeout {
		t.Errorf("GET /me took %v against a share with an outstanding probe — a display read must degrade, "+
			"not block on a mount that is not answering", elapsed)
	}
	if ud != nil {
		t.Errorf("/me offered user_drive = %v while the share has an unanswered probe outstanding: the launch "+
			"door refuses this same subject 422 share_unreachable, so the card would draw a checkbox for a "+
			"mount the create path will not make", ud)
	}
	if reason != driveUnavailableUnmountable {
		t.Errorf("user_drive_unavailable = %q, want %q — the launch door's own answer for a share that did "+
			"not answer", reason, driveUnavailableUnmountable)
	}
	if denied != "" {
		t.Errorf("user_drive_denied_by_profile = %q, want empty — no profile is involved", denied)
	}

	// AND THE TWO DOORS STILL AGREE, asserted rather than assumed: /me withheld
	// it because the launch would refuse it, not instead of the launch refusing.
	mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, ctx)
	if ok || mount != nil {
		t.Fatalf("the launch MOUNTED the drive /me withheld: %+v", mount)
	}
	if w.Code != 422 {
		t.Errorf("launch = %d, want 422", w.Code)
	}
}
