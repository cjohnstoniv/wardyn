// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// In-process integration test for the SSH gateway (C2): a real x/crypto/ssh
// CLIENT dials a real net.Listener wired to this package's own
// ServeSSHGateway, against a fake Runner (fake ExecStream/Attach) and an
// in-memory Store. No daemons, no docker, no real sandbox — the gateway
// protocol logic (auth/authz, channel dispatch, the exec/sftp/forward
// bridges) is the thing under test, per the C5-later-lane gate.
package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── fakes ─────────────────────────────────────────────────────────────────

// sshMemStore is a minimal in-memory store.Store for this test: only the
// methods the gateway's auth/channel path touches are implemented; every
// other Store method panics via the embedded nil store.Store — the
// grantsStore/notFoundStore convention (sshkey_test.go, groundtruth_test.go).
type sshMemStore struct {
	store.Store
	mu   sync.Mutex
	runs map[uuid.UUID]types.AgentRun
	keys map[string]types.SSHPublicKey // by fingerprint
}

func newSSHMemStore() *sshMemStore {
	return &sshMemStore{runs: map[uuid.UUID]types.AgentRun{}, keys: map[string]types.SSHPublicKey{}}
}

func (s *sshMemStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return types.AgentRun{}, store.ErrNotFound
	}
	return r, nil
}

func (s *sshMemStore) TouchRun(context.Context, uuid.UUID) error { return nil }

func (s *sshMemStore) GetSSHKeyByFingerprint(_ context.Context, fp string) (types.SSHPublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[fp]
	if !ok {
		return types.SSHPublicKey{}, store.ErrNotFound
	}
	return k, nil
}

func (s *sshMemStore) putRun(r types.AgentRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[r.ID] = r
}

func (s *sshMemStore) putKey(k types.SSHPublicKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[k.Fingerprint] = k
}

// AddSSHKey/ListSSHKeysByPrincipal/DeleteSSHKey complete sshMemStore's
// store.Store surface for the REST-layer tests (sshkeys_test.go), mirroring
// store_sshkeys.go's PG semantics: fingerprint-PK conflict, principal-scoped
// list, owner-scoped delete (ErrNotFound on someone else's key — no
// existence leak).
func (s *sshMemStore) AddSSHKey(_ context.Context, k types.SSHPublicKey) (types.SSHPublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.keys[k.Fingerprint]; exists {
		return types.SSHPublicKey{}, store.ErrConflict
	}
	s.keys[k.Fingerprint] = k
	return k, nil
}

func (s *sshMemStore) ListSSHKeysByPrincipal(_ context.Context, principal string) ([]types.SSHPublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []types.SSHPublicKey{}
	for _, k := range s.keys {
		if k.Principal == principal {
			out = append(out, k)
		}
	}
	return out, nil
}

func (s *sshMemStore) DeleteSSHKey(_ context.Context, fingerprint, principal string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[fingerprint]
	if !ok || k.Principal != principal {
		return store.ErrNotFound
	}
	delete(s.keys, fingerprint)
	return nil
}

// sshFakeRunner is Attach/ExecStream-capable (the brief's explicit "fake
// ExecStream/Attach" ask); the rest of runner.Runner is unused by this test
// and errors loudly if reached. execFn/attachFn are set per (sub)test to
// exactly the behavior that test needs — a pluggable factory rather than a
// pile of mode fields.
type sshFakeRunner struct {
	mu       sync.Mutex
	attachFn func() (runner.Session, error)
	execFn   func(spec runner.ExecSpec) (*runner.ExecSession, error)
	lastArgv []string
	lastEnv  []string
}

func (f *sshFakeRunner) Name() string { return "ssh-fake" }
func (f *sshFakeRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return runner.Capabilities{Driver: "ssh-fake"}, nil
}
func (f *sshFakeRunner) CreateSandbox(context.Context, runner.SandboxSpec) (runner.Sandbox, error) {
	return runner.Sandbox{}, errors.New("not used by this test")
}
func (f *sshFakeRunner) Exec(context.Context, string, []string) (string, error) {
	return "", errors.New("not used by this test")
}
func (f *sshFakeRunner) Wait(context.Context, string) (int, error) {
	return 0, errors.New("not used by this test")
}
func (f *sshFakeRunner) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{State: types.RunRunning}, nil
}
func (f *sshFakeRunner) AgentStatus(context.Context, string, string) (runner.Status, error) {
	return runner.Status{State: types.RunRunning}, nil
}
func (f *sshFakeRunner) StopSandbox(context.Context, string) error { return nil }
func (f *sshFakeRunner) KillSandbox(context.Context, string) error { return nil }

func (f *sshFakeRunner) Attach(context.Context, string, runner.AttachOptions) (runner.Session, error) {
	f.mu.Lock()
	fn := f.attachFn
	f.mu.Unlock()
	if fn == nil {
		return newFakeShellSession(), nil
	}
	return fn()
}

func (f *sshFakeRunner) ExecStream(_ context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	f.mu.Lock()
	f.lastArgv = spec.Argv
	f.lastEnv = spec.Env
	fn := f.execFn
	f.mu.Unlock()
	if fn == nil {
		return nil, runner.ErrExecStreamUnsupported
	}
	return fn(spec)
}

func (f *sshFakeRunner) lastCall() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastArgv, f.lastEnv
}

var _ runner.Runner = (*sshFakeRunner)(nil)

// fakeExecSession backs ExecStream with UNBUFFERED io.Pipes, mirroring the
// real docker driver's demux (runner/docker/session.go) — so a gateway
// bridge that forgot to drain Stderr BEFORE reading Stdout would genuinely
// deadlock a caller of this fake, exactly the HARD CONTRACT the brief calls
// out (this is what TestSSHGateway_StderrDrainedBeforeStdout pins). stderrMsg
// is written BEFORE any Stdout byte, matching sftp-server's own error-path
// ordering.
func fakeExecSession(stdoutMsg, stderrMsg string, exitCode int) *runner.ExecSession {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	go func() {
		if stderrMsg != "" {
			_, _ = io.WriteString(stderrW, stderrMsg)
		}
		if stdoutMsg != "" {
			_, _ = io.WriteString(stdoutW, stdoutMsg)
		}
		_ = stdoutW.Close()
		_ = stderrW.Close()
	}()
	return &runner.ExecSession{
		Stdout: stdoutR,
		Stderr: stderrR,
		Wait:   func() (int, error) { return exitCode, nil },
	}
}

// fakeEchoExecSession copies Stdin verbatim to Stdout — the "subsystem
// plumbing" / direct-tcpip tests' proof that bytes the client sends reach
// ExecStream's Stdin and whatever ExecStream writes to Stdout reaches the
// client, with neither side protocol-aware (no real sftp-server/socat).
func fakeEchoExecSession() *runner.ExecSession {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	go func() {
		_, _ = io.Copy(stdoutW, stdinR)
		_ = stdoutW.Close()
		_ = stderrW.Close()
	}()
	return &runner.ExecSession{
		Stdin:  stdinW,
		Stdout: stdoutR,
		Stderr: stderrR,
		Wait:   func() (int, error) { return 0, nil },
	}
}

// fakeBlockingExecSession backs a direct-tcpip forward whose Stdout produces
// nothing until the caller closes the returned writer — giving a test full
// control over exactly when sshBridgeExecSession's blocking
// io.Copy(channel, sess.Stdout) unblocks and the bridge proceeds to its
// trailing recordAudit call. TestSSHGateway_ForwardAuditSurvivesKill uses
// this to construct the connection-teardown race deterministically instead
// of hoping to win a timing race.
func fakeBlockingExecSession() (sess *runner.ExecSession, stdoutW *io.PipeWriter) {
	stdoutR, w := io.Pipe()
	return &runner.ExecSession{
		Stdout: stdoutR,
		Wait:   func() (int, error) { return 0, nil },
	}, w
}

// fakeShellSession is a trivial runner.Session for the "shell" (Attach)
// path: it echoes whatever is written back on Read, so the PTY pump can be
// exercised with no real sandbox.
type fakeShellSession struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func newFakeShellSession() *fakeShellSession {
	r, w := io.Pipe()
	return &fakeShellSession{r: r, w: w}
}

func (s *fakeShellSession) Read(p []byte) (int, error)                   { return s.r.Read(p) }
func (s *fakeShellSession) Write(p []byte) (int, error)                  { return s.w.Write(p) }
func (s *fakeShellSession) Resize(context.Context, uint16, uint16) error { return nil }
func (s *fakeShellSession) Close() error                                 { _ = s.w.Close(); return s.r.Close() }

var _ runner.Session = (*fakeShellSession)(nil)

// ─── harness ───────────────────────────────────────────────────────────────

// sshTestHarness starts a real SSH gateway (ServeSSHGateway) on a loopback
// port backed by st/fr, and returns everything a test needs to dial it.
type sshTestHarness struct {
	addr    string
	hostPub ssh.PublicKey
	audit   *sshTestRecorder
}

// sshTestRecorder is a thread-safe audit.Recorder. Unlike the rest of this
// package's synchronous-call-chain tests (which share the plain, unlocked
// recRecorder safely — a handler records, then returns, then the test reads),
// the gateway's own goroutines (accept loop, channel bridges) record
// concurrently with a test that is actively polling for the result
// (waitForAudit), so a bare slice would race under -race.
type sshTestRecorder struct {
	mu     sync.Mutex
	events []types.AuditEvent
}

// Record mirrors the real store.Recorder/pgx behaviour this package's
// production code depends on: a cancelled ctx fails the write instead of
// silently accepting it (see TestSSHGateway_ForwardAuditSurvivesKill) — a
// call site that races connection teardown against its own audit write is
// caught here, in-process, instead of only on a real Postgres-backed run.
func (r *sshTestRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *sshTestRecorder) snapshot() []types.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]types.AuditEvent(nil), r.events...)
}

func newSSHTestHarness(t *testing.T, st *sshMemStore, fr *sshFakeRunner) *sshTestHarness {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}

	// Ask the OS for a free port, release it, then bind the SAME address —
	// the standard Go test idiom for "know the port before the real listener
	// exists"; a collision in that window is not realistic for a
	// single-process test run.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe free port: %v", err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	audit := &sshTestRecorder{}
	srv := New(Config{
		Store:         st,
		Identity:      mustIDP(t),
		Approvals:     newFakeApprovals(),
		Broker:        &fakeBroker{},
		Audit:         audit,
		Runner:        fr,
		AdminToken:    adminToken,
		TrustDomain:   "wardyn.local",
		DefaultPolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},

		SSHListenAddr:    addr,
		SSHAdvertiseAddr: addr,
		SSHHostKey:       hostPriv,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if err := srv.ServeSSHGateway(ctx); err != nil {
			t.Logf("ssh gateway stopped: %v", err)
		}
	}()
	waitForListener(t, addr)

	return &sshTestHarness{addr: addr, hostPub: signer.PublicKey(), audit: audit}
}

// waitForListener polls addr until something accepts a TCP connection —
// ServeSSHGateway's net.Listen happens in the goroutine above with no
// separate readiness signal, so a short poll stands in for one.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ssh gateway never started listening on %s", addr)
}

// waitForAudit polls audit for a matching event for up to a second. A
// completed client-side call (Session.Run returning, a forwarded conn
// closing) only proves the CHANNEL closed — the server goroutine's own
// recordAudit call is one more line after that same close, on the SAME
// goroutine but observed via a separate network event on the client side, so
// asserting immediately is a genuine (if narrow) race, not a test bug to
// paper over silently.
func waitForAudit(t *testing.T, audit *sshTestRecorder, runID uuid.UUID, action, outcome string) *types.AuditEvent {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if ev := findAudit(audit.snapshot(), runID, action, outcome); ev != nil {
			return ev
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// mustSSHKeypair generates an ed25519 keypair and wraps the public half as
// ssh.PublicKey (what gets registered / offered for auth).
func mustSSHKeypair(t *testing.T) (ed25519.PrivateKey, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrap public key: %v", err)
	}
	return priv, sshPub
}

// offerOnlySigner implements ssh.Signer with a genuine public key but a Sign
// that always fails — modeling an attacker who KNOWS a victim's public key
// (public keys are not secret) but does not hold the matching private key.
// Dialing with this signer still sends the real SSH_MSG_USERAUTH_REQUEST
// "query" (RFC 4252 §7, HasSig=false) for the offered key — the same message
// that reaches PublicKeyCallback/sshAuth for ANY pubkey attempt, signed or
// not — but can never produce the signed follow-up VerifiedPublicKeyCallback
// gates success on (golang.org/x/crypto/ssh's client wraps a plain Signer's
// Sign via algorithmSignerWrapper regardless of signature-algorithm
// negotiation, so this Sign is what ultimately gets called).
type offerOnlySigner struct {
	pub ssh.PublicKey
}

func (s offerOnlySigner) PublicKey() ssh.PublicKey { return s.pub }
func (s offerOnlySigner) Sign(io.Reader, []byte) (*ssh.Signature, error) {
	return nil, errors.New("offer-only: no private key (simulated attacker)")
}

// sshDial authenticates as username with clientPriv against h, pinning the
// gateway's own host key (verify-on-first-connect, exactly what a real
// client does against the fingerprint the run-detail pane shows).
func sshDial(t *testing.T, h *sshTestHarness, username string, clientPriv ed25519.PrivateKey) (*ssh.Client, error) {
	t.Helper()
	signer, err := ssh.NewSignerFromKey(clientPriv)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}
	return ssh.Dial("tcp", h.addr, &ssh.ClientConfig{
		User:            username,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(h.hostPub),
		Timeout:         3 * time.Second,
	})
}

// ─── tests ─────────────────────────────────────────────────────────────────

// TestSSHGateway_AuthRejectAccept covers auth reject/accept: an unregistered
// key is rejected, a registered key authenticating for a run it does NOT own
// is rejected (owner-only), and the owner's registered key is accepted —
// each rejection/acceptance is audited under ssh.auth.
func TestSSHGateway_AuthRejectAccept(t *testing.T) {
	st := newSSHMemStore()
	ownRun := uuid.New()
	otherRun := uuid.New()
	st.putRun(types.AgentRun{ID: ownRun, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1"})
	st.putRun(types.AgentRun{ID: otherRun, CreatedBy: "bob@example.com", State: types.RunRunning, SandboxRef: "sbx-2"})

	alicePriv, alicePub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{
		Fingerprint: ssh.FingerprintSHA256(alicePub),
		Principal:   "alice@example.com",
		PublicKey:   string(ssh.MarshalAuthorizedKey(alicePub)),
	})

	h := newSSHTestHarness(t, st, &sshFakeRunner{})

	t.Run("unregistered key rejected", func(t *testing.T) {
		strangerPriv, _ := mustSSHKeypair(t)
		_, err := sshDial(t, h, ownRun.String(), strangerPriv)
		if err == nil {
			t.Fatal("dial with an unregistered key succeeded, want rejected")
		}
	})

	t.Run("registered key for a run it does not own is rejected", func(t *testing.T) {
		_, err := sshDial(t, h, otherRun.String(), alicePriv)
		if err == nil {
			t.Fatal("dial for a non-owned run succeeded, want owner-only rejection")
		}
		// The key itself is genuine (alice's), just not authorized for
		// bob's run — the audit trail must name alice, not "unknown", and
		// must carry the caller's source IP.
		ev := waitForAudit(t, h.audit, otherRun, "ssh.auth", "failure")
		if ev == nil {
			t.Fatalf("no failed ssh.auth event for the owner-mismatch case; events=%s", auditDump(h.audit.snapshot(), otherRun))
		}
		if ev.Actor != "alice@example.com" {
			t.Errorf("ssh.auth failure actor = %q, want alice@example.com (a real, known principal — not \"unknown\")", ev.Actor)
		}
		if ev.SourceIP == "" {
			t.Error("ssh.auth failure has no SourceIP recorded")
		}
	})

	t.Run("malformed username (not a run id) is rejected", func(t *testing.T) {
		_, err := sshDial(t, h, "not-a-uuid", alicePriv)
		if err == nil {
			t.Fatal("dial with a non-UUID username succeeded, want rejected")
		}
	})

	t.Run("owner's registered key is accepted", func(t *testing.T) {
		client, err := sshDial(t, h, ownRun.String(), alicePriv)
		if err != nil {
			t.Fatalf("owner dial failed: %v", err)
		}
		defer client.Close()

		ev := waitForAudit(t, h.audit, ownRun, "ssh.auth", "success")
		if ev == nil {
			t.Fatalf("no successful ssh.auth event; events=%s", auditDump(h.audit.snapshot(), ownRun))
		}
		if ev.SourceIP == "" {
			t.Error("ssh.auth success has no SourceIP recorded")
		}
	})

	// Every case above left a trail: at least one failure and one success.
	var failures, successes int
	for _, ev := range h.audit.snapshot() {
		if ev.Action != "ssh.auth" {
			continue
		}
		if ev.Outcome == "success" {
			successes++
		} else {
			failures++
		}
	}
	if successes == 0 || failures == 0 {
		t.Errorf("ssh.auth audit trail = %d success, %d failure; want at least one of each", successes, failures)
	}
}

// TestSSHGateway_AdminKeyOverride covers F1's admin override (migration 0043):
// a key whose STORED role is admin reaches a run it does not own, and the
// ssh.auth success that results carries override:true; a key stored with the
// member role is still refused for the same run; and an ordinary owner login
// carries NO override datum (its absence is what makes the datum greppable).
func TestSSHGateway_AdminKeyOverride(t *testing.T) {
	st := newSSHMemStore()
	bobRun := uuid.New()
	adminRun := uuid.New()
	st.putRun(types.AgentRun{ID: bobRun, CreatedBy: "bob@example.com", State: types.RunRunning, SandboxRef: "sbx-bob"})
	st.putRun(types.AgentRun{ID: adminRun, CreatedBy: "root@example.com", State: types.RunRunning, SandboxRef: "sbx-root"})

	now := time.Now()
	adminPriv, adminPub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{
		Fingerprint:   ssh.FingerprintSHA256(adminPub),
		Principal:     "root@example.com",
		PublicKey:     string(ssh.MarshalAuthorizedKey(adminPub)),
		Role:          oidc.RoleAdmin,
		RoleCheckedAt: &now, // fresh — this test is about role/ownership, not staleness (see TestSSHGateway_OverrideRoleIsBoundedStale)
	})
	memberPriv, memberPub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{
		Fingerprint:   ssh.FingerprintSHA256(memberPub),
		Principal:     "mallory@example.com",
		PublicKey:     string(ssh.MarshalAuthorizedKey(memberPub)),
		Role:          oidc.RoleMember,
		RoleCheckedAt: &now,
	})

	h := newSSHTestHarness(t, st, &sshFakeRunner{})

	t.Run("admin key reaches a run it does not own, audited as an override", func(t *testing.T) {
		client, err := sshDial(t, h, bobRun.String(), adminPriv)
		if err != nil {
			t.Fatalf("admin dial for another human's run failed: %v", err)
		}
		defer client.Close()

		ev := waitForAudit(t, h.audit, bobRun, "ssh.auth", "success")
		if ev == nil {
			t.Fatalf("no successful ssh.auth event for the admin override; events=%s", auditDump(h.audit.snapshot(), bobRun))
		}
		if ev.Actor != "root@example.com" {
			t.Errorf("ssh.auth success actor = %q, want root@example.com (the admin, not the run's owner)", ev.Actor)
		}
		var data struct {
			Override bool `json:"override"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("decode ssh.auth data %q: %v", string(ev.Data), err)
		}
		if !data.Override {
			t.Errorf("ssh.auth success data = %q, want override:true", string(ev.Data))
		}
	})

	t.Run("member key is still refused for a run it does not own", func(t *testing.T) {
		if _, err := sshDial(t, h, bobRun.String(), memberPriv); err == nil {
			t.Fatal("dial with a member-role key for another human's run succeeded, want refused")
		}
		ev := waitForAudit(t, h.audit, bobRun, "ssh.auth", "failure")
		if ev == nil {
			t.Fatalf("no failed ssh.auth event for the member key; events=%s", auditDump(h.audit.snapshot(), bobRun))
		}
		if ev.Actor != "mallory@example.com" {
			t.Errorf("ssh.auth failure actor = %q, want mallory@example.com", ev.Actor)
		}
	})

	t.Run("an admin reaching their OWN run is not an override", func(t *testing.T) {
		client, err := sshDial(t, h, adminRun.String(), adminPriv)
		if err != nil {
			t.Fatalf("admin dial for own run failed: %v", err)
		}
		defer client.Close()

		ev := waitForAudit(t, h.audit, adminRun, "ssh.auth", "success")
		if ev == nil {
			t.Fatalf("no successful ssh.auth event; events=%s", auditDump(h.audit.snapshot(), adminRun))
		}
		if len(ev.Data) != 0 {
			t.Errorf("owner ssh.auth success data = %q, want no datum at all (override marks the exception, not the rule)", string(ev.Data))
		}
	})
}

// TestSSHGateway_OverrideRoleIsBoundedStale pins the migration-0046 ceiling:
// the override reads the key's STORED role, stamped at registration and
// refreshed at every OIDC login, but sshAuth never re-derives it live at
// connect time — EXCEPT that it now also refuses the override once
// role_checked_at is older than WARDYN_SSH_ROLE_TTL (default 24h in the test
// harness, since none of these keys override it), or was never stamped at
// all (nil — a pre-0046 row). A principal whose live role has since changed —
// in EITHER direction — is governed by the stamp (bounded by the TTL) until
// the key is deleted, re-registered, or its owner logs in again.
func TestSSHGateway_OverrideRoleIsBoundedStale(t *testing.T) {
	st := newSSHMemStore()
	run := uuid.New()
	st.putRun(types.AgentRun{ID: run, CreatedBy: "bob@example.com", State: types.RunRunning, SandboxRef: "sbx-bob"})

	fresh := time.Now()
	stale := time.Now().Add(-48 * time.Hour) // past the 24h default TTL

	// A key stamped admin RECENTLY (registration, or a login within the TTL)
	// whose principal is a DEMOTED admin: nothing about them is admin any
	// more, and the gateway has no live check able to notice — the stamp
	// still opens bob's run, because it is still within the TTL window.
	demotedFreshPriv, demotedFreshPub := mustSSHKeypair(t)
	demotedFreshFP := ssh.FingerprintSHA256(demotedFreshPub)
	st.putKey(types.SSHPublicKey{
		Fingerprint:   demotedFreshFP,
		Principal:     "exadmin@example.com",
		PublicKey:     string(ssh.MarshalAuthorizedKey(demotedFreshPub)),
		Role:          oidc.RoleAdmin,
		RoleCheckedAt: &fresh,
	})
	// The SAME shape, but the stamp is older than WARDYN_SSH_ROLE_TTL: the TTL
	// bites even though role still reads "admin" — this is the bound migration
	// 0046 adds, catching a demotion the demoted human never logs in again to
	// self-correct.
	demotedStalePriv, demotedStalePub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{
		Fingerprint:   ssh.FingerprintSHA256(demotedStalePub),
		Principal:     "exadmin2@example.com",
		PublicKey:     string(ssh.MarshalAuthorizedKey(demotedStalePub)),
		Role:          oidc.RoleAdmin,
		RoleCheckedAt: &stale,
	})
	// role=admin but role_checked_at is nil — a pre-0046 row that has never
	// been through a login refresh. Treated as infinitely stale, same as the
	// TTL-exceeded case, not as an exemption from the check.
	neverCheckedPriv, neverCheckedPub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{
		Fingerprint: ssh.FingerprintSHA256(neverCheckedPub),
		Principal:   "exadmin3@example.com",
		PublicKey:   string(ssh.MarshalAuthorizedKey(neverCheckedPub)),
		Role:        oidc.RoleAdmin,
		// RoleCheckedAt intentionally left nil.
	})
	// A key stamped member at registration whose principal has since been
	// PROMOTED. The stamp is stale in the other direction, so no override
	// applies regardless of role_checked_at — role itself must read admin.
	promotedPriv, promotedPub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{
		Fingerprint:   ssh.FingerprintSHA256(promotedPub),
		Principal:     "newadmin@example.com",
		PublicKey:     string(ssh.MarshalAuthorizedKey(promotedPub)),
		Role:          oidc.RoleMember,
		RoleCheckedAt: &fresh,
	})

	h := newSSHTestHarness(t, st, &sshFakeRunner{})

	client, err := sshDial(t, h, run.String(), demotedFreshPriv)
	if err != nil {
		t.Fatalf("a key stamped admin within the TTL must keep its override, got: %v", err)
	}
	client.Close()

	if _, err := sshDial(t, h, run.String(), demotedStalePriv); err == nil {
		t.Fatal("a key stamped admin but role_checked_at past WARDYN_SSH_ROLE_TTL reached another human's run; the TTL must refuse it")
	}

	if _, err := sshDial(t, h, run.String(), neverCheckedPriv); err == nil {
		t.Fatal("a key stamped admin with a nil role_checked_at (pre-0046 row) reached another human's run; nil must read as infinitely stale")
	}

	if _, err := sshDial(t, h, run.String(), promotedPriv); err == nil {
		t.Fatal("a key stamped member reached another human's run after its principal was promoted; the stamp, not the live role, must govern")
	}

	// The remediation the docs promise: delete the fresh-but-demoted key and
	// the override goes with it (re-registering, or logging in again, would
	// re-stamp from the then-current role).
	if err := st.DeleteSSHKey(context.Background(), demotedFreshFP, "exadmin@example.com"); err != nil {
		t.Fatalf("delete demoted admin key: %v", err)
	}
	if _, err := sshDial(t, h, run.String(), demotedFreshPriv); err == nil {
		t.Fatal("a deleted key still authenticated; deletion is the documented remediation for a stale stamp")
	}
}

// TestSSHGateway_OfferWithoutSignatureNeverAudited pins W25.4-1: PublicKeyCallback
// (sshAuth) fires on the UNSIGNED "query" every pubkey auth attempt opens
// with (RFC 4252 §7) — before the client ever proves it holds the matching
// private key. Auditing "ssh.auth success" there (the pre-fix behavior) let
// an attacker who merely KNOWS a victim's public key — never the private key
// — mint a forged success row attributed to that victim. The offered key
// here is genuinely registered and owned (sshAuth's own checks all pass, so
// the query itself is accepted server-side) — but offerOnlySigner can never
// sign, so the connection ultimately fails and VerifiedPublicKeyCallback
// (where success is audited post-fix, and only after a real Verify) never
// runs.
func TestSSHGateway_OfferWithoutSignatureNeverAudited(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	_, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{})

	_, err := ssh.Dial("tcp", h.addr, &ssh.ClientConfig{
		User:            run.ID.String(),
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(offerOnlySigner{pub: pub})},
		HostKeyCallback: ssh.FixedHostKey(h.hostPub),
		Timeout:         3 * time.Second,
	})
	if err == nil {
		t.Fatal("dial with an unsigned (offer-only) key succeeded, want a signing failure")
	}

	// We want the ABSENCE of an event, so a bounded sleep-out is the right
	// shape here (giving the server-side query handling — and, pre-fix, its
	// forged success write — a moment to run), not waitForAudit's
	// poll-until-found, which would just time out either way.
	time.Sleep(200 * time.Millisecond)
	if ev := findAudit(h.audit.snapshot(), run.ID, "ssh.auth", "success"); ev != nil {
		t.Fatalf("ssh.auth success recorded for an offer that was never signed: %+v; events=%s", ev, auditDump(h.audit.snapshot(), run.ID))
	}
}

// TestSSHGateway_ExecExitCode covers the exec channel: the command reaches
// ExecStream wrapped as `/bin/sh -c <command>` (no protocol
// reimplementation), the real exit code round-trips to the client, and the
// env allowlist forwards TERM but drops an arbitrary variable.
func TestSSHGateway_ExecExitCode(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})

	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
		return fakeExecSession("", "", 7), nil
	}}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	_ = sess.Setenv("TERM", "xterm-256color")
	_ = sess.Setenv("SECRET_LEAK", "should-not-forward")

	err = sess.Run("do the thing")
	var exitErr *ssh.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Run error = %v (%T), want *ssh.ExitError", err, err)
	}
	if exitErr.ExitStatus() != 7 {
		t.Errorf("exit status = %d, want 7", exitErr.ExitStatus())
	}

	argv, env := fr.lastCall()
	if len(argv) != 3 || argv[0] != "/bin/sh" || argv[1] != "-c" || argv[2] != "do the thing" {
		t.Errorf("argv = %v, want [/bin/sh -c \"do the thing\"] (no protocol reimplementation)", argv)
	}
	if !containsEnv(env, "TERM=xterm-256color") {
		t.Errorf("env = %v, want TERM forwarded", env)
	}
	if containsPrefix(env, "SECRET_LEAK=") {
		t.Errorf("env = %v, want SECRET_LEAK dropped (allowlist is TERM/LANG/LC_* only)", env)
	}

	// The audit trail names the argv and the exit code (ssh.exec{argv,exit}).
	// Session.Run() unblocking (above) races the server goroutine's own
	// recordAudit call slightly (both follow the same channel-close network
	// event, on different sides) — poll briefly rather than asserting
	// immediately.
	if ev := waitForAudit(t, h.audit, run.ID, "ssh.exec", "success"); ev == nil {
		t.Errorf("no successful ssh.exec audit event; events=%s", auditDump(h.audit.snapshot(), run.ID))
	}
}

// TestSSHGateway_ExecExitZero guards the success path (exit 0 -> nil error),
// so the exit-status assertion above isn't vacuously true for "any nonzero".
func TestSSHGateway_ExecExitZero(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeExecSession("ok\n", "", 0), nil }}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	out, err := sess.Output("echo ok")
	if err != nil {
		t.Fatalf("Output() = %v, want nil (exit 0)", err)
	}
	if string(out) != "ok\n" {
		t.Errorf("stdout = %q, want %q", out, "ok\n")
	}
}

// TestSSHGateway_StderrDrainedBeforeStdout pins the HARD CONTRACT from the
// A0+C1 review: ExecSession's Stdout/Stderr are unbuffered io.Pipes, so a
// consumer that reads Stdout before draining Stderr deadlocks the instant the
// far side writes a stderr byte before any stdout — sftp-server's own
// error-path ordering. fakeExecSession writes stderr FIRST; if the gateway's
// drain didn't start before its Stdout copy, this test would hang (and fail
// on the suite's timeout) instead of completing.
func TestSSHGateway_StderrDrainedBeforeStdout(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
		return fakeExecSession("stdout-after-stderr\n", "stderr-first\n", 0), nil
	}}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	out, err := sess.CombinedOutput("cmd")
	if err != nil {
		t.Fatalf("CombinedOutput() = %v, want nil", err)
	}
	if len(out) == 0 {
		t.Fatal("no output observed — the stderr-first write may have wedged the stream")
	}
}

// TestSSHGateway_ExecStreamUnsupportedIsCleanError pins the post-brief
// context: runner.ErrExecStreamUnsupported maps to a clean channel error
// (nonzero exit + a message), never a hang.
func TestSSHGateway_ExecStreamUnsupportedIsCleanError(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) { return nil, runner.ErrExecStreamUnsupported }}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	out, runErr := sess.CombinedOutput("cmd")
	if runErr == nil {
		t.Fatal("exec on an ExecStream-unsupported substrate succeeded, want a clean nonzero error")
	}
	if !strings.Contains(string(out), "does not support") {
		t.Errorf("output = %q, want it to name the unsupported-substrate reason", out)
	}
}

// TestSSHGateway_SubsystemPlumbing covers subsystem plumbing: the sftp
// subsystem's backing exec is bidirectionally bridged (bytes written by the
// client reach ExecStream's Stdin and come back via Stdout), and a
// subsystem name other than "sftp" is refused.
func TestSSHGateway_SubsystemPlumbing(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeEchoExecSession(), nil }}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	t.Run("sftp subsystem bridges stdin<->stdout", func(t *testing.T) {
		sess, err := client.NewSession()
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		defer sess.Close()
		in, err := sess.StdinPipe()
		if err != nil {
			t.Fatalf("stdin pipe: %v", err)
		}
		out, err := sess.StdoutPipe()
		if err != nil {
			t.Fatalf("stdout pipe: %v", err)
		}
		if err := sess.RequestSubsystem("sftp"); err != nil {
			t.Fatalf("request sftp subsystem: %v", err)
		}
		payload := []byte("sftp-init-packet")
		if _, err := in.Write(payload); err != nil {
			t.Fatalf("write stdin: %v", err)
		}
		buf := make([]byte, len(payload))
		if _, err := io.ReadFull(out, buf); err != nil {
			t.Fatalf("read stdout: %v", err)
		}
		if string(buf) != string(payload) {
			t.Errorf("echoed = %q, want %q (subsystem's exec is not bridged)", buf, payload)
		}
		argv, _ := fr.lastCall()
		if len(argv) != 2 || argv[0] != sftpServerPath || argv[1] != "-e" {
			t.Errorf("argv = %v, want [%s -e]", argv, sftpServerPath)
		}
	})

	t.Run("a non-sftp subsystem is refused", func(t *testing.T) {
		sess, err := client.NewSession()
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		defer sess.Close()
		if err := sess.RequestSubsystem("not-sftp"); err == nil {
			t.Error("non-sftp subsystem was accepted, want refused")
		}
	})
}

// TestSSHGateway_DirectTCPIPLoopbackRestriction covers `-L` forwarding: a
// non-loopback destination is refused before any ExecStream call, and a
// loopback destination is bridged through `socat` (fakeEchoExecSession
// proves the bidirectional plumbing, same as the sftp case).
func TestSSHGateway_DirectTCPIPLoopbackRestriction(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeEchoExecSession(), nil }}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	t.Run("non-loopback destination refused", func(t *testing.T) {
		if _, err := client.Dial("tcp", "10.0.0.5:8080"); err == nil {
			t.Error("forward to a non-loopback destination succeeded, want refused")
		}
	})

	t.Run("loopback destination bridges via socat", func(t *testing.T) {
		conn, err := client.Dial("tcp", "127.0.0.1:9999")
		if err != nil {
			t.Fatalf("forward to sandbox loopback: %v", err)
		}
		payload := []byte("forwarded-bytes")
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		buf := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(buf) != string(payload) {
			t.Errorf("echoed = %q, want %q", buf, payload)
		}
		argv, _ := fr.lastCall()
		if len(argv) != 3 || argv[0] != "socat" || argv[2] != "TCP:127.0.0.1:9999" {
			t.Errorf("argv = %v, want [socat - TCP:127.0.0.1:9999]", argv)
		}
		// Close explicitly (not deferred): the server-side bridge only
		// finishes — and records ssh.forward — once it observes EOF, which
		// for direct-tcpip happens on OUR close, not the fake's.
		_ = conn.Close()
	})

	// The server-side audit write races this goroutine's return from Dial's
	// Close above; poll briefly rather than asserting immediately.
	if ev := waitForAudit(t, h.audit, run.ID, "ssh.forward", "success"); ev == nil {
		t.Errorf("no successful ssh.forward audit event; events=%s", auditDump(h.audit.snapshot(), run.ID))
	}
}

// TestSSHGateway_ForwardAuditSurvivesKill pins the audit-race the live C5
// e2e caught (scripts/run-e2e-ssh.sh's -L forward step, see its step-5
// comment): killing the WHOLE SSH session (not just the forwarded conn)
// races handleSSHConn's connCtx cancellation — deferred, fires the instant
// the connection tears down — against handleSSHDirectTCPIP's own trailing
// recordAudit call for ssh.forward. That call used to run on connCtx; if the
// cancellation lands first, the write is attempted on an already-cancelled
// context and is silently dropped (sshTestRecorder.Record mirrors the real
// store/pgx behaviour here: it errors on a cancelled ctx instead of writing).
// The fix runs that trailing write on s.cfg.BaseCtx instead — see
// bridgeSSHExec's FINDING comment in sshgateway_channels.go.
//
// Deterministic, not timing-dependent: the fake ExecSession's Stdout blocks
// until this test itself closes it, so "the connection is already dead" is
// guaranteed true (a bounded wait lets handleSSHConn's teardown actually
// finish) BEFORE the bridge's tail — and its recordAudit call — is ever
// allowed to run. Revert the s.cfg.BaseCtx fix and this test fails (the
// event never appears within waitForAudit's deadline); with the fix, BaseCtx
// is never cancelled by the kill, so the row lands regardless.
func TestSSHGateway_ForwardAuditSurvivesKill(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})

	sess, stdoutW := fakeBlockingExecSession()
	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) { return sess, nil }}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	conn, err := client.Dial("tcp", "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("forward to sandbox loopback: %v", err)
	}
	defer conn.Close()

	// Kill the WHOLE session — the exact shape scripts/run-e2e-ssh.sh's step
	// 5 describes (`kill $FWD_PID`), not merely closing this one forward.
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	// Let handleSSHConn's own teardown (its channel-accept loop exiting once
	// the connection dies, then its deferred connCtx cancel) actually finish
	// BEFORE the bridge below is allowed to proceed — this is what makes the
	// race deterministic instead of a coin flip.
	time.Sleep(200 * time.Millisecond)

	// NOW let the bridge's blocked io.Copy(channel, sess.Stdout) see EOF and
	// run its tail (Wait + the trailing recordAudit), with connCtx already
	// cancelled per the wait above.
	_ = stdoutW.Close()

	if ev := waitForAudit(t, h.audit, run.ID, "ssh.forward", "success"); ev == nil {
		t.Errorf("no ssh.forward audit event survived connection teardown; events=%s", auditDump(h.audit.snapshot(), run.ID))
	}
}

// TestSSHGateway_RemoteForwardRejected covers "-R": the gateway serves no
// global requests, so a "tcpip-forward" request (remote/reverse port
// forwarding) is refused rather than silently ignored or hung.
func TestSSHGateway_RemoteForwardRejected(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{})
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if _, err := client.Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Error("remote forward (-R / tcpip-forward) succeeded, want refused")
	}
}

// TestSSHGateway_ShellChannelDrivesFakeSession drives an actual "shell"
// channel through fakeShellSession end to end: the client's bytes reach
// Runner.Attach's returned Session and echo back, and the bridge goes
// through the SAME recorder call path as the browser terminal
// (newSessionRecorder — a nil RecordingStore here makes it a documented
// no-op, but the call itself, and the session.attach{transport:ssh} audit
// it wraps, are exercised exactly as they would be in production).
func TestSSHGateway_ShellChannelDrivesFakeSession(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	fr := &sshFakeRunner{attachFn: func() (runner.Session, error) { return newFakeShellSession(), nil }}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	in, err := sess.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}

	payload := []byte("echo hi\n")
	if _, err := in.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(out, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != string(payload) {
		t.Errorf("echoed = %q, want %q (fakeShellSession echoes verbatim)", buf, payload)
	}

	ev := waitForAudit(t, h.audit, run.ID, "session.attach", "success")
	if ev == nil {
		t.Fatalf("no successful session.attach audit event; events=%s", auditDump(h.audit.snapshot(), run.ID))
	}
	if !strings.Contains(string(ev.Data), `"transport":"ssh"`) {
		t.Errorf("session.attach data = %s, want transport:ssh", ev.Data)
	}
}

// TestSSHGateway_MaxSessionsPerRunEnforced pins the per-run channel cap
// (maxSSHSessionsPerRun): the run's owner may hold that many concurrent
// "session" channels open, and the NEXT one is rejected at channel-open time
// (client.NewSession() itself errors — the cap bites before any shell/exec/
// subsystem request is even sent).
func TestSSHGateway_MaxSessionsPerRunEnforced(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	fr := &sshFakeRunner{attachFn: func() (runner.Session, error) { return newFakeShellSession(), nil }}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	var sessions []*ssh.Session
	defer func() {
		for _, s := range sessions {
			_ = s.Close()
		}
	}()
	for i := 0; i < maxSSHSessionsPerRun; i++ {
		sess, err := client.NewSession()
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		// A real (never-written, never-closed) StdinPipe — NOT a bare
		// Shell() with no Stdin configured. Session.Shell wires an EMPTY
		// bytes.Buffer as Stdin when none is set, and the client's own
		// background copy goroutine hits that buffer's immediate EOF and
		// sends channel EOF right back — which tears the session down
		// server-side (and frees its cap slot) before this loop even reaches
		// its next iteration, so EVERY session "succeeds" and the cap looks
		// unenforced. StdinPipe keeps the channel genuinely open.
		if _, err := sess.StdinPipe(); err != nil {
			t.Fatalf("session %d stdin pipe: %v", i, err)
		}
		if err := sess.Shell(); err != nil {
			t.Fatalf("session %d shell: %v", i, err)
		}
		sessions = append(sessions, sess)
	}

	if _, err := client.NewSession(); err == nil {
		t.Errorf("session over maxSSHSessionsPerRun=%d succeeded, want rejected", maxSSHSessionsPerRun)
	}
}

// TestSSHGateway_MaxConnectionsEnforced pins the total concurrent-connection
// cap (maxSSHConnections): a connection accepted over the cap is closed
// immediately, before any handshake byte is exchanged — checked at the raw
// TCP level (no auth needed to observe it; the cap bites before
// NewServerConn is even called).
func TestSSHGateway_MaxConnectionsEnforced(t *testing.T) {
	h := newSSHTestHarness(t, newSSHMemStore(), &sshFakeRunner{})

	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < maxSSHConnections; i++ {
		c, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		conns = append(conns, c)
	}

	over, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial over cap: %v", err)
	}
	defer over.Close()
	_ = over.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, rerr := over.Read(buf)
	if n > 0 || rerr == nil {
		t.Errorf("connection over maxSSHConnections got a byte / no error (n=%d, err=%v), want the server to close it immediately", n, rerr)
	}
}

// TestSSHGateway_HandshakeTimeoutFires pins the pre-auth DoS bound itself: a
// connection that never sends the SSH version string is closed by the
// server on its own within sshHandshakeTimeout, not held open forever —
// ssh.NewServerConn has no default timeout, so this deadline is the only
// thing that bounds it. Slow by design (waits out the real constant).
func TestSSHGateway_HandshakeTimeoutFires(t *testing.T) {
	h := newSSHTestHarness(t, newSSHMemStore(), &sshFakeRunner{})

	conn, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// Never send a client version string — the handshake can never
	// complete. This is NOT "zero bytes ever arrive": unlike the over-cap
	// case above (closed before ssh.NewServerConn is even called), an
	// under-cap connection DOES reach NewServerConn, which sends the
	// server's OWN version banner immediately as part of the protocol —
	// drain and discard it. What is under test is whether the connection
	// EVER closes on its own: a read deadline well past sshHandshakeTimeout
	// distinguishes "closed by the server's own deadline" (io.Copy reaches
	// EOF/an error before ours fires) from "held open forever" (ours fires
	// first, surfacing as a Timeout() net.Error).
	_ = conn.SetReadDeadline(time.Now().Add(sshHandshakeTimeout + 5*time.Second))
	_, rerr := io.Copy(io.Discard, conn)
	var netErr net.Error
	if errors.As(rerr, &netErr) && netErr.Timeout() {
		t.Fatalf("connection stayed open past sshHandshakeTimeout+5s margin — the server-side deadline never closed it")
	}
}

// ─── small helpers ─────────────────────────────────────────────────────────

// sshOwnedRunningRun seeds a store with one RUNNING, owned run and returns it
// alongside the principal, for the tests that don't need to exercise auth
// itself.
func sshOwnedRunningRun(t *testing.T) (*sshMemStore, types.AgentRun, string) {
	t.Helper()
	st := newSSHMemStore()
	const principal = "alice@example.com"
	run := types.AgentRun{ID: uuid.New(), CreatedBy: principal, State: types.RunRunning, SandboxRef: "sbx-1"}
	st.putRun(run)
	return st, run, principal
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

func containsPrefix(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// TestSSHGateway_MixedChannelTypesShareOneCap is B2′.
//
// TestSSHGateway_MaxSessionsPerRunEnforced above exercises "session" channels
// ONLY. Nothing proved that "direct-tcpip" (-L forwards) draws on the SAME
// per-run counter — which is the whole structural finding: Wardyn's cap is
// per-RUN and shared across channel TYPES, inverting the OpenSSH model, where
// MaxSessions scopes to session channels and a direct-tcpip is dispatched
// straight to the forward path. That inversion is why a client which opens
// shells and forwards together (VS Code Remote-SSH being the motivating case)
// can exhaust a cap that looks generous for shells alone.
//
// It also pins the audit, which did not exist before 0.7: a refusal used to be
// invisible to the deployment.
func TestSSHGateway_MixedChannelTypesShareOneCap(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	fr := &sshFakeRunner{
		attachFn: func() (runner.Session, error) { return newFakeShellSession(), nil },
		execFn:   func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeEchoExecSession(), nil },
	}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// Fill the cap with a MIX: one fewer session than the cap, then a forward.
	// If the two types had separate counters, both would fit with room to spare
	// and the assertion below would not fire.
	var sessions []*ssh.Session
	var conns []net.Conn
	defer func() {
		for _, s := range sessions {
			_ = s.Close()
		}
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < maxSSHSessionsPerRun-1; i++ {
		sess, err := client.NewSession()
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		// StdinPipe, not a bare Shell(): see the sibling test — an empty Stdin
		// buffer EOFs immediately and frees the slot before the next iteration,
		// which makes the cap look unenforced.
		if _, err := sess.StdinPipe(); err != nil {
			t.Fatalf("session %d stdin pipe: %v", i, err)
		}
		if err := sess.Shell(); err != nil {
			t.Fatalf("session %d shell: %v", i, err)
		}
		sessions = append(sessions, sess)
	}
	conn, err := client.Dial("tcp", "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("the forward that fills the cap must succeed: %v", err)
	}
	conns = append(conns, conn)

	// The cap is now full via N-1 sessions PLUS one forward. Both of the next
	// two must be refused, whichever type they are.
	t.Run("a further session is refused", func(t *testing.T) {
		if _, err := client.NewSession(); err == nil {
			t.Errorf("a session opened past the shared cap of %d — the forward above did not consume a slot, so the counter is NOT shared across channel types", maxSSHSessionsPerRun)
		}
	})
	t.Run("a further forward is refused", func(t *testing.T) {
		if c, err := client.Dial("tcp", "127.0.0.1:9999"); err == nil {
			_ = c.Close()
			t.Errorf("a forward opened past the shared cap of %d — the sessions above did not consume slots, so the counter is NOT shared across channel types", maxSSHSessionsPerRun)
		}
	})

	// ...and the refusal is visible to the deployment, not just to the client.
	t.Run("the refusal is audited, naming the channel type", func(t *testing.T) {
		ev := waitForAudit(t, h.audit, run.ID, "ssh.channel_rejected", "failure")
		if ev.Action == "" {
			t.Fatalf("a channel-cap refusal emitted no audit event: the client sees ResourceShortage and the deployment sees nothing. events=%s",
				auditDump(h.audit.snapshot(), run.ID))
		}
		var got []types.AuditEvent
		for _, e := range h.audit.snapshot() {
			if e.Action == "ssh.channel_rejected" {
				got = append(got, e)
			}
		}
		var sawSession, sawForward bool
		for _, ev := range got {
			if strings.Contains(string(ev.Data), `"session"`) {
				sawSession = true
			}
			if strings.Contains(string(ev.Data), `"direct-tcpip"`) {
				sawForward = true
			}
		}
		if !sawSession || !sawForward {
			t.Errorf("audit must distinguish the refused channel type (session=%v direct-tcpip=%v) — that distinction IS the shared-counter evidence", sawSession, sawForward)
		}
	})
}
