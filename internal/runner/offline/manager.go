// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// Manager contains only local containment and evidence state. It cannot mint
// credentials or grant authority, and never chooses a run's terminal state.
type Manager struct {
	actionMu sync.Mutex
	lock     *os.File
	closed   bool
	replayMu sync.Mutex
	mu       sync.Mutex
	runnerID uuid.UUID
	// pendingLogged is when each held run's containment-pending warning was last logged.
	pendingLogged map[uuid.UUID]time.Time
	dir           string
	root          *os.Root
	sub           substrate.Substrate
	expectedRef   func(uuid.UUID) string
	outputSettled func(context.Context, uuid.UUID, string) (int64, bool, error)
	eraseOutput   func(uuid.UUID) error
	releaseOutput func(uuid.UUID, string) error
	stopper       runner.ProxyStopper
	// proofTimeout bounds the engine inspect behind a lost-identity proof.
	proofTimeout  time.Duration
	clock         *Clock
	now           func() time.Time
	since         func(time.Time) time.Duration
	online        bool
	replayReady   bool
	activityEpoch uuid.UUID
	activitySeen  map[uuid.UUID]uuid.UUID
	peer          *runnerwire.Peer
	state         localState
	dirty         bool
	inflight      map[mutationKey]int
	deadlines     map[uuid.UUID]deadline
	pending       map[uuid.UUID]runnerwire.Lease
	// selfLapse is Options.ProxySelfLapses (k8s runner): each proxy closes itself at its own
	// deadline, so a restart keeps a running proxy on a provisional cleanup deadline instead of
	// containing it, and the 30 s pod list records exits.
	selfLapse    bool
	observer     runner.ProxyObserver
	lastObserved time.Time
	tick         func(context.Context)
}

// Options are the manager's substrate-specific behaviours.
type Options struct {
	// ProxySelfLapses: the substrate's proxies lapse themselves (design K1 §6.2); the substrate must
	// implement runner.ProxyObserver.
	ProxySelfLapses bool
	// Tick runs first on each containment tick, before lapses and retirement: the k8s runner's
	// decision sink regrants and retries loss rows there.
	Tick func(context.Context)
}

type deadline struct {
	tokenAt, endsAt     time.Time
	receipt             time.Time
	tokenLeft, endsLeft time.Duration
}

// New recovers local state. Run must start before connecting: a restart cannot
// reconstruct a monotonic deadline, so existing proxies are contained until
// an authenticated fresh lease establishes one.
// eraseOutput deletes one run's retained output; the manager calls it only for
// an org-erased (asserted) run's quarantined evidence at its retention ceiling.
func New(stateDir string, runnerID uuid.UUID, sub substrate.Substrate, expectedRef func(uuid.UUID) string, outputSettled func(context.Context, uuid.UUID, string) (int64, bool, error), eraseOutput func(uuid.UUID) error) (*Manager, error) {
	if runnerID == uuid.Nil || sub == nil || substrate.IsRemote(sub) || expectedRef == nil || outputSettled == nil || eraseOutput == nil {
		return nil, errors.New("offline manager requires a registered runner and local substrate")
	}
	stopper, ok := sub.(runner.ProxyStopper)
	if !ok {
		return nil, runner.ErrEndUnsupported
	}
	dir := filepath.Join(stateDir, "offline")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("offline state directory is not private")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	lock, err := root.OpenFile("daemon.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		root.Close()
		return nil, err
	}
	st, err = lock.Stat()
	link, linkErr := root.Lstat("daemon.lock")
	if err != nil || linkErr != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || link.Mode()&os.ModeSymlink != 0 || !os.SameFile(st, link) {
		lock.Close()
		root.Close()
		return nil, errors.New("unsafe runner lifecycle lock")
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		root.Close()
		return nil, errors.New("runner lifecycle directory is already active")
	}
	m := &Manager{lock: lock, runnerID: runnerID, dir: stateDir, root: root, sub: sub, expectedRef: expectedRef, outputSettled: outputSettled, eraseOutput: eraseOutput, stopper: stopper, proofTimeout: 5 * time.Second, clock: NewClock(nil), now: time.Now, since: time.Since, deadlines: map[uuid.UUID]deadline{}, pending: map[uuid.UUID]runnerwire.Lease{}}
	if err := m.load(); err != nil {
		lock.Close()
		root.Close()
		return nil, err
	}
	return m, nil
}

// Close releases local state handles after Run and the connection loop stop.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	return errors.Join(m.root.Close(), m.lock.Close())
}

// Clock anchors a newly authenticated connection without extending deadlines
// already running on the local monotonic clock.
func (m *Manager) Clock(h runnerwire.Hello, received time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.online = false
	m.replayReady = false
	m.activityEpoch = uuid.Nil
	m.publishActivity()
	m.peer = nil
	m.clearPending()
	return m.clock.Hello(h, received)
}

// BeforeReady enables admission only after pending actions and fresh replay.
// It does not write to peer; resumed peers have not started their read loop.
func (m *Manager) BeforeReady(ctx context.Context, peer *runnerwire.Peer, _ bool) error {
	m.mu.Lock()
	revoked := m.state.Revoked
	m.mu.Unlock()
	if revoked {
		return runnerwire.ErrRevoked
	}
	if err := m.CheckLapses(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.state.Revoked {
		return runnerwire.ErrRevoked
	}
	if err := m.ensureDurable(); err != nil {
		return err
	}
	if peer == nil {
		return errors.New("offline admission requires an authenticated peer")
	}
	m.online = true
	m.replayReady = false
	m.activityEpoch = uuid.Nil
	m.publishActivity()
	m.peer = peer
	return nil
}

// Online permits new audit frames only after transport resume has replayed its
// old frames and started the peer. BeforeReady independently admits incoming RPCs.
func (m *Manager) Online(peer *runnerwire.Peer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.online && m.peer == peer && peer != nil && !m.state.Revoked {
		m.replayReady = true
		m.activityEpoch = uuid.New()
		m.activitySeen = map[uuid.UUID]uuid.UUID{}
		m.publishActivity()
	}
}

// Offline preserves agents and files. Run independently owns the safety timer;
// the connection's already-cancelled context cannot cancel containment.
func (m *Manager) Offline(context.Context) {
	m.mu.Lock()
	m.online = false
	m.replayReady = false
	m.activityEpoch = uuid.Nil
	m.publishActivity()
	m.peer = nil
	m.clearPending()
	m.mu.Unlock()
}

// Lease is checked synchronously before a subsequent CreateSandbox CALL. The
// org must send the initial lease first; no token is parsed without verification.
func (m *Manager) Lease(ctx context.Context, l runnerwire.Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !m.online || m.state.Revoked {
		return runner.ErrRunnerOffline
	}
	if l.RunID == uuid.Nil {
		return errors.New("runner lease lacks run identity")
	}
	if r := m.state.Runs[l.RunID]; r.ContainmentOnly || r.Quarantine != nil || r.Containment != nil || (!r.LeaseAfter.IsZero() && !l.TokenRenewedAt.After(r.LeaseAfter)) {
		return errors.New("containment-only identity cannot receive a lease")
	}
	tokenLeft, err := m.clock.TokenRemaining(l.TokenRenewedAt)
	if err != nil {
		return err
	}
	var endsLeft time.Duration
	if !l.EndsAt.IsZero() {
		endsLeft, err = m.clock.Remaining(l.EndsAt)
		if err != nil {
			return err
		}
	}
	if _, known := m.state.Runs[l.RunID]; !known {
		if _, exists := m.pending[l.RunID]; !exists && len(m.pending) >= 1024 {
			return errors.New("runner initial lease capacity reached")
		}
		if tokenLeft <= 0 || (!l.EndsAt.IsZero() && endsLeft <= 0) {
			return errors.New("runner initial lease is expired")
		}
		m.pending[l.RunID] = l
	}
	old, exists := m.deadlines[l.RunID]
	d := deadline{tokenAt: l.TokenRenewedAt, endsAt: l.EndsAt, receipt: m.now(), tokenLeft: tokenLeft, endsLeft: endsLeft}
	if exists && !l.TokenRenewedAt.After(old.tokenAt) {
		d.tokenAt = old.tokenAt
		d.tokenLeft = old.tokenLeft - m.since(old.receipt)
	}
	if exists && !l.EndsAt.IsZero() && l.EndsAt.Equal(old.endsAt) {
		d.endsLeft = old.endsLeft - m.since(old.receipt)
	}
	m.deadlines[l.RunID] = d
	if r, ok := m.state.Runs[l.RunID]; ok && !r.Tombstone {
		r.Lease = l
		m.state.Runs[l.RunID] = r
		return m.save()
	}
	return nil
}

// BeforeCreate commits a creation intent before any substrate side effect.
func (m *Manager) BeforeCreate(ctx context.Context, spec runner.SandboxSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !m.online || m.state.Revoked {
		return runner.ErrRunnerOffline
	}
	if err := m.ensureDurable(); err != nil {
		return err
	}
	if spec.RunID == uuid.Nil {
		return errors.New("runner create has no run id")
	}
	if _, exists := m.state.Runs[spec.RunID]; exists {
		return errors.New("runner run already has a creation intent")
	}
	if n := m.liveRuns(); spec.RunnerMaxRuns > 0 && n >= spec.RunnerMaxRuns {
		return runnerwire.Refuse("this runner already holds %d runs, its limit of runs at once (%d)", n, spec.RunnerMaxRuns)
	}
	l, ok := m.pending[spec.RunID]
	if !ok || m.expired(spec.RunID) {
		return errors.New("runner create requires a live initial lease")
	}
	ref := m.expectedRef(spec.RunID)
	if ref == "" {
		return errors.New("runner create requires a recoverable substrate reference")
	}
	m.state.Runs[spec.RunID] = localRun{RunID: spec.RunID, Ref: ref, Lease: l}
	if err := m.save(); err != nil {
		// A failed directory sync may follow a successful rename. Retain the
		// intent even on error so an ambiguous durable write cannot allow retry.
		return err
	}
	delete(m.pending, spec.RunID)
	m.beginMutation(runnerwire.MethodCreateSandbox, ref, uuid.Nil)
	return nil
}

// AdmitResident binds a runner_resident delivery to a run this runner may still create: online, not
// revoked, a live initial lease and no creation intent yet (the Lease and BeforeCreate predicates).
// Lock order: Resident calls this while holding its own mutex, so Resident.mu is always taken before
// m.mu. The manager never calls into Resident; the revoke hook runs Revoked, which has released m.mu,
// and only then the resident erase.
func (m *Manager) AdmitResident(runID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.online || m.state.Revoked {
		return runner.ErrRunnerOffline
	}
	if _, exists := m.state.Runs[runID]; exists {
		return errors.New("runner resident delivery after the run's creation intent")
	}
	if _, ok := m.pending[runID]; !ok || m.expired(runID) {
		return errors.New("runner resident delivery requires a live initial lease")
	}
	return nil
}

// IsRevoked reports the durable revocation latch.
func (m *Manager) IsRevoked() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.Revoked
}

// liveRuns counts the runs whose sandbox this runner still holds: created or being created, not failed,
// retired, quarantined, exited or stopped. It errs low, so the backstop to the control plane's count
// never refuses a run the control plane rightly placed. m.mu must be held.
func (m *Manager) liveRuns() int {
	n := 0
	for _, r := range m.state.Runs {
		if !r.Retired && !r.FailedCreation && !r.ContainmentOnly && r.Quarantine == nil && r.Exit == nil && r.ProxyStoppedAt == nil {
			n++
		}
	}
	return n
}

// Created durably binds the substrate ref before the dispatcher acknowledges
// creation. Wire cancellation cannot discard a successfully provisioned ref.
func (m *Manager) Created(ctx context.Context, spec runner.SandboxSpec, localRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	r, ok := m.state.Runs[spec.RunID]
	if !ok || localRef == "" || r.FailedCreation || r.ContainmentOnly || r.Quarantine != nil || r.Containment != nil || (r.Ref != "" && r.Ref != localRef) {
		return errors.New("runner creation has no matching durable intent")
	}
	r.Ref = localRef
	// The dispatcher binds the output writer before Created, so a run whose
	// create captured output says so here, before its sandbox is used.
	r.OutputCapture = spec.ExecOutput != nil
	m.state.Runs[spec.RunID] = r
	m.publishActivity()
	return m.save()
}

func (m *Manager) expired(id uuid.UUID) bool {
	d, ok := m.deadlines[id]
	if !ok {
		return true
	}
	elapsed := m.since(d.receipt)
	return elapsed < 0 || elapsed >= d.tokenLeft || (!d.endsAt.IsZero() && elapsed >= d.endsLeft)
}

// CheckLapses stops only proxies, never the agent or its files. It is also a
// deterministic fake-clock test seam; failed containment is retried on every tick.
func (m *Manager) CheckLapses(ctx context.Context) error {
	m.mu.Lock()
	ids := m.runIDs()
	m.mu.Unlock()
	var errs []error
	for _, id := range ids {
		m.mu.Lock()
		r := m.state.Runs[id]
		expired := m.state.Revoked || r.FailedCreation || (r.Containment != nil && !r.Containment.Contained) || (r.Quarantine != nil && r.ProxyStoppedAt == nil) || (r.MutationFailure != nil && !r.MutationFailure.Contained) || (r.ProxyExited != nil && r.ProxyStoppedAt == nil) || m.expired(id)
		m.mu.Unlock()
		if r.Retired || r.Ref == "" || !expired {
			continue
		}
		if err := m.stopper.StopProxy(ctx, r.Ref); err != nil {
			var pending *runner.ContainmentPendingError
			if errors.As(err, &pending) {
				// Held as containment pending: contained for READY only. No ProxyStoppedAt, no
				// receipt, no settlement; retried every tick until completion evidence (P2, R4-M2).
				if perr := m.holdPending(id, pending); perr != nil {
					errs = append(errs, perr)
				}
				if m.pendingLogDue(id) {
					slog.WarnContext(ctx, "runner containment pending", "run_id", id, "kind", pending.Kind, "error", pending.Err)
				}
				continue
			}
			errs = append(errs, fmt.Errorf("contain run %s: %w", id, err))
			continue
		}
		m.mu.Lock()
		current := m.state.Runs[id]
		if !sameContainmentTarget(r, current) {
			m.mu.Unlock()
			errs = append(errs, fmt.Errorf("contain run %s: mutation changed during containment", id))
			continue
		}
		r = current
		delete(m.pendingLogged, id)
		if r.ProxyStoppedAt == nil || (r.MutationFailure != nil && !r.MutationFailure.Contained) {
			before := r
			at := m.now().UTC()
			r.ProxyStoppedAt = &at
			r.ContainmentPending = nil // promoted to plain containment only on completion evidence
			if r.MutationFailure != nil {
				confirmed := *r.MutationFailure
				confirmed.Contained = true
				r.MutationFailure = &confirmed
			}
			m.state.Runs[id] = r
			if err := m.save(); err != nil {
				// Confirmation is not durable; retain the obligation independently of lease.
				m.state.Runs[id] = before
				errs = append(errs, err)
			}
		}
		m.mu.Unlock()
	}
	return errors.Join(errs...)
}

// Run owns containment independently of connection cancellation and replay IO.
// The caller supplies daemon lifetime, retaining it across reconnect attempts.
func (m *Manager) Run(ctx context.Context) error {
	replayCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	activityStopped := make(chan struct{})
	go func() { defer close(activityStopped); m.activityLoop(replayCtx) }()
	defer func() { cancel(); <-activityStopped }()
	stopped := make(chan struct{})
	go func() { defer close(stopped); m.replayLoop(replayCtx) }()
	defer func() { cancel(); <-stopped }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		safety, done := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		m.mu.Lock()
		tick := m.tick
		m.mu.Unlock()
		if tick != nil {
			tick(safety)
		}
		err := errors.Join(m.observeProxiesDue(safety), m.CheckLapses(safety), m.eraseAssertedEvidence())
		done()
		if err != nil {
			slog.ErrorContext(ctx, "runner containment will retry", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (m *Manager) replayLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		observe, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := m.observeExits(observe); err != nil {
			slog.WarnContext(ctx, "runner lifecycle observation will retry", "error", err)
		}
		m.mu.Lock()
		peer, ready := m.peer, m.online && m.replayReady
		m.mu.Unlock()
		if ready && peer != nil {
			if err := m.Replay(observe, peer); err != nil {
				slog.WarnContext(ctx, "runner audit replay will retry", "error", err)
			}
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Revoked latches the durable refusal before stopping every governed proxy.
// The organisation owns the pending terminal-state decision and cascade.
func (m *Manager) Revoked(ctx context.Context) error {
	m.mu.Lock()
	m.online = false
	m.replayReady = false
	m.activityEpoch = uuid.Nil
	m.publishActivity()
	m.peer = nil
	m.state.Revoked = true
	err := m.save()
	m.mu.Unlock()
	safety, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return errors.Join(err, m.CheckLapses(safety), m.eraseAssertedEvidence())
}

func (m *Manager) clearPending() {
	for id := range m.pending {
		if _, known := m.state.Runs[id]; !known {
			delete(m.deadlines, id)
		}
	}
	m.pending = map[uuid.UUID]runnerwire.Lease{}
}

// SetOptions applies Options before Run starts. ProxySelfLapses needs a substrate that observes its
// proxies, and gives every run that has no deadline yet (a restart) a provisional one, restart +
// RunTokenTTL: a cleanup bound, never sent to the proxy, which never clears an obligation. Call
// ObserveProxies first, so a run whose proxy already exited or is still Pending past its start
// timeout gets its obligation instead (P4).
func (m *Manager) SetOptions(o Options) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tick = o.Tick
	if !o.ProxySelfLapses {
		m.selfLapse, m.observer = false, nil
		return nil
	}
	obs, ok := m.sub.(runner.ProxyObserver)
	if !ok {
		return errors.New("proxy self-lapse requires a substrate that observes its proxies")
	}
	m.selfLapse, m.observer = true, obs
	return nil
}

// ProvisionalDeadlines gives each held run with no deadline and no obligation the provisional
// cleanup deadline restart + RunTokenTTL (design K1 §6.2 "After a restart"). The org's next lease
// replaces it.
func (m *Manager) ProvisionalDeadlines() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.selfLapse {
		return
	}
	now := m.now()
	for id, r := range m.state.Runs {
		if _, ok := m.deadlines[id]; ok || r.Retired || r.Ref == "" || r.ProxyExited != nil || r.ProxyStoppedAt != nil {
			continue
		}
		m.deadlines[id] = deadline{receipt: now, tokenLeft: identity.RunTokenTTL}
	}
}

// proxyObserveInterval is the k8s runner's pod-list cadence (design K1 §6.2 P1).
const proxyObserveInterval = 30 * time.Second

func (m *Manager) observeProxiesDue(ctx context.Context) error {
	m.mu.Lock()
	due := m.selfLapse && m.now().Sub(m.lastObserved) >= proxyObserveInterval
	m.mu.Unlock()
	if !due {
		return nil
	}
	return m.ObserveProxies(ctx)
}

// ObserveProxies lists every held run's proxy in one call and records a ProxyExited obligation for
// each that is Failed, Succeeded, NotFound or Pending past its start timeout. A Running proxy, or
// one Pending within its timeout, records nothing.
func (m *Manager) ObserveProxies(ctx context.Context) error {
	m.mu.Lock()
	if !m.selfLapse {
		m.mu.Unlock()
		return nil
	}
	m.lastObserved = m.now()
	// Every unretired run is listed, held ones included, so the substrate's per-tick checks (the
	// name-reuse list, identity sampling) keep covering them; only open runs take an obligation.
	var ids []uuid.UUID
	open := map[uuid.UUID]bool{}
	for _, id := range m.runIDs() {
		if r := m.state.Runs[id]; !r.Retired && r.Ref != "" {
			ids = append(ids, id)
			open[id] = r.ProxyExited == nil && r.ProxyStoppedAt == nil
		}
	}
	obs := m.observer
	m.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	seen, err := obs.ObserveProxies(ctx, ids)
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	for id, o := range seen {
		if open[id] && (o.Exited() || o == runner.ProxyPendingPastStart) {
			errs = append(errs, m.ProxyExited(id, string(o)))
		}
	}
	return errors.Join(errs...)
}

// ProxyExited records the durable obligation that a run's proxy will not serve again: on the
// proxy's validated lapsed frame, or an observed terminal phase or missing pod. A sink session
// ending is not evidence. CheckLapses contains the obligation; a NotFound records it but never
// ends it, since ProxyStoppedAt still needs completion evidence.
func (m *Manager) ProxyExited(runID uuid.UUID, evidence string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.state.Runs[runID]
	if !ok || r.Retired || r.ProxyExited != nil {
		return nil
	}
	r.ProxyExited = &proxyExit{Evidence: evidence, At: m.now().UTC()}
	m.state.Runs[runID] = r
	return m.save()
}

// holdPending records (or refreshes) a run's containment-pending state.
func (m *Manager) holdPending(id uuid.UUID, p *runner.ContainmentPendingError) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.holdPendingLocked(id, p)
}

// holdPendingLocked records a run as containment pending. m.mu is held.
func (m *Manager) holdPendingLocked(id uuid.UUID, p *runner.ContainmentPendingError) error {
	r, ok := m.state.Runs[id]
	if !ok {
		return nil
	}
	next := &runnerwire.ContainmentPending{Kind: p.Kind, Since: m.now().UTC(), NameReused: p.NameReused, NoCausalAssociation: p.NoCausalAssociation, Tuple: p.Tuple}
	if old := r.ContainmentPending; old != nil {
		next.Since = old.Since
		if reflect.DeepEqual(old, next) {
			return nil
		}
	}
	r.ContainmentPending = next
	m.state.Runs[id] = r
	return m.save()
}

// pendingLogEvery bounds the containment-pending warning, which CheckLapses would otherwise repeat
// every tick, to one per run per interval.
const pendingLogEvery = time.Minute

// pendingLogDue reports whether run id's containment-pending warning is due, and if so marks it.
func (m *Manager) pendingLogDue(id uuid.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if last, ok := m.pendingLogged[id]; ok && now.Sub(last) < pendingLogEvery {
		return false
	}
	if m.pendingLogged == nil {
		m.pendingLogged = map[uuid.UUID]time.Time{}
	}
	m.pendingLogged[id] = now
	return true
}

// LoadEvidence returns a run's substrate evidence record (nil when none).
func (m *Manager) LoadEvidence(runID uuid.UUID) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.state.Runs[runID].Substrate), nil
}

// SaveEvidence writes a run's substrate evidence record with the state's atomic write and fsync. A
// run the runner holds no record of (a lost identity met by the sweep) gets a containment-only
// record, which CheckLapses contains.
func (m *Manager) SaveEvidence(runID uuid.UUID, b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runID == uuid.Nil {
		return errors.New("evidence for no run")
	}
	r, ok := m.state.Runs[runID]
	if !ok {
		ref := m.expectedRef(runID)
		if ref == "" {
			return errors.New("evidence for a run with no recoverable reference")
		}
		r = localRun{RunID: runID, Ref: ref, ContainmentOnly: true, Lease: runnerwire.Lease{RunID: runID}}
	}
	before := m.state.Runs[runID]
	r.Substrate = slices.Clone(b)
	m.state.Runs[runID] = r
	if err := m.save(); err != nil {
		if ok {
			m.state.Runs[runID] = before
		} else {
			delete(m.state.Runs, runID)
		}
		return err
	}
	return nil
}

// SinkRunIDs lists the runs whose decision sink is registered again at startup: those holding a
// substrate evidence record that are neither retired nor erased, so a spool is never reopened.
func (m *Manager) SinkRunIDs() []uuid.UUID {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []uuid.UUID
	for _, id := range m.runIDs() {
		if r := m.state.Runs[id]; len(r.Substrate) > 0 && !r.Retired && !r.EvidenceErased {
			ids = append(ids, id)
		}
	}
	return ids
}

// EvidenceRunIDs lists the unretired runs holding a substrate evidence record, for the relay's
// fence replay: a fence is dropped with the run's offline state at retirement.
func (m *Manager) EvidenceRunIDs() []uuid.UUID {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []uuid.UUID
	for _, id := range m.runIDs() {
		if r := m.state.Runs[id]; len(r.Substrate) > 0 && !r.Retired {
			ids = append(ids, id)
		}
	}
	return ids
}
