// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// closedBeforeAnyByte reports whether the server closed c without sending its
// version banner.
func closedBeforeAnyByte(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(make([]byte, 1))
	return n == 0 && err != nil
}

// One source address holds at most 16 (the documented default) of the
// handshake slots: the 17th connection from it is closed before any byte,
// while the slots it leaves stay free for every other source.
func TestSSHGateway_HandshakesPerSourceBounded(t *testing.T) {
	h := newSSHTestHarness(t, newSSHMemStore(), &sshFakeRunner{})
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := range 16 {
		c, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	over, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial over the per-source bound: %v", err)
	}
	defer over.Close()
	if !closedBeforeAnyByte(over) {
		t.Fatal("17th handshake from one source got a byte or stayed open, want it closed before any byte")
	}
	_ = conns[0].Close()
	conns = conns[1:]
	// The freed slot is taken back once the server sees the close; until then a
	// dial may still meet the full bound, so retry within the handshake window.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 4)
		n, _ := io.ReadFull(c, buf)
		_ = c.Close()
		if n == 4 && string(buf) == "SSH-" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a slot freed by a closed handshake was never given back to its source")
		}
	}
}

// The established-connection ceiling is configurable; a connection that
// authenticates over it is closed, and the refusal is counted by bound.
func TestSSHGateway_MaxConnectionsConfigurableAndCounted(t *testing.T) {
	st := newSSHMemStore()
	runID := uuid.New()
	st.putRun(types.AgentRun{ID: runID, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1"})
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: "alice@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{}, func(c *Config) { c.SSHMaxConnections = 1 })

	first, err := sshDial(t, h, runID.String(), priv)
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	defer first.Close()
	if second, err := sshDial(t, h, runID.String(), priv); err == nil {
		_, _, serr := second.SendRequest("keepalive@openssh.com", true, nil)
		_ = second.Close()
		if serr == nil {
			t.Fatal("second session over a ceiling of 1 was served, want it closed")
		}
	}
	h.srv.metrics.mu.Lock()
	got := h.srv.metrics.sshRefused[sshBoundEstablished]
	h.srv.metrics.mu.Unlock()
	if got != 1 {
		t.Fatalf("established refusals counted = %d, want 1", got)
	}
	if _, _, err := first.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("first session after the refusal: %v", err)
	}
}

// With the default ceiling, DefaultSSHMaxConnections authenticated sessions are
// served and the next is closed after its key was accepted; closing one session
// frees its slot for a new connection.
func TestSSHGateway_DefaultCeilingRefusesTheNextEstablishedConnection(t *testing.T) {
	st := newSSHMemStore()
	runID := uuid.New()
	st.putRun(types.AgentRun{ID: runID, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1"})
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: "alice@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	// No SSO is configured, so the one principal's share is the whole ceiling: all 64 are served.
	h := newSSHTestHarness(t, st, &sshFakeRunner{})

	served := func(c *ssh.Client) bool {
		_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
		return err == nil
	}
	var clients []*ssh.Client
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	for i := 0; i < DefaultSSHMaxConnections; i++ {
		c, err := sshDial(t, h, runID.String(), priv)
		if err != nil {
			t.Fatalf("session %d of %d: %v", i+1, DefaultSSHMaxConnections, err)
		}
		clients = append(clients, c)
		// A keepalive answer means the server holds this session's slot, so the
		// next dial cannot overtake it.
		if !served(c) {
			t.Fatalf("session %d of %d was not served", i+1, DefaultSSHMaxConnections)
		}
	}

	if over, err := sshDial(t, h, runID.String(), priv); err == nil {
		ok := served(over)
		_ = over.Close()
		if ok {
			t.Fatalf("session %d was served, want it closed", DefaultSSHMaxConnections+1)
		}
	}
	h.srv.metrics.mu.Lock()
	got := h.srv.metrics.sshRefused[sshBoundEstablished]
	h.srv.metrics.mu.Unlock()
	if got != 1 {
		t.Fatalf("established refusals counted = %d, want 1", got)
	}

	_ = clients[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := sshDial(t, h, runID.String(), priv)
		if err == nil {
			ok := served(c)
			if ok {
				clients = append(clients, c)
				break
			}
			_ = c.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("a session closed at the ceiling never freed its slot")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// One person holds at most their share of the authenticated slots: a third connection from the
// same principal (a second key counts too) is closed and counted under "principal", while another
// person is still served.
func TestSSHGateway_ConnectionsPerPrincipalBounded(t *testing.T) {
	st := newSSHMemStore()
	runID := uuid.New()
	st.putRun(types.AgentRun{ID: runID, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1"})
	bobRun := uuid.New()
	st.putRun(types.AgentRun{ID: bobRun, CreatedBy: "bob@example.com", State: types.RunRunning, SandboxRef: "sbx-2"})
	alicePriv, alicePub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(alicePub), Principal: "alice@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(alicePub))})
	bobPriv, bobPub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(bobPub), Principal: "bob@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(bobPub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{}, func(c *Config) { c.SSHMaxConnectionsPerPrincipal = 2 })

	served := func(c *ssh.Client) bool {
		_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
		return err == nil
	}
	for i := range 2 {
		c, err := sshDial(t, h, runID.String(), alicePriv)
		if err != nil {
			t.Fatalf("alice connection %d: %v", i+1, err)
		}
		defer c.Close()
		if !served(c) {
			t.Fatalf("alice connection %d was not served", i+1)
		}
	}
	if over, err := sshDial(t, h, runID.String(), alicePriv); err == nil {
		ok := served(over)
		_ = over.Close()
		if ok {
			t.Fatal("alice's third connection was served, want it closed at her share")
		}
	}
	h.srv.metrics.mu.Lock()
	got, global := h.srv.metrics.sshRefused[sshBoundPrincipal], h.srv.metrics.sshRefused[sshBoundEstablished]
	h.srv.metrics.mu.Unlock()
	if got != 1 || global != 0 {
		t.Fatalf("refusals counted: principal %d, established %d, want 1 and 0", got, global)
	}
	bob, err := sshDial(t, h, bobRun.String(), bobPriv)
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	defer bob.Close()
	if !served(bob) {
		t.Fatal("bob was refused while alice sat at her share")
	}
}

// A connection that opens no channel was never re-checked, so deleting its key left it, and its
// slots, in place. The keepalive now re-checks the key: the idle connections close within a
// keepalive and the person's slots drain, so a new key of theirs is served again.
func TestSSHGateway_DeletedKeyClosesIdleConnectionsAndFreesSlots(t *testing.T) {
	prev := setSSHKeepaliveEvery(100 * time.Millisecond)
	t.Cleanup(func() { setSSHKeepaliveEvery(prev) })
	st := newSSHMemStore()
	runID := uuid.New()
	st.putRun(types.AgentRun{ID: runID, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1"})
	oldPriv, oldPub := mustSSHKeypair(t)
	oldFP := ssh.FingerprintSHA256(oldPub)
	st.putKey(types.SSHPublicKey{Fingerprint: oldFP, Principal: "alice@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(oldPub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{}, func(c *Config) { c.SSHMaxConnectionsPerPrincipal = 2 })

	var idle []*ssh.Client
	for i := range 2 {
		c, err := sshDial(t, h, runID.String(), oldPriv)
		if err != nil {
			t.Fatalf("connection %d: %v", i+1, err)
		}
		defer c.Close()
		idle = append(idle, c)
	}
	if err := st.DeleteSSHKey(context.Background(), oldFP, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	for i, c := range idle {
		closed := make(chan struct{})
		go func() { _ = c.Wait(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatalf("idle connection %d outlived its deleted key", i+1)
		}
	}

	newPriv, newPub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(newPub), Principal: "alice@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(newPub))})
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := sshDial(t, h, runID.String(), newPriv)
		if err == nil {
			_, _, serr := c.SendRequest("keepalive@openssh.com", true, nil)
			_ = c.Close()
			if serr == nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the closed connections never gave their slots back")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The share a person gets: the configured value; else, with no SSO, the global ceiling (every key is
// one person); else the default of 8.
func TestSSHGateway_PerPrincipalCapValues(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sso          bool
		perPrincipal int
		global       int
		want         int
	}{
		{"no SSO, nothing set", false, 0, 0, DefaultSSHMaxConnections},
		{"no SSO, global 20", false, 0, 20, 20},
		{"no SSO, variable 3", false, 3, 20, 3},
		{"SSO, nothing set", true, 0, 0, DefaultSSHMaxConnectionsPerPrincipal},
		{"SSO, global 20", true, 0, 20, DefaultSSHMaxConnectionsPerPrincipal},
		{"SSO, variable 12", true, 12, 0, 12},
	} {
		s := &Server{cfg: Config{SSHMaxConnections: tc.global, SSHMaxConnectionsPerPrincipal: tc.perPrincipal}}
		if tc.sso {
			s.cfg.OIDC = &oidc.Authenticator{}
		}
		if got := s.sshPerPrincipalCap(); got != tc.want {
			t.Errorf("%s: cap = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// With no SSO and nothing set, a ninth connection from the one principal is served (it was refused
// by the first version of the per-person cap).
func TestSSHGateway_NoSSONinthConnectionServed(t *testing.T) {
	st := newSSHMemStore()
	runID := uuid.New()
	st.putRun(types.AgentRun{ID: runID, CreatedBy: "admin-token", State: types.RunRunning, SandboxRef: "sbx-1"})
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: "admin-token", PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{})
	for i := range DefaultSSHMaxConnectionsPerPrincipal + 1 {
		c, err := sshDial(t, h, runID.String(), priv)
		if err != nil {
			t.Fatalf("connection %d: %v", i+1, err)
		}
		defer c.Close()
		if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
			t.Fatalf("connection %d was not served: %v", i+1, err)
		}
	}
}

// A connection refused over its person's share is told why on its first channel and an
// ssh.connection.reject row names the key.
func TestSSHGateway_OverPrincipalCapIsToldAndAudited(t *testing.T) {
	st := newSSHMemStore()
	runID := uuid.New()
	st.putRun(types.AgentRun{ID: runID, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1"})
	priv, pub := mustSSHKeypair(t)
	fp := ssh.FingerprintSHA256(pub)
	st.putKey(types.SSHPublicKey{Fingerprint: fp, Principal: "alice@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{}, func(c *Config) { c.SSHMaxConnectionsPerPrincipal = 1 })
	first, err := sshDial(t, h, runID.String(), priv)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, _, err := first.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatal(err)
	}
	over, err := sshDial(t, h, runID.String(), priv)
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	defer over.Close()
	_, err = over.NewSession()
	if err == nil || !strings.Contains(err.Error(), "too many SSH connections for this person (max 1)") {
		t.Fatalf("session on the refused connection: %v, want the cap named", err)
	}
	ev := waitForAudit(t, h.audit, runID, "ssh.connection.reject", "failure")
	if ev == nil || ev.Target != fp || ev.Actor != "alice@example.com" || !strings.Contains(string(ev.Data), sshCapReasonPrincipal) {
		t.Fatalf("ssh.connection.reject row = %+v", ev)
	}
}

// The refusal pool: one refusal-grace slot per person and sshRefusalPool in all, released on leave.
func TestSSHConnLimits_RefusalPoolBounds(t *testing.T) {
	l := &sshConnLimits{}
	if !l.enterRefusal("alice") || l.enterRefusal("alice") {
		t.Fatal("a person must hold exactly one refusal-grace slot")
	}
	for i := 1; i < sshRefusalPool; i++ {
		if !l.enterRefusal("p" + strings.Repeat("x", i)) {
			t.Fatalf("slot %d of %d refused", i+1, sshRefusalPool)
		}
	}
	if l.enterRefusal("one-more") {
		t.Fatalf("slot %d admitted, want the pool of %d full", sshRefusalPool+1, sshRefusalPool)
	}
	l.leaveRefusal("alice")
	if !l.enterRefusal("one-more") || l.enterRefusal("another") {
		t.Fatal("a released slot must be reusable exactly once")
	}
	if l.refusing != sshRefusalPool {
		t.Fatalf("refusing = %d, want %d", l.refusing, sshRefusalPool)
	}
}

// Alice's connections over her share never take a slot from Bob. Global ceiling 3, share 1: before,
// alice's two no-channel excess connections each held a global slot for the whole refusal grace, so
// bob (within his share) was refused; now they hold none.
func TestSSHGateway_ExcessConnectionsDoNotStarveAnotherPerson(t *testing.T) {
	st := newSSHMemStore()
	aliceRun, bobRun := uuid.New(), uuid.New()
	st.putRun(types.AgentRun{ID: aliceRun, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1"})
	st.putRun(types.AgentRun{ID: bobRun, CreatedBy: "bob@example.com", State: types.RunRunning, SandboxRef: "sbx-2"})
	alicePriv, alicePub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(alicePub), Principal: "alice@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(alicePub))})
	bobPriv, bobPub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(bobPub), Principal: "bob@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(bobPub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{}, func(c *Config) { c.SSHMaxConnections = 3; c.SSHMaxConnectionsPerPrincipal = 1 })

	first, err := sshDial(t, h, aliceRun.String(), alicePriv)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, _, err := first.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatal(err)
	}
	for i := range 2 { // no channel opened: they sit in the refusal wait or are closed at once
		c, err := sshDial(t, h, aliceRun.String(), alicePriv)
		if err != nil {
			t.Fatalf("excess %d: %v", i+1, err)
		}
		defer c.Close()
	}
	// Both excess connections have reached the refusal path once both rows are written.
	if !waitUntil(5*time.Second, func() bool { return rejectRows(h.audit) >= 2 }) {
		t.Fatalf("only %d ssh.connection.reject rows for alice's two excess connections", rejectRows(h.audit))
	}
	bob, err := sshDial(t, h, bobRun.String(), bobPriv)
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	defer bob.Close()
	if _, _, err := bob.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("bob was refused while alice's excess connections waited: %v", err)
	}
}

// The refusal wait gives its slot back on timeout and on the first channel's rejection: with one
// slot per person, every excess connection in turn must still be admitted to the wait and told why.
func TestSSHGateway_RefusalWaitIsReleased(t *testing.T) {
	t.Cleanup(func() { setSSHRefusalGrace(sshRefusalGrace) })
	st := newSSHMemStore()
	runID := uuid.New()
	st.putRun(types.AgentRun{ID: runID, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1"})
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: "alice@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{}, func(c *Config) { c.SSHMaxConnectionsPerPrincipal = 1 })
	held, err := sshDial(t, h, runID.String(), priv)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, _, err := held.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatal(err)
	}
	told := func(c *ssh.Client) bool {
		_, err := c.NewSession()
		return err != nil && strings.Contains(err.Error(), "too many SSH connections for this person")
	}
	closedWithin := func(c *ssh.Client, d time.Duration) bool {
		done := make(chan struct{})
		go func() { _ = c.Wait(); close(done) }()
		select {
		case <-done:
			return true
		case <-time.After(d):
			return false
		}
	}

	// First-channel rejection releases: three in a row are each told.
	for i := range 3 {
		c, err := sshDial(t, h, runID.String(), priv)
		if err != nil {
			t.Fatalf("excess %d: %v", i+1, err)
		}
		if !told(c) {
			t.Fatalf("excess %d was not told why: the wait slot was not released", i+1)
		}
		_ = c.Close()
		// The next excess connection must find the slot free: wait for the gateway's own count.
		if !waitUntil(5*time.Second, func() bool { return refusalWaiting(h) == 0 }) {
			t.Fatalf("after excess %d the refusal slot was never released", i+1)
		}
	}

	// Timeout releases: a connection that opens no channel is closed after the grace, and the person's
	// slot is free again for the next.
	setSSHRefusalGrace(200 * time.Millisecond)
	waiting, err := sshDial(t, h, runID.String(), priv)
	if err != nil {
		t.Fatal(err)
	}
	defer waiting.Close()
	if !closedWithin(waiting, 3*time.Second) {
		t.Fatal("a refused connection that opened no channel was not closed after the grace")
	}
	if !waitUntil(5*time.Second, func() bool { return refusalWaiting(h) == 0 }) {
		t.Fatal("the grace timed out but the refusal slot was never released")
	}
	setSSHRefusalGrace(sshRefusalGrace) // the next one is judged on being told, not on the clock
	after, err := sshDial(t, h, runID.String(), priv)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	if !told(after) {
		t.Fatal("after the grace timed out the person's wait slot was not free")
	}
}

// rejectRows counts the recorded ssh.connection.reject rows.
func rejectRows(a *sshTestRecorder) int {
	n := 0
	for _, ev := range a.snapshot() {
		if ev.Action == "ssh.connection.reject" {
			n++
		}
	}
	return n
}

// waitUntil polls cond until it holds or the deadline passes.
func waitUntil(d time.Duration, cond func() bool) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// refusalWaiting is the gateway's refusal-pool occupancy.
func refusalWaiting(h *sshTestHarness) int {
	l := h.srv.metrics.sshLimits.Load()
	l.principalsMu.Lock()
	defer l.principalsMu.Unlock()
	return l.refusing
}

// blockOnReject is a recorder that records everything except ssh.connection.reject, which it blocks
// on until the caller's context ends (as a stalled audit store would), counting how many are in
// flight at once.
type blockOnReject struct {
	audit.Recorder
	inflight, peak atomic.Int32
}

func (b *blockOnReject) Record(ctx context.Context, ev types.AuditEvent) error {
	if ev.Action != "ssh.connection.reject" {
		return b.Recorder.Record(ctx, ev)
	}
	n := b.inflight.Add(1)
	defer b.inflight.Add(-1)
	for {
		p := b.peak.Load()
		if n <= p || b.peak.CompareAndSwap(p, n) {
			break
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

// A stalled audit store must not let refused connections pile up: the reject row is written under
// the refusal grace, the work stays under the handshake bounds the connection still holds, and the
// deadline cleans every handler up.
func TestSSHGateway_StalledRejectAuditStaysBounded(t *testing.T) {
	prev := setSSHRefusalGrace(300 * time.Millisecond)
	t.Cleanup(func() { setSSHRefusalGrace(prev) })
	st := newSSHMemStore()
	runID := uuid.New()
	st.putRun(types.AgentRun{ID: runID, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1"})
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: "alice@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	blocker := &blockOnReject{}
	h := newSSHTestHarness(t, st, &sshFakeRunner{}, func(c *Config) {
		c.SSHMaxConnectionsPerPrincipal = 1
		blocker.Recorder = c.Audit
		c.Audit = blocker
	})
	held, err := sshDial(t, h, runID.String(), priv)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, _, err := held.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatal(err)
	}
	// Far more valid over-share handshakes than the per-source handshake bound allows at once.
	for range 3 * DefaultSSHMaxHandshakesPerSource {
		if c, err := sshDial(t, h, runID.String(), priv); err == nil {
			defer c.Close()
		}
	}
	if peak := int(blocker.peak.Load()); peak == 0 || peak > DefaultSSHMaxHandshakesPerSource {
		t.Fatalf("peak in-flight reject handlers = %d, want 1 to %d (the per-source handshake bound)", peak, DefaultSSHMaxHandshakesPerSource)
	}
	if !waitUntil(5*time.Second, func() bool { return blocker.inflight.Load() == 0 && refusalWaiting(h) == 0 }) {
		t.Fatalf("after the deadline %d reject handlers and %d refusal slots are still held", blocker.inflight.Load(), refusalWaiting(h))
	}
}
