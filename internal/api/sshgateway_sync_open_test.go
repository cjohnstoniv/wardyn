// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func waitSyncOpen(t *testing.T, f *sshSyncFixture, want int) []runSyncOpen {
	t.Helper()
	var got []runSyncOpen
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if got = f.h.srv.runSyncOpenView(f.run.ID).Open; len(got) == want {
			return got
		}
	}
	t.Fatalf("sync.open = %v, want %d entries", got, want)
	return nil
}

func TestSSHGateway_SyncOpenFollowsTheLiveSessions(t *testing.T) {
	f := newSSHSyncFixture(t)
	if got := f.h.srv.runSyncOpenView(f.run.ID); got.Open == nil {
		t.Fatal("sync.open is nil with no session; it must marshal as []")
	}

	// A refused directory ends at once and is never listed as open.
	sess, in, _, err := f.openSync(t, map[string]string{"WARDYN_SYNC_DIR": "/etc"})
	if err != nil {
		t.Fatal(err)
	}
	_ = in.Close()
	_ = sess.Wait()
	waitForAudit(t, f.h.audit, f.run.ID, "ssh.sync.transfer", "failure")
	if got := f.h.srv.runSyncOpenView(f.run.ID).Open; len(got) != 0 {
		t.Fatalf("a refused sync is listed as open: %v", got)
	}

	_, in1, out1, err := f.openSync(t, map[string]string{"WARDYN_SYNC_DIR": "/home/agent/b"})
	if err != nil {
		t.Fatal(err)
	}
	syncEcho(t, in1, out1, "x")
	waitSyncOpen(t, f, 1)
	time.Sleep(2 * time.Millisecond)
	_, in2, out2, err := f.openSync(t, map[string]string{"WARDYN_SYNC_DIR": "/home/agent/a"})
	if err != nil {
		t.Fatal(err)
	}
	syncEcho(t, in2, out2, "x")
	got := waitSyncOpen(t, f, 2)
	if got[0].Dir != "/home/agent/b" || got[1].Dir != "/home/agent/a" || !got[0].OpenedAt.Before(got[1].OpenedAt) {
		t.Errorf("sync.open = %+v, want oldest first", got)
	}

	_ = in1.Close()
	if got := waitSyncOpen(t, f, 1); got[0].Dir != "/home/agent/a" {
		t.Errorf("after the first closed: %+v", got)
	}
	_ = in2.Close()
	waitSyncOpen(t, f, 0)
	raw, _ := json.Marshal(f.h.srv.runSyncOpenView(f.run.ID))
	if string(raw) != `{"open":[]}` {
		t.Errorf("empty wire shape = %s", raw)
	}
}

func TestGetRun_SyncOpenIsOnTheRunDetail(t *testing.T) {
	f, _ := newOwnerFixture(t)
	f.srv.cfg.Store = keptUntilStore{f.rs}
	open := func() []any {
		sync, ok := f.getRunBody(t)["sync"].(map[string]any)
		if !ok {
			t.Fatalf("sync absent from the run detail")
		}
		list, ok := sync["open"].([]any)
		if !ok {
			t.Fatalf("sync.open = %v, want a list (never null)", sync["open"])
		}
		return list
	}
	if got := open(); len(got) != 0 {
		t.Fatalf("sync.open = %v, want []", got)
	}
	release := f.srv.sshSyncOpened(f.run.ID, "/home/agent/work/app")
	other := f.srv.sshSyncOpened(uuid.New(), "/home/agent/elsewhere")
	defer other()
	got := open()
	if len(got) != 1 {
		t.Fatalf("sync.open = %v, want only this run's session", got)
	}
	e := got[0].(map[string]any)
	if _, err := time.Parse(time.RFC3339Nano, e["opened_at"].(string)); err != nil || e["dir"] != "/home/agent/work/app" || len(e) != 2 {
		t.Errorf("entry = %v", e)
	}
	release()
	if got := open(); len(got) != 0 {
		t.Errorf("after release: %v", got)
	}
}
