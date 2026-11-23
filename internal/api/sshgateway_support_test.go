// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A support helper over SSH: the run's owner accepted a grant, and an admin's key enters the run through the
// doors the owner ticked, governed as the owner is. sshgateway_support.go holds the arm.

const (
	sshSupportOwner  = "owner@example.com"
	sshSupportHelper = "root@example.com"
)

var sshAllSupportDoors = []string{"terminal", "commands", "files", "forward"}

// sshSupportStore is the gateway tests' run and key store with the support grant record on top.
type sshSupportStore struct {
	*sshMemStore
	*fakeSupportGrants
	failRun atomic.Bool // GetRun fails, as a store blip would
}

func (s *sshSupportStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if s.failRun.Load() {
		return types.AgentRun{}, errors.New("runs unavailable")
	}
	return s.sshMemStore.GetRun(ctx, id)
}

func newSSHSupportStore() *sshSupportStore {
	mem := newSSHMemStore()
	return &sshSupportStore{sshMemStore: mem, fakeSupportGrants: newFakeSupportGrants(func(id uuid.UUID) (types.AgentRun, bool) {
		mem.mu.Lock()
		defer mem.mu.Unlock()
		r, ok := mem.runs[id]
		return r, ok
	})}
}

type sshSupportRig struct {
	h     *sshTestHarness
	st    *sshSupportStore
	run   types.AgentRun
	priv  ed25519.PrivateKey
	key   types.SSHPublicKey
	grant store.SupportGrant
	execs *atomic.Int32
}

// newSSHSupportRig seeds a person's RUNNING run, an admin's fresh key and, unless doors is nil, an accepted
// grant ticking doors, then starts the gateway. It dials nothing.
func newSSHSupportRig(t *testing.T, doors []string, configure ...func(*Config)) *sshSupportRig {
	t.Helper()
	t.Setenv(envSupportSessions, "1")
	st := newSSHSupportStore()
	run := types.AgentRun{ID: uuid.New(), CreatedBy: sshSupportOwner, State: types.RunRunning, SandboxRef: "sbx-1"}
	st.putRun(run)
	priv, pub := mustSSHKeypair(t)
	now := time.Now().UTC()
	key := types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: sshSupportHelper, PublicKey: string(ssh.MarshalAuthorizedKey(pub)),
		CreatedAt: now, Role: oidc.RoleAdmin, RoleCheckedAt: &now}
	st.putKey(key)
	r := &sshSupportRig{st: st, run: run, priv: priv, key: key, execs: &atomic.Int32{}}
	if doors != nil {
		accepted := supportFakeEpoch
		expires := accepted.Add(time.Hour)
		r.grant = store.SupportGrant{ID: uuid.New(), RunID: run.ID, Helper: sshSupportHelper, DoorsAsked: doors, Doors: doors, Minutes: 60,
			RequestedAt: accepted, AcceptedAt: &accepted, ExpiresAt: &expires}
		st.put(r.grant)
	}
	fr := &sshFakeRunner{
		execFn:   func(runner.ExecSpec) (*runner.ExecSession, error) { r.execs.Add(1); return fakeEchoExecSession(), nil },
		attachFn: func() (runner.Session, error) { return newFakeShellSession(), nil },
	}
	r.h = newSSHTestHarness(t, st, fr, configure...)
	return r
}

func (r *sshSupportRig) dial(t *testing.T) (*ssh.Client, error) {
	t.Helper()
	return sshDial(t, r.h, r.run.ID.String(), r.priv)
}

// endGrant ends the grant's row as an owner's revoke does.
func (r *sshSupportRig) endGrant() {
	r.st.fakeSupportGrants.mu.Lock()
	defer r.st.fakeSupportGrants.mu.Unlock()
	r.st.fakeSupportGrants.end(0, "revoked", true)
}

// setKeepaliveForTest shrinks the probe tick for the rig's connections and puts it back.
func setKeepaliveForTest(t *testing.T, d time.Duration) {
	t.Helper()
	prev := setSSHKeepaliveEvery(d)
	t.Cleanup(func() { setSSHKeepaliveEvery(prev) })
}

// closedWithin reports whether the client's connection ended within d.
func closedWithin(client *ssh.Client, d time.Duration) bool {
	gone := make(chan struct{})
	go func() { _ = client.Wait(); close(gone) }()
	select {
	case <-gone:
		return true
	case <-time.After(d):
		return false
	}
}

// authFailure is the one ssh.authenticate failure row's data, or "" when none was written.
func (r *sshSupportRig) authFailure(t *testing.T) string {
	t.Helper()
	ev := waitForAudit(t, r.h.audit, r.run.ID, "ssh.authenticate", "failure")
	if ev == nil {
		return ""
	}
	return string(ev.Data)
}

// (a) A live grant with an SSH door admits the helper's key, and the success row names the grant. Auth stamps
// no last_seen_at (check-2 L2); the first re-check, which passes the grant id, does.
func TestSSHSupport_AuthAdmitsALiveGrantAndStampsOnRecheck(t *testing.T) {
	r := newSSHSupportRig(t, sshAllSupportDoors)
	client, err := r.dial(t)
	if err != nil {
		t.Fatalf("a helper with a live grant was refused: %v", err)
	}
	defer client.Close()
	ev := waitForAudit(t, r.h.audit, r.run.ID, "ssh.authenticate", "success")
	if ev == nil {
		t.Fatalf("no ssh.authenticate success row; events=%s", auditDump(r.h.audit.snapshot(), r.run.ID))
	}
	d := sshAuditData(t, ev)
	if d["support_grant"] != r.grant.ID.String() || d["entry"] != "support" {
		t.Errorf("success row data = %v, want support_grant %s and entry support", d, r.grant.ID)
	}
	if _, has := d["override"]; has {
		t.Errorf("a helper's row carries override: %v", d)
	}
	for _, c := range r.st.liveCalls() {
		if c.Stamp {
			t.Errorf("auth read the grant with stamp %+v, want no last_seen_at stamp from auth", c)
		}
	}
	if g, _ := r.st.row(r.grant.ID); g.LastSeenAt != nil {
		t.Fatalf("last_seen_at = %v after auth alone, want nil", g.LastSeenAt)
	}
	checkSSHNewChannels(t, client, true) // each channel open re-checks, and the re-check names the grant
	var stamped bool
	for _, c := range r.st.liveCalls() {
		stamped = stamped || (c.Stamp && c.Grant == r.grant.ID)
	}
	if g, _ := r.st.row(r.grant.ID); !stamped || g.LastSeenAt == nil {
		t.Errorf("after a channel open: stamped call = %v, last_seen_at = %v, want the re-check to stamp", stamped, g.LastSeenAt)
	}
}

// (b), (c) No grant, a grant that ticks only web doors, a requested-only grant and the switch off are all the
// owner rule's refusal, run_owner_only.
func TestSSHSupport_AuthRefusedWithoutAnSSHDoor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		doors []string
		tweak func(*sshSupportRig)
		env   string
	}{
		{"no grant", nil, nil, ""},
		{"web doors only", []string{"web_apps", "desktop"}, nil, ""},
		{"requested only", sshAllSupportDoors, func(r *sshSupportRig) {
			r.st.fakeSupportGrants.mu.Lock()
			r.st.fakeSupportGrants.list[0].AcceptedAt, r.st.fakeSupportGrants.list[0].ExpiresAt = nil, nil
			r.st.fakeSupportGrants.mu.Unlock()
		}, ""},
		{"switch off", sshAllSupportDoors, nil, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSSHSupportRig(t, tc.doors)
			if tc.env != "" {
				t.Setenv(envSupportSessions, tc.env)
			}
			if tc.tweak != nil {
				tc.tweak(r)
			}
			if client, err := r.dial(t); err == nil {
				_ = client.Close()
				t.Fatal("the helper's key opened a connection, want refused")
			}
			if got := r.authFailure(t); !strings.Contains(got, "run_owner_only") {
				t.Errorf("failure row = %q, want reason run_owner_only", got)
			}
		})
	}
}

// sshSupportTry opens one channel kind on client and returns what the client was told when it was refused, or ""
// when the gateway served it.
type sshSupportTry func(t *testing.T, c *ssh.Client) string

func sshTryFor(kind string) sshSupportTry {
	return func(t *testing.T, c *ssh.Client) string {
		t.Helper()
		if kind == "forward" {
			conn, err := c.Dial("tcp", "127.0.0.1:9999")
			if err == nil {
				_ = conn.Close()
				return ""
			}
			var oce *ssh.OpenChannelError
			if errors.As(err, &oce) {
				return oce.Message
			}
			return err.Error()
		}
		sess, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sess.Close() })
		switch kind {
		case "shell":
			if _, err := sess.StdinPipe(); err != nil {
				t.Fatal(err)
			}
			err = sess.Shell()
		case "exec":
			err = sess.Start("cat /etc/hostname")
		case "sftp":
			err = sess.RequestSubsystem("sftp")
		case "sync":
			// A refused sync is acked and then ended with the sentence, so it is read off stderr.
			if err = sess.RequestSubsystem(sshSyncSubsystem); err == nil {
				if got := stderrOf(sess); strings.Contains(got, "did not allow") {
					return got
				}
			}
		}
		if err != nil {
			return stderrOf(sess) + " " + err.Error()
		}
		return ""
	}
}

// (d) Each kind is refused with its door unticked (the one sentence, one ssh.channel.reject row with reason
// support_door_not_granted, no exec) and admitted with it ticked. Sync follows the files door, as sftp does.
func TestSSHSupport_EachKindFollowsItsDoor(t *testing.T) {
	for _, tc := range []struct {
		kind, door, channelType string
		word                    string
	}{
		{"shell", "terminal", "shell", "a terminal"},
		{"exec", "commands", "exec", "commands (exec)"},
		{"sftp", "files", "sftp", "file transfer (sftp)"},
		{"sync", "files", "sync", "file transfer (sftp)"},
		{"forward", "forward", "direct-tcpip", "port forwarding"},
	} {
		others := []string{}
		for _, d := range sshAllSupportDoors {
			if d != tc.door {
				others = append(others, d)
			}
		}
		t.Run(tc.kind+"/unticked", func(t *testing.T) {
			r := newSSHSupportRig(t, others)
			client, err := r.dial(t)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer client.Close()
			got := sshTryFor(tc.kind)(t, client)
			if want := "wardyn: the run's owner did not allow " + tc.word + " in this support session"; !strings.Contains(got, want) {
				t.Errorf("the helper was told %q, want it to hold %q", got, want)
			}
			ev := waitForAudit(t, r.h.audit, r.run.ID, "ssh.channel.reject", "failure")
			if ev == nil {
				t.Fatalf("no ssh.channel.reject row; events=%s", auditDump(r.h.audit.snapshot(), r.run.ID))
			}
			if d := sshAuditData(t, ev); d["channel_type"] != tc.channelType || d["reason"] != "support_door_not_granted" || d["support_grant"] != r.grant.ID.String() {
				t.Errorf("row data = %v, want channel_type %q, reason support_door_not_granted and the grant", d, tc.channelType)
			}
			if n := r.execs.Load(); n != 0 {
				t.Errorf("%d execs reached the sandbox through a door that is not ticked", n)
			}
		})
		t.Run(tc.kind+"/ticked", func(t *testing.T) {
			r := newSSHSupportRig(t, []string{tc.door})
			client, err := r.dial(t)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer client.Close()
			if got := sshTryFor(tc.kind)(t, client); got != "" {
				t.Errorf("a ticked %s door was refused: %q", tc.door, got)
			}
			if ev := findAudit(r.h.audit.snapshot(), r.run.ID, "ssh.channel.reject", "failure"); ev != nil {
				t.Errorf("a ticked door wrote a reject row: %s", ev.Data)
			}
		})
	}
}

// The channel switch still binds a helper: a kind the operator left out is refused as channel_off even when the
// owner ticked its door, and the row says so.
func TestSSHSupport_ChannelSwitchStillBinds(t *testing.T) {
	r := newSSHSupportRig(t, sshAllSupportDoors, func(c *Config) { c.SSHChannels = map[string]bool{"shell": true} })
	client, err := r.dial(t)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	if got := sshTryFor("exec")(t, client); !strings.Contains(got, "WARDYN_SSH_CHANNELS") {
		t.Errorf("exec with the switch off for a helper: %q, want the switch's sentence", got)
	}
	ev := waitForAudit(t, r.h.audit, r.run.ID, "ssh.channel.reject", "failure")
	if ev == nil || sshAuditData(t, ev)["reason"] != "channel_off" {
		t.Fatalf("reject row = %v, want reason channel_off", ev)
	}
}

// (e) deny_interactive binds a helper even with GovernAdminRuns off, where an admin's own run would take the
// exemption (H1). The owner's profile closes the door for the helper as it does for the owner.
func TestSSHSupport_DenyInteractiveBindsAHelper(t *testing.T) {
	for _, govern := range []bool{false, true} {
		name := "GovernAdminRuns off"
		if govern {
			name = "GovernAdminRuns on"
		}
		t.Run(name, func(t *testing.T) {
			r := newSSHSupportRig(t, sshAllSupportDoors, func(c *Config) { c.GovernAdminRuns = govern })
			profile := types.GovernanceUnit{ID: uuid.New(), Name: "ci", Limits: types.GovernanceLimits{DenyInteractive: true}}
			r.st.sshMemStore.profiles = []types.GovernanceUnit{profile}
			r.run.UnitID = &profile.ID
			r.st.putRun(r.run)
			if client, err := r.dial(t); err == nil {
				_ = client.Close()
				t.Fatal("a helper entered a deny_interactive run, want refused")
			}
			if got := r.authFailure(t); !strings.Contains(got, `governance profile \"ci\" denies interactive sessions`) {
				t.Errorf("failure row = %q, want the profile named", got)
			}
		})
	}
}

// (f) A helper's key must still pass the admin-override checks: capped, no longer an admin, or a role stamp
// older than the TTL or never set is refused at auth and, on an open connection, at the next channel (M7).
func TestSSHSupport_AdminKeyRulesStillBind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*types.SSHPublicKey)
	}{
		{"capped", func(k *types.SSHPublicKey) { k.Capped = true }},
		{"not an admin", func(k *types.SSHPublicKey) { k.Role = oidc.RoleUser }},
		{"stale stamp", func(k *types.SSHPublicKey) { old := time.Now().Add(-2 * defaultSSHRoleTTL); k.RoleCheckedAt = &old }},
		{"missing stamp", func(k *types.SSHPublicKey) { k.RoleCheckedAt = nil }},
	} {
		t.Run(tc.name+"/auth", func(t *testing.T) {
			r := newSSHSupportRig(t, sshAllSupportDoors)
			tc.change(&r.key)
			r.st.putKey(r.key)
			if client, err := r.dial(t); err == nil {
				_ = client.Close()
				t.Fatal("a key that fails the admin rules entered through a grant, want refused")
			}
		})
		t.Run(tc.name+"/open connection", func(t *testing.T) {
			r := newSSHSupportRig(t, sshAllSupportDoors)
			client, err := r.dial(t)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer client.Close()
			checkSSHNewChannels(t, client, true)
			tc.change(&r.key)
			r.st.putKey(r.key)
			checkSSHNewChannels(t, client, false)
		})
	}
}

// (g) A revoke or an expiry refuses the next channel at once and closes the connection on the next probe tick.
func TestSSHSupport_RevokeAndExpiryCutTheConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		cut  func(*sshSupportRig)
	}{
		{"revoke", func(r *sshSupportRig) { r.endGrant() }},
		{"expiry", func(r *sshSupportRig) { r.st.tick(61 * time.Minute) }},
		{"run ended", func(r *sshSupportRig) {
			r.run.State = types.RunStopped
			r.st.putRun(r.run)
		}},
	} {
		t.Run(tc.name+"/new channels", func(t *testing.T) {
			r := newSSHSupportRig(t, sshAllSupportDoors)
			client, err := r.dial(t)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer client.Close()
			checkSSHNewChannels(t, client, true)
			tc.cut(r)
			checkSSHNewChannels(t, client, false)
			ev := waitForAudit(t, r.h.audit, r.run.ID, "ssh.channel.reject", "failure")
			if ev == nil || !strings.Contains(string(ev.Data), sshSupportEnded) || !strings.Contains(string(ev.Data), `"entry":"support"`) ||
				!strings.Contains(string(ev.Data), r.grant.ID.String()) {
				t.Errorf("reject row = %v, want reason %q, entry support and the grant", ev, sshSupportEnded)
			}
			_, _, err = client.OpenChannel("session", nil)
			var oce *ssh.OpenChannelError
			if !errors.As(err, &oce) || oce.Message != "the support session ended" {
				t.Errorf("a refused channel told the helper %v, want %q", err, "the support session ended")
			}
		})
		t.Run(tc.name+"/idle connection", func(t *testing.T) {
			setKeepaliveForTest(t, 50*time.Millisecond)
			r := newSSHSupportRig(t, sshAllSupportDoors)
			client, err := r.dial(t)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer client.Close()
			time.Sleep(200 * time.Millisecond)
			if closedWithin(client, 10*time.Millisecond) {
				t.Fatal("the connection closed while its grant was live")
			}
			tc.cut(r)
			if !closedWithin(client, 3*time.Second) {
				t.Fatal("the connection outlived its ended grant by more than the probe ticks")
			}
		})
	}
}

// (h) A failed grant read closes an idle helper's connection on the tick; a failed key read does too (the
// strict mapping). The same failed key read on an owner's idle connection keeps it, unchanged.
func TestSSHSupport_FailedReadClosesAHelperButNotAnOwner(t *testing.T) {
	setKeepaliveForTest(t, 50*time.Millisecond)
	r := newSSHSupportRig(t, sshAllSupportDoors)
	client, err := r.dial(t)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	time.Sleep(200 * time.Millisecond)
	r.st.failOn("SupportGrantLive", errors.New("grants unavailable"))
	if !closedWithin(client, 3*time.Second) {
		t.Fatal("a failed grant read left a helper's idle connection open")
	}

	mem, run, principal := sshOwnedRunningRun(t)
	st := &sshKeyReadFailureStore{sshMemStore: mem}
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{})
	owner, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("owner dial: %v", err)
	}
	defer owner.Close()
	st.failed.Store(true)
	if closedWithin(owner, 500*time.Millisecond) {
		t.Fatal("a failed key read closed an owner's idle connection, which is unchanged behaviour")
	}
}

// A failed run read on a helper's idle connection closes it on the tick (sshSupportStrict through the probe), as
// the grant read does.
func TestSSHSupport_FailedRunReadClosesAHelper(t *testing.T) {
	setKeepaliveForTest(t, 50*time.Millisecond)
	r := newSSHSupportRig(t, sshAllSupportDoors)
	client, err := r.dial(t)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	time.Sleep(200 * time.Millisecond)
	if closedWithin(client, 10*time.Millisecond) {
		t.Fatal("the connection closed before the run read failed")
	}
	r.st.failRun.Store(true)
	if !closedWithin(client, 3*time.Second) {
		t.Fatal("a failed run read left a helper's idle connection open")
	}
}

// (j) The owner's web attach takes the writer seat from a helper's SSH shell: the helper's channel reads "The
// run's owner joined", one session.takeover row has reason owner_joined, and the shell was seated as a helper's.
// Every row of the helper's shell, its two session.attach rows and its session.detach, names the grant and the
// support entry.
func TestSSHSupport_OwnerAttachDisplacesAHelperShell(t *testing.T) {
	r := newSSHSupportRig(t, []string{"terminal"})
	client, err := r.dial(t)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	srv := r.h.srv
	deadline := time.Now().Add(3 * time.Second)
	for {
		if w := srv.attachHolderFor(r.run.ID); w != nil && w.support == r.grant.ID && w.principal == sshSupportHelper {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper's shell never took the writer seat as a support holder")
		}
		time.Sleep(10 * time.Millisecond)
	}

	owner := &attachHolder{principal: sshSupportOwner, actorType: types.ActorHuman, since: time.Now().UTC(), source: attachSourceWeb,
		sandboxRef: r.run.SandboxRef, displace: func(string) {}, notify: func(bool, *attachHolder) {}}
	if readOnly, _ := srv.registerAttachHolder(r.run.ID, owner); readOnly || !owner.writable.Load() {
		t.Fatalf("the owner joined read-only (%v), want the seat", readOnly)
	}
	got := make(chan string, 1)
	go func() { b, _ := io.ReadAll(stderr); got <- string(b) }()
	select {
	case s := <-got:
		if !strings.Contains(s, "wardyn: The run's owner joined") {
			t.Errorf("the helper's channel read %q, want %q", s, "wardyn: The run's owner joined")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the helper's shell was never displaced")
	}
	ev := waitForAudit(t, r.h.audit, r.run.ID, "session.takeover", "success")
	if ev == nil || ev.Actor != sshSupportOwner || !strings.Contains(string(ev.Data), `"reason":"owner_joined"`) || !strings.Contains(string(ev.Data), `"previous_holder":"`+sshSupportHelper+`"`) {
		t.Errorf("session.takeover row = %+v, want the owner's with reason owner_joined naming the helper", ev)
	}
	waitForActorAudit(t, r.h.audit, r.run.ID, "session.detach", sshSupportHelper)
	rows := map[string]int{}
	for _, ev := range r.h.audit.snapshot() {
		if ev.Actor != sshSupportHelper || (ev.Action != "session.attach" && ev.Action != "session.detach") {
			continue
		}
		rows[ev.Action]++
		if d := uiRowData(t, ev); d["entry"] != supportEntryHelper || d["support_grant"] != r.grant.ID.String() {
			t.Errorf("the helper's %s %s row = %s, want entry %s and support_grant %s", ev.Action, ev.Outcome, ev.Data, supportEntryHelper, r.grant.ID)
		}
	}
	if rows["session.attach"] != 2 || rows["session.detach"] != 1 {
		t.Errorf("the helper's shell wrote %v, want two session.attach rows and one session.detach", rows)
	}
}

// takeoverGate is an audit recorder that holds the session.takeover row's writer until hold closes. The
// owner's claim displaces the helper only after that row is written (evictSupportWriter), so while the writer
// is held the helper is evicted but not yet told why.
type takeoverGate struct {
	*sshTestRecorder
	seen chan struct{} // closed once the takeover row is in
	hold <-chan struct{}
}

func (g *takeoverGate) Record(ctx context.Context, ev types.AuditEvent) error {
	err := g.sshTestRecorder.Record(ctx, ev)
	if ev.Action == "session.takeover" {
		close(g.seen)
		<-g.hold
	}
	return err
}

// The owner joins while the helper's shell is still opening (#4184): the helper's Attach returns only after it
// was evicted, and the displacement's line is held back until the helper's channel has closed or 300ms have
// passed. The helper still reads why: its shell waits for the displacement before the channel closes.
func TestSSHSupport_HelperDisplacedWhileItsShellOpensReadsWhy(t *testing.T) {
	hold := make(chan struct{})
	gate := &takeoverGate{seen: make(chan struct{}), hold: hold}
	r := newSSHSupportRig(t, []string{"terminal"}, func(c *Config) {
		gate.sshTestRecorder = c.Audit.(*sshTestRecorder)
		c.Audit = gate
	})
	fr := r.h.srv.cfg.Runner.(*sshFakeRunner)
	fr.mu.Lock()
	fr.attachFn = func() (runner.Session, error) { <-gate.seen; return newFakeShellSession(), nil }
	fr.mu.Unlock()
	client, err := r.dial(t)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	closed := make(chan struct{})
	go func() { _ = sess.Wait(); close(closed) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if w := r.h.srv.attachHolderFor(r.run.ID); w != nil && w.support == r.grant.ID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper's shell never took the writer seat as a support holder")
		}
		time.Sleep(10 * time.Millisecond)
	}
	go func() {
		select {
		case <-closed:
		case <-time.After(300 * time.Millisecond):
		}
		close(hold)
	}()
	owner := &attachHolder{principal: sshSupportOwner, actorType: types.ActorHuman, since: time.Now().UTC(), source: attachSourceWeb,
		sandboxRef: r.run.SandboxRef, displace: func(string) {}, notify: func(bool, *attachHolder) {}}
	if readOnly, _ := r.h.srv.registerAttachHolder(r.run.ID, owner); readOnly {
		t.Fatal("the owner joined read-only, want the seat")
	}
	got := make(chan string, 1)
	go func() { b, _ := io.ReadAll(stderr); got <- string(b) }()
	select {
	case s := <-got:
		if !strings.Contains(s, "wardyn: "+supportOwnerJoinedReason) {
			t.Errorf("the helper's channel read %q, want %q", s, "wardyn: "+supportOwnerJoinedReason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the helper's shell was never displaced")
	}
}

// A helper's shell refused at the session cap writes the cap row naming its grant and the support entry, on the
// ssh transport, as the helper's other rows do.
func TestSSHSupport_CapRefusedShellNamesItsGrant(t *testing.T) {
	r := newSSHSupportRig(t, []string{"terminal"})
	for i := range maxAttachSessionsPerRun {
		if _, ok := r.h.srv.reserveAttachSlot(r.run, "holder-"+strings.Repeat("x", i+1)); !ok {
			t.Fatalf("slot %d refused before the cap", i)
		}
	}
	client, err := r.dial(t)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	_ = sess.Shell() // refused at the cap: the channel is told and closed
	ev := waitForAudit(t, r.h.audit, r.run.ID, "session.attach", "failure")
	if ev == nil {
		t.Fatal("no session.attach failure row for the capped shell")
	}
	d := uiRowData(t, *ev)
	if ev.Actor != sshSupportHelper || d["reason"] != reasonAttachSessionCapReached || d["transport"] != "ssh" ||
		d["support_grant"] != r.grant.ID.String() || d["entry"] != supportEntryHelper {
		t.Errorf("cap row = %s by %s, want reason %s, transport ssh, support_grant %s and entry %s", ev.Data, ev.Actor, reasonAttachSessionCapReached, r.grant.ID, supportEntryHelper)
	}
}

// A helper promoted from the queue to the writer seat is written as a helper: the session.promote row names the
// grant and the support entry.
func TestSSHSupport_PromotedHelperRowNamesTheEntry(t *testing.T) {
	rec := &sshTestRecorder{}
	srv := New(Config{Audit: rec, AdminToken: adminToken})
	runID, grant := uuid.New(), uuid.New()
	owner := &attachHolder{principal: sshSupportOwner, actorType: types.ActorHuman, source: attachSourceWeb, since: time.Now()}
	helper := &attachHolder{principal: sshSupportHelper, actorType: types.ActorHuman, source: attachSourceSSH, since: time.Now(),
		support: grant, notify: func(bool, *attachHolder) {}}
	_, releaseOwner := srv.registerAttachHolder(runID, owner)
	if readOnly, _ := srv.registerAttachHolder(runID, helper); !readOnly {
		t.Fatal("the helper took the seat the owner holds")
	}
	announce := releaseOwner()
	if announce == nil {
		t.Fatal("the owner left with the helper queued and nothing was promoted")
	}
	announce()
	ev := waitForActorAudit(t, rec, runID, "session.promote", sshSupportHelper)
	if d := uiRowData(t, *ev); d["entry"] != supportEntryHelper || d["support_grant"] != grant.String() {
		t.Errorf("session.promote row = %s, want entry %s and support_grant %s", ev.Data, supportEntryHelper, grant)
	}
}

// sshSupportStrict maps a store-read failure to a closing reason for a helper only.
func TestSSHSupportStrictValueList(t *testing.T) {
	grant := &ssh.Permissions{Extensions: map[string]string{sshExtSupportGrant: uuid.NewString()}}
	override := &ssh.Permissions{Extensions: map[string]string{sshExtOverride: "true"}}
	owner := &ssh.Permissions{Extensions: map[string]string{}}
	for _, tc := range []struct {
		name   string
		perms  *ssh.Permissions
		reason string
		want   string
	}{
		{"helper, key lookup failed", grant, sshReasonKeyLookup, sshSupportUnverified},
		{"helper, run lookup failed", grant, sshReasonRunLookup, sshSupportUnverified},
		{"helper, cutoff lookup failed", grant, sshReasonCutoffLookup, sshSupportUnverified},
		{"helper, key deleted", grant, "SSH key registration changed", "SSH key registration changed"},
		{"helper, grant ended", grant, sshSupportEnded, sshSupportEnded},
		{"helper, fine", grant, "", ""},
		{"override, key lookup failed", override, sshReasonKeyLookup, sshReasonKeyLookup},
		{"owner, run lookup failed", owner, sshReasonRunLookup, sshReasonRunLookup},
	} {
		if got := sshSupportStrict(tc.perms, tc.reason); got != tc.want {
			t.Errorf("%s: sshSupportStrict = %q, want %q", tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		v    supportVerdict
		want string
	}{
		{supportOK, ""}, {supportEnded, sshSupportEnded}, {supportNone, sshSupportEnded}, {supportDoorRefused, sshSupportEnded},
		{supportOff, sshSupportEnded}, {supportUnavailable, sshSupportUnverified},
	} {
		if got := sshSupportReason(tc.v); got != tc.want {
			t.Errorf("sshSupportReason(%d) = %q, want %q", tc.v, got, tc.want)
		}
	}
}

// (i) A helper with the forward door still cannot reach a macOS VM's Screen Sharing ports, and a forward to the
// sandbox's own loopback works.
func TestSSHSupport_ForwardStillRefusesScreenSharing(t *testing.T) {
	r := newSSHSupportRig(t, []string{"forward"})
	r.run.MachineKind = types.MachineMacOSVM
	r.st.putRun(r.run)
	client, err := r.dial(t)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	for _, port := range []string{"5900", "5905", "5909"} {
		if conn, err := client.Dial("tcp", "127.0.0.1:"+port); err == nil {
			_ = conn.Close()
			t.Errorf("a helper's forward to %s succeeded, want refused", port)
		}
	}
	if n := r.execs.Load(); n != 0 {
		t.Fatalf("a refused Screen Sharing forward ran %d execs", n)
	}
	conn, err := client.Dial("tcp", "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("a helper's forward to the sandbox loopback: %v", err)
	}
	_ = conn.Close()
}

// An owner's and an override's connection carry no support field and keep their rows: the owner's success row has
// no data, an admin key on an operator-owned run has override and no support_grant.
func TestSSHSupport_OwnerAndOverrideRowsUnchanged(t *testing.T) {
	r := newSSHSupportRig(t, sshAllSupportDoors)
	ownerPriv, ownerPub := mustSSHKeypair(t)
	r.st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(ownerPub), Principal: sshSupportOwner, PublicKey: string(ssh.MarshalAuthorizedKey(ownerPub))})
	opRun := types.AgentRun{ID: uuid.New(), CreatedBy: "admin-token", OperatorOwned: true, State: types.RunRunning, SandboxRef: "sbx-2"}
	r.st.putRun(opRun)

	owner, err := sshDial(t, r.h, r.run.ID.String(), ownerPriv)
	if err != nil {
		t.Fatalf("owner dial: %v", err)
	}
	defer owner.Close()
	op, err := sshDial(t, r.h, opRun.ID.String(), r.priv)
	if err != nil {
		t.Fatalf("override dial: %v", err)
	}
	defer op.Close()

	ev := waitForAudit(t, r.h.audit, r.run.ID, "ssh.authenticate", "success")
	if ev == nil || strings.Contains(string(ev.Data), "override") || strings.Contains(string(ev.Data), "support_grant") {
		t.Errorf("owner row = %+v, want a success row that names neither an override nor a grant", ev)
	}
	ev = waitForAudit(t, r.h.audit, opRun.ID, "ssh.authenticate", "success")
	if ev == nil {
		t.Fatal("no override success row")
	}
	if d := sshAuditData(t, ev); d["override"] != true || d["support_grant"] != nil {
		t.Errorf("override row data = %v, want override true and no support_grant", d)
	}
	for _, c := range r.st.liveCalls() {
		if c.Run == opRun.ID {
			t.Errorf("an operator-owned run's auth read a support grant: %+v", c)
		}
	}
}

// sshSupportDoorOfKind and sshSupportDoorRefused fail closed: a kind with no door is refused to a helper even when
// the grant's door list holds the empty name, an empty support_doors extension is no doors at all, and a
// connection that is not a helper's is never refused here.
func TestSSHSupportDoorRefusedFailsClosed(t *testing.T) {
	const unmapped sshChannelKind = "agent-forward"
	if got := sshSupportDoorOfKind(unmapped); got != "" {
		t.Fatalf("sshSupportDoorOfKind(%q) = %q, want no door", unmapped, got)
	}
	for kind, want := range map[sshChannelKind]string{sshKindShell: "terminal", sshKindExec: "commands", sshKindSFTP: "files", sshKindSync: "files", sshKindForward: "forward"} {
		if got := sshSupportDoorOfKind(kind); got != want {
			t.Errorf("sshSupportDoorOfKind(%q) = %q, want %q", kind, got, want)
		}
	}
	entry := func(doors ...string) context.Context {
		return context.WithValue(context.Background(), sshSupportCtxKey{}, sshSupportEntry{grant: uuid.New(), doors: doors})
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		kind sshChannelKind
		want bool
	}{
		{"unmapped kind, every door ticked", entry(sshAllSupportDoors...), unmapped, true},
		{"unmapped kind, the empty door listed", entry(""), unmapped, true},
		{"mapped kind, door ticked", entry("files"), sshKindSync, false},
		{"mapped kind, door not ticked", entry("files"), sshKindExec, true},
		{"no doors", entry(), sshKindShell, true},
		{"not a helper", context.Background(), unmapped, false},
	} {
		if got := sshSupportDoorRefused(tc.ctx, tc.kind); got != tc.want {
			t.Errorf("%s: sshSupportDoorRefused = %v, want %v", tc.name, got, tc.want)
		}
	}
	// An empty extension is no doors: withSupportDoors must not turn "" into the one door named "".
	perms := &ssh.Permissions{Extensions: map[string]string{sshExtSupportGrant: uuid.NewString(), sshExtSupportDoors: ""}}
	ctx := withSupportDoors(context.Background(), perms)
	if e, ok := sshSupportOf(ctx); !ok || len(e.doors) != 0 {
		t.Errorf("an empty support_doors gave doors %q (helper %v), want none", e.doors, ok)
	}
	for _, kind := range []sshChannelKind{sshKindShell, sshKindExec, sshKindSFTP, sshKindSync, sshKindForward, unmapped} {
		if !sshSupportDoorRefused(ctx, kind) {
			t.Errorf("a grant with no doors admitted %q", kind)
		}
	}
	for reason, want := range map[string]string{
		sshSupportEnded: "the support session ended", sshSupportUnverified: "the support session could not be verified",
		"SSH key registration changed": "SSH key is no longer authorized", runOwnerOnlyReason: "SSH key is no longer authorized",
	} {
		if got := sshRejectMessage(reason); got != want {
			t.Errorf("sshRejectMessage(%q) = %q, want %q", reason, got, want)
		}
	}
}

// A support-grant extension that is present and not a grant id is refused: it closes at the re-check and its
// channels have no doors, so it is never read as the run's owner. A good one passes.
func TestSSHSupportUnreadableGrantRefuses(t *testing.T) {
	r := newSSHSupportRig(t, sshAllSupportDoors)
	pub, err := ssh.NewPublicKey(r.priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	perms, err := r.h.srv.sshAuth(fakeSSHConn{user: r.run.ID.String()}, pub)
	if err != nil {
		t.Fatalf("setup: the helper's auth: %v", err)
	}
	if reason := r.h.srv.sshCurrentKey(context.Background(), perms); reason != "" {
		t.Fatalf("a good grant extension re-checked as %q, want it to stand", reason)
	}
	if sshSupportGrantUnreadable(perms) {
		t.Fatal("a good grant extension reads as unreadable")
	}
	// An id that does not parse, and the nil id, which parses but names no grant.
	for _, bad := range []string{"not-a-grant-id", uuid.Nil.String()} {
		perms.Extensions[sshExtSupportGrant] = bad
		if !sshSupportGrantUnreadable(perms) {
			t.Fatalf("grant extension %q reads as readable", bad)
		}
		if reason := r.h.srv.sshCurrentKey(context.Background(), perms); reason != sshSupportUnverified {
			t.Errorf("grant extension %q re-checked as %q, want %q", bad, reason, sshSupportUnverified)
		}
		ctx := withSupportDoors(context.Background(), perms)
		if _, helper := sshSupportOf(ctx); !helper {
			t.Fatalf("grant extension %q gave a context that reads as the owner's", bad)
		}
		for _, kind := range []sshChannelKind{sshKindShell, sshKindExec, sshKindSFTP, sshKindSync, sshKindForward} {
			if !sshSupportDoorRefused(ctx, kind) {
				t.Errorf("grant extension %q admitted %q", bad, kind)
			}
		}
	}
	if sshSupportGrantUnreadable(&ssh.Permissions{Extensions: map[string]string{}}) {
		t.Error("an owner's connection reads as an unreadable grant")
	}
}
