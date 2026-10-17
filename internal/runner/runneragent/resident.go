// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// Keystore is where resident values live; production is package keystore,
// under this runner's own service.
type Keystore struct {
	Set    func(account, value string) error
	Get    func(account string) (string, error)
	Delete func(account string) error
	// DeleteAll deletes every item under this runner's service.
	DeleteAll func() error
	// Precheck refuses a value the keystore cannot take before any entry is
	// written or any keystore call is made.
	Precheck func(account, value string) error
}

// Resident is the runner half of runner_resident delivery (design 0.9 §7.4).
// Values live only in the keystore and in the sandbox or proxy they are
// applied to. The index of what this runner holds (never a value) is kept in
// the state directory, so a restarted runner can still erase at run end.
//
// Keystore operations are serialized per account: an account is busy from
// just before its keystore call until that call really returns, even when the
// caller stopped waiting (a keystore write that outlives its timeout). No
// second call starts on a busy account; an erase that meets one asks for the
// delete instead, and the call's own completion carries it out. A process-local
// mutex is enough because the state directory has one writer (its flock).
type Resident struct {
	keystore Keystore
	index    string
	admit    func(run uuid.UUID) error
	now      func() time.Time

	// mu guards idx and busy. Lock order: mu, then the lifecycle manager's
	// mutex, only through admit; the manager never calls into Resident. No
	// keystore call is made while mu is held.
	mu   sync.Mutex
	idx  residentIndex
	busy map[string]struct{}

	// The idle DeleteAll backstop. deleteAll is set while the call is really
	// running, as busy is for one account; while it is set a delivery refuses.
	deleteAll       bool
	backstopPending bool
	backstopAt      time.Time
	wasIdle         bool
}

// residentIndex is the saved index: what is held per run and grant, and the
// runs already erased here, which take no further delivery.
type residentIndex struct {
	Grants map[uuid.UUID]map[uuid.UUID]residentGrant `json:"grants"`
	Erased map[uuid.UUID]time.Time                   `json:"erased,omitempty"`
}

// residentGrant is one index entry; never a value.
type residentGrant struct {
	Sandbox   bool      `json:"sandbox,omitempty"`
	NotAfter  time.Time `json:"not_after"`
	State     string    `json:"state"`
	WrittenAt time.Time `json:"written_at"`
	// Unsettled: a write timed out, or was found in flight at start. It is
	// sticky: the value is deleted again until settleWindow has passed.
	Unsettled      bool      `json:"unsettled,omitempty"`
	EraseRequested bool      `json:"erase_requested,omitempty"`
	ErasedAt       time.Time `json:"erased_at,omitzero"`
	// ReportID identifies the current reportable outcome, ReportOutcome
	// (failed or erased); every change of outcome gets a fresh one.
	ReportID      uuid.UUID `json:"report_id,omitzero"`
	ReportOutcome string    `json:"report_outcome,omitempty"`
	Reported      bool      `json:"reported,omitempty"`
}

// Index entry states.
const (
	grantWriting = "writing"
	grantHeld    = "held"
	grantErased  = "erased"
)

// residentCeiling caps how long the runner keeps a value, whatever not_after
// the org sent (owner ruling: at most 24 h).
const residentCeiling = 24 * time.Hour

// backstopEvery is how often the DeleteAll backstop runs again while idle.
const backstopEvery = time.Hour

// residentRetention is how long a tombstone, or an erased entry the org never
// acknowledged, stays indexed.
const residentRetention = 7 * 24 * time.Hour

// errEraseIncomplete is retryable: the org erases again until it is nil.
var errEraseIncomplete = errors.New("resident erase incomplete")

// NewResident loads the index at stateDir/resident.json. admit binds a
// delivery to a run this runner may still create. An entry found writing was
// in flight when the runner stopped: it is marked for erasure and unsettled,
// and the first sweep deletes it.
func NewResident(ks Keystore, stateDir string, admit func(uuid.UUID) error, now func() time.Time) (*Resident, error) {
	r := &Resident{keystore: ks, index: filepath.Join(stateDir, "resident.json"), admit: admit, now: now, busy: map[string]struct{}{}}
	b, err := os.ReadFile(r.index)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &r.idx); err != nil {
			return nil, err
		}
	}
	if r.idx.Grants == nil {
		r.idx.Grants = map[uuid.UUID]map[uuid.UUID]residentGrant{}
	}
	if r.idx.Erased == nil {
		r.idx.Erased = map[uuid.UUID]time.Time{}
	}
	found := false
	for _, grants := range r.idx.Grants {
		for grant, g := range grants {
			if g.State == grantWriting {
				g.EraseRequested, g.Unsettled, found = true, true, true
				grants[grant] = g
			}
		}
	}
	if found {
		if err := r.saveLocked(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func residentAccount(run, grant uuid.UUID) string { return run.String() + "/" + grant.String() }

func (r *Resident) entry(run, grant uuid.UUID) (residentGrant, bool) {
	g, ok := r.idx.Grants[run][grant]
	return g, ok
}

func (r *Resident) put(run, grant uuid.UUID, g residentGrant) {
	if r.idx.Grants[run] == nil {
		r.idx.Grants[run] = map[uuid.UUID]residentGrant{}
	}
	r.idx.Grants[run][grant] = g
}

// setOutcomeLocked records a grant's reportable outcome. A change gets a fresh
// ReportID and is unreported; an unchanged outcome keeps its ID, so a resend
// is the same event. The caller holds mu and saves.
func (r *Resident) setOutcomeLocked(run, grant uuid.UUID, outcome string) {
	g, ok := r.entry(run, grant)
	if !ok || g.ReportOutcome == outcome {
		return
	}
	g.ReportOutcome, g.ReportID, g.Reported = outcome, uuid.New(), false
	r.put(run, grant, g)
}

func (r *Resident) drop(run, grant uuid.UUID) {
	delete(r.idx.Grants[run], grant)
	if len(r.idx.Grants[run]) == 0 {
		delete(r.idx.Grants, run)
	}
}

// The durable-save steps, swappable so a test can fault each one and see
// their order against the keystore calls.
var (
	syncFile   = (*os.File).Sync
	renameFile = os.Rename
)

// saveLocked rewrites the index durably, in the lifecycle manager's order
// (offline/state.go): exclusive temp create, write, sync, checked close,
// rename, directory sync. Any error returns before the keystore call the
// saved state guards. The caller holds mu.
func (r *Resident) saveLocked() error {
	b, err := json.Marshal(r.idx)
	if err != nil {
		return err
	}
	tmp := r.index + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(b)
	syncErr := syncFile(f)
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := renameFile(tmp, r.index); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(r.index))
	if err != nil {
		return err
	}
	defer dir.Close()
	return syncFile(dir)
}

// keystoreTimeout bounds how long a caller waits for one keystore call: an OS
// prompt nobody answers must not hold a delivery or an erase open. The call
// itself may run on; its account stays busy until it returns.
var keystoreTimeout = 30 * time.Second

// settleWindow is how long after a write that did not settle its value is
// still deleted again, in case the write lands late.
func settleWindow() time.Duration { return 2 * keystoreTimeout }

// bounded runs one keystore call under keystoreTimeout. On a timeout the call
// may still finish later.
func bounded[T any](ctx context.Context, f func() (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, keystoreTimeout)
	defer cancel()
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() { v, err := f(); done <- result{v, err} }()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func keystoreRefusal(err error) error {
	return runnerwire.Refuse("%s: %v", placement.ReasonRunnerKeystoreUnavailable, err)
}

// Call serves deliver_resident and erase_resident.
func (r *Resident) Call(ctx context.Context, peer *runnerwire.Peer, method string, args json.RawMessage) (any, error) {
	if method == runnerwire.MethodEraseResident {
		var a runnerwire.EraseResidentArgs
		if err := runnerwire.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		return nil, r.erase(ctx, peer, a.RunID)
	}
	var a runnerwire.DeliverResidentArgs
	if err := runnerwire.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	if a.RunID == uuid.Nil || a.GrantID == uuid.Nil || a.Value == "" {
		return nil, runnerwire.Refuse("a resident delivery names a run, a grant and a value")
	}
	return nil, r.deliver(ctx, a)
}

// deliver registers the grant as writing (durably) and marks its account busy
// in one critical section, then writes the value outside mu. The answer is read
// from the entry's final state, after the write and any delete it led to.
func (r *Resident) deliver(ctx context.Context, a runnerwire.DeliverResidentArgs) error {
	account := residentAccount(a.RunID, a.GrantID)
	if err := r.register(a, account); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- r.write(a.RunID, a.GrantID, account, a.Value) }()
	wait, cancel := context.WithTimeout(ctx, keystoreTimeout)
	defer cancel()
	var writeErr error
	select {
	case writeErr = <-done:
	case <-wait.Done():
		if err := r.unsettle(a.RunID, a.GrantID, account, wait.Err()); err != nil {
			return err
		}
		// The write finished between the timeout and the lock.
		writeErr = <-done
	}
	if writeErr != nil {
		return keystoreRefusal(writeErr)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.entry(a.RunID, a.GrantID); ok && g.State == grantHeld && !g.EraseRequested && !g.Unsettled {
		return nil
	}
	return runnerwire.Refuse("resident grant %s is not held: its run was erased while the value was written", a.GrantID)
}

// register is deliver's one critical section: the run is not erased here, the
// keystore can take the value, the run is admitted, its NotAfter is ahead
// (kept at most residentCeiling away), neither the account nor the DeleteAll
// backstop is busy, and the writing entry is durable before the account is
// marked busy. Any refusal leaves no new entry and issues no write.
func (r *Resident) register(a runnerwire.DeliverResidentArgs, account string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, erased := r.idx.Erased[a.RunID]; erased {
		return runnerwire.Refuse("run %s was already erased on this runner; it takes no further resident value", a.RunID)
	}
	if err := r.keystore.Precheck(account, a.Value); err != nil {
		return keystoreRefusal(err)
	}
	if err := r.admit(a.RunID); err != nil {
		return err
	}
	now := r.now()
	if a.NotAfter.IsZero() || !a.NotAfter.After(now) {
		return runnerwire.Refuse("a resident delivery needs a not_after still ahead")
	}
	notAfter := a.NotAfter
	if ceiling := now.Add(residentCeiling); notAfter.After(ceiling) {
		notAfter = ceiling
	}
	if _, inFlight := r.busy[account]; inFlight || r.deleteAll {
		return keystoreRefusal(errors.New("a keystore call for this grant is still in flight"))
	}
	prev, had := r.entry(a.RunID, a.GrantID)
	if had && prev.EraseRequested && prev.State != grantErased {
		return keystoreRefusal(errors.New("an erase of this grant is still pending"))
	}
	r.put(a.RunID, a.GrantID, residentGrant{Sandbox: a.Sandbox, NotAfter: notAfter, State: grantWriting, WrittenAt: now, Unsettled: had && prev.Unsettled})
	if err := r.saveLocked(); err != nil {
		if had {
			r.put(a.RunID, a.GrantID, prev)
		} else {
			r.drop(a.RunID, a.GrantID)
		}
		return keystoreRefusal(err)
	}
	r.busy[account] = struct{}{}
	return nil
}

// write is the one keystore write of a delivery; it owns the busy account
// until every call it makes has returned. A successful write that no erase
// asked to undo is held; otherwise the value is deleted before the account is
// free again.
func (r *Resident) write(run, grant uuid.UUID, account, value string) error {
	err := r.keystore.Set(account, value)
	r.mu.Lock()
	g, _ := r.entry(run, grant)
	if err == nil && !g.EraseRequested {
		g.State = grantHeld
		r.put(run, grant, g)
		if err = r.saveLocked(); err == nil {
			delete(r.busy, account)
			r.mu.Unlock()
			return nil
		}
	}
	g.EraseRequested = true
	r.put(run, grant, g)
	// The index on disk still lists the entry, so a crash before this save
	// re-erases it at start; the delete need not wait for it.
	saveErr := r.saveLocked()
	r.mu.Unlock()
	if err := r.remove(run, grant, account); err != nil {
		slog.Warn("runner resident delete after a write will retry on the sweep", "error", err)
	}
	return errors.Join(err, saveErr)
}

// unsettle runs when deliver stops waiting: a write still in flight marks the
// entry Unsettled (durably) and the delivery is refused. nil means the write
// had already finished.
func (r *Resident) unsettle(run, grant uuid.UUID, account string, cause error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, inFlight := r.busy[account]; !inFlight {
		return nil
	}
	g, _ := r.entry(run, grant)
	g.Unsettled = true
	r.put(run, grant, g)
	return keystoreRefusal(errors.Join(cause, r.saveLocked()))
}

// remove deletes a busy account outside mu, then records the result under mu
// and frees the account. A first erasure is a new reportable outcome; a repeat
// delete of an unsettled entry keeps its report.
func (r *Resident) remove(run, grant uuid.UUID, account string) error {
	err := r.keystore.Delete(account)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.busy, account)
	if err != nil {
		return err
	}
	g, ok := r.entry(run, grant)
	if !ok {
		return nil
	}
	g.State, g.ErasedAt = grantErased, r.now()
	r.put(run, grant, g)
	r.setOutcomeLocked(run, grant, runnerwire.ResidentErased)
	return r.saveLocked()
}

// boundedRemove waits keystoreTimeout for remove; the delete runs on, and its
// account stays busy, after a timeout.
func (r *Resident) boundedRemove(ctx context.Context, run, grant uuid.UUID) error {
	_, err := bounded(ctx, func() (struct{}, error) { return struct{}{}, r.remove(run, grant, residentAccount(run, grant)) })
	return err
}

// claimLocked marks the account of an entry that still needs deleting busy;
// false when a call is in flight on it. The caller holds mu.
func (r *Resident) claimLocked(run, grant uuid.UUID) bool {
	account := residentAccount(run, grant)
	if _, inFlight := r.busy[account]; inFlight || r.deleteAll {
		return false
	}
	r.busy[account] = struct{}{}
	return true
}

// erase deletes every value held for runID and reports each grant as a
// resident_erase event. One critical section tombstones the run (durably), so
// no later delivery is taken, and snapshots its entries; an entry with a call
// in flight is marked for erasure and reported failed, and that call's own
// completion deletes it. erase returns nil only once every entry is erased and
// its report acknowledged.
func (r *Resident) erase(ctx context.Context, peer *runnerwire.Peer, runID uuid.UUID) error {
	grants, deletes, err := r.beginErase(runID)
	if err != nil {
		return err
	}
	var errs []error
	for _, g := range deletes {
		if err := r.boundedRemove(ctx, runID, g); err != nil {
			errs = append(errs, err)
		}
	}
	for _, g := range grants {
		report, send, err := r.reportFor(runID, g)
		if err == nil && send {
			var data json.RawMessage
			if data, err = runnerwire.Marshal(report); err == nil {
				err = peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventResidentErase, RunID: &runID, Data: data})
			}
			if err == nil {
				err = r.markReported(runID, report)
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	r.mu.Lock()
	for _, e := range r.idx.Grants[runID] {
		if e.State != grantErased || !e.Reported {
			errs = append(errs, errEraseIncomplete)
			break
		}
	}
	if r.unindexLocked(r.now()) {
		errs = append(errs, r.saveLocked())
	}
	r.mu.Unlock()
	// The erase that empties the live set is a transition to idle.
	errs = append(errs, r.backstop(ctx, r.now()))
	return errors.Join(errs...)
}

// reportFor is the report erase sends for one grant: its persisted outcome.
// A grant not erased by now is a failed outcome, durable before it is sent.
// send is false once the current outcome was acknowledged.
func (r *Resident) reportFor(run, grant uuid.UUID) (runnerwire.ResidentErase, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entry(run, grant); ok && e.State != grantErased && e.ReportOutcome != runnerwire.ResidentEraseFailed {
		r.setOutcomeLocked(run, grant, runnerwire.ResidentEraseFailed)
		if err := r.saveLocked(); err != nil {
			return runnerwire.ResidentErase{}, false, err
		}
	}
	e, ok := r.entry(run, grant)
	return runnerwire.ResidentErase{GrantID: grant, Outcome: e.ReportOutcome, ReportID: e.ReportID}, ok && !e.Reported, nil
}

// beginErase is erase's critical section.
func (r *Resident) beginErase(runID uuid.UUID) (grants, deletes []uuid.UUID, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.idx.Erased[runID]; !ok {
		r.idx.Erased[runID] = r.now()
	}
	grants = slices.SortedFunc(maps.Keys(r.idx.Grants[runID]), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	for _, g := range grants {
		if e, _ := r.entry(runID, g); e.State != grantErased {
			e.EraseRequested = true
			r.put(runID, g, e)
			if _, inFlight := r.busy[residentAccount(runID, g)]; inFlight {
				r.setOutcomeLocked(runID, g, runnerwire.ResidentEraseFailed)
			}
		}
	}
	if err := r.saveLocked(); err != nil {
		return nil, nil, err
	}
	for _, g := range grants {
		if e, _ := r.entry(runID, g); e.State != grantErased && r.claimLocked(runID, g) {
			deletes = append(deletes, g)
		}
	}
	return grants, deletes, nil
}

// markReported records an acknowledged report only while the entry's current
// ReportID and outcome are still the ones acknowledged: a late
// acknowledgement of a failed report never marks a later erasure reported.
func (r *Resident) markReported(run uuid.UUID, report runnerwire.ResidentErase) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.entry(run, report.GrantID)
	if !ok || g.ReportID != report.ReportID || g.ReportOutcome != report.Outcome {
		return nil
	}
	g.Reported = true
	r.put(run, report.GrantID, g)
	return r.saveLocked()
}

// unindexLocked drops what no longer needs indexing: an erased, reported entry
// with no call in flight (an unsettled one only once its delete is past the
// settle window), and after residentRetention an erased entry never reported
// or a tombstone whose run holds nothing. The caller holds mu.
func (r *Resident) unindexLocked(now time.Time) bool {
	changed := false
	for run, grants := range r.idx.Grants {
		for grant, g := range grants {
			if _, inFlight := r.busy[residentAccount(run, grant)]; inFlight || g.State != grantErased {
				continue
			}
			settled := !g.Unsettled || !g.ErasedAt.Before(g.WrittenAt.Add(settleWindow()))
			if (g.Reported && settled) || now.Sub(g.ErasedAt) > residentRetention {
				r.drop(run, grant)
				changed = true
			}
		}
	}
	for run, at := range r.idx.Erased {
		if _, holds := r.idx.Grants[run]; !holds && now.Sub(at) > residentRetention {
			delete(r.idx.Erased, run)
			changed = true
		}
	}
	return changed
}

// EraseAll deletes every value this runner holds, on revoke. Every entry is
// marked for erasure first (durably), so one with a call in flight is deleted
// by that call's completion and any other by the sweep if its delete fails
// here. It sends no report: the org settles a revoked runner's erasures itself.
func (r *Resident) EraseAll(ctx context.Context) error {
	r.mu.Lock()
	for run, grants := range r.idx.Grants {
		for grant, g := range grants {
			if g.State != grantErased {
				g.EraseRequested = true
				r.put(run, grant, g)
			}
		}
	}
	if err := r.saveLocked(); err != nil {
		r.mu.Unlock()
		return err
	}
	type key struct{ run, grant uuid.UUID }
	var todo []key
	for run, grants := range r.idx.Grants {
		for grant, g := range grants {
			if g.State != grantErased && r.claimLocked(run, grant) {
				todo = append(todo, key{run, grant})
			}
		}
	}
	r.mu.Unlock()
	var errs []error
	for _, k := range todo {
		errs = append(errs, r.boundedRemove(ctx, k.run, k.grant))
	}
	return errors.Join(errs...)
}

// EraseExpired is the sweep. It deletes every value past its NotAfter, whether
// or not its run's end ever reached this runner, every value marked for
// erasure with no call in flight, and an unsettled value again while its
// settle window is open. Each is marked erased but unreported, and the run's
// next erase reports it. A value whose delete fails stays for the next sweep.
func (r *Resident) EraseExpired(ctx context.Context, now time.Time) error {
	type key struct{ run, grant uuid.UUID }
	var todo []key
	r.mu.Lock()
	for run, grants := range r.idx.Grants {
		for grant, g := range grants {
			due := g.State != grantErased && (g.EraseRequested || (!g.NotAfter.IsZero() && !now.Before(g.NotAfter)))
			again := g.State == grantErased && g.Unsettled && g.ErasedAt.Before(g.WrittenAt.Add(settleWindow()))
			if (due || again) && r.claimLocked(run, grant) {
				todo = append(todo, key{run, grant})
			}
		}
	}
	r.mu.Unlock()
	var errs []error
	for _, k := range todo {
		errs = append(errs, r.boundedRemove(ctx, k.run, k.grant))
	}
	r.mu.Lock()
	if r.unindexLocked(now) {
		errs = append(errs, r.saveLocked())
	}
	r.mu.Unlock()
	errs = append(errs, r.backstop(ctx, now))
	return errors.Join(errs...)
}

// backstop runs DeleteAll over this runner's service when the runner holds
// nothing: no entry writing or held and no keystore call in flight. It catches
// a value a keystore helper wrote after its entry was erased. It runs on the
// first idle check (the sweep at start), on each transition to idle, then at
// most every backstopEvery; one call at a time. The caller waits
// keystoreTimeout; the flag stays set until the call really returns. A timeout
// leaves the backstop pending for the next check; a failure waits for the next
// hourly slot, so a runner with no keystore is not retried every sweep.
func (r *Resident) backstop(ctx context.Context, now time.Time) error {
	r.mu.Lock()
	idle := len(r.busy) == 0 && !r.deleteAll
	for _, grants := range r.idx.Grants {
		for _, g := range grants {
			idle = idle && g.State == grantErased
		}
	}
	due := idle && (!r.wasIdle || r.backstopPending || !now.Before(r.backstopAt.Add(backstopEvery)))
	r.wasIdle = idle || r.deleteAll
	if !due {
		r.mu.Unlock()
		return nil
	}
	r.deleteAll, r.backstopPending, r.backstopAt = true, false, now
	r.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		err := r.keystore.DeleteAll()
		r.mu.Lock()
		r.deleteAll = false
		r.mu.Unlock()
		done <- err
	}()
	wait, cancel := context.WithTimeout(ctx, keystoreTimeout)
	defer cancel()
	select {
	case err := <-done:
		return err
	case <-wait.Done():
		r.mu.Lock()
		r.backstopPending = true
		r.mu.Unlock()
		return wait.Err()
	}
}

// SweepExpired runs EraseExpired every interval until ctx ends.
func (r *Resident) SweepExpired(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := r.EraseExpired(ctx, r.now()); err != nil {
			slog.WarnContext(ctx, "runner resident expiry sweep will retry", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// residentToken matches a whole JSON string that is a placeholder.
var residentToken = regexp.MustCompile(`"` + regexp.QuoteMeta(strings.TrimSuffix(placement.ResidentPlaceholder(uuid.Nil), uuid.Nil.String())) + `([0-9a-f-]{36})"`)

// value reads one delivered value of the run; sandbox says it is going into
// the sandbox, which only a grant delivered for the sandbox may. It is the
// only read path: it refuses a grant that is not held, is marked for erasure,
// has a keystore call in flight, or has reached its NotAfter.
func (r *Resident) value(ctx context.Context, run, grant uuid.UUID, sandbox bool) (string, error) {
	if r == nil {
		return "", runnerwire.Refuse("runner credential delivery is unavailable")
	}
	r.mu.Lock()
	g, ok := r.entry(run, grant)
	_, inFlight := r.busy[residentAccount(run, grant)]
	now := r.now()
	r.mu.Unlock()
	switch {
	case !ok:
		return "", runnerwire.Refuse("resident grant %s was not delivered to this runner for the run", grant)
	case g.State != grantHeld || g.EraseRequested || inFlight:
		return "", runnerwire.Refuse("resident grant %s is not held on this runner", grant)
	case !now.Before(g.NotAfter):
		return "", runnerwire.Refuse("resident grant %s is past its not_after", grant)
	case sandbox && !g.Sandbox:
		return "", runnerwire.Refuse("resident grant %s is for the runner's proxy only, never the sandbox", grant)
	}
	v, err := bounded(ctx, func() (string, error) { return r.keystore.Get(residentAccount(run, grant)) })
	if err != nil {
		return "", keystoreRefusal(err)
	}
	return v, nil
}

// fillJSON replaces every placeholder string in one JSON value.
func (r *Resident) fillJSON(ctx context.Context, run uuid.UUID, raw []byte) ([]byte, error) {
	var err error
	out := residentToken.ReplaceAllFunc(raw, func(m []byte) []byte {
		grant, ok := placement.ResidentGrantID(string(bytes.Trim(m, `"`)))
		if !ok || err != nil {
			err = errors.Join(err, runnerwire.Refuse("malformed resident placeholder"))
			return m
		}
		v, verr := r.value(ctx, run, grant, false)
		if verr != nil {
			err = verr
			return m
		}
		enc, _ := json.Marshal(v)
		return enc
	})
	return out, err
}

// fillProxyFields fills placeholders in a proxy configuration object, only
// under its credential-bearing fields (placement.CredentialProxyField, by the
// Go field name fieldOf maps each key to). A placeholder anywhere else, such
// as a detail string shown to the sandbox, stays literal.
func (r *Resident) fillProxyFields(ctx context.Context, run uuid.UUID, cfg []byte, fieldOf func(string) string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &obj); err != nil {
		return nil, err
	}
	filled := false
	for key, raw := range obj {
		if !residentToken.Match(raw) || !placement.CredentialProxyField(fieldOf(key)) {
			continue
		}
		v, err := r.fillJSON(ctx, run, raw)
		if err != nil {
			return nil, err
		}
		obj[key], filled = v, true
	}
	if !filled {
		return cfg, nil
	}
	return json.Marshal(obj)
}

// fill is the spec CreateSandbox gets: each placeholder in SecretEnv,
// ManagedFiles or a credential field of the proxy configuration replaced by
// its delivered value. The org's spec, and every copy the runner persists,
// keeps the placeholders.
func (r *Resident) fill(ctx context.Context, spec runner.SandboxSpec) (runner.SandboxSpec, error) {
	env := maps.Clone(spec.SecretEnv)
	for k, v := range env {
		if grant, ok := placement.ResidentGrantID(v); ok {
			val, err := r.value(ctx, spec.RunID, grant, true)
			if err != nil {
				return runner.SandboxSpec{}, err
			}
			env[k] = val
		}
	}
	files := slices.Clone(spec.ManagedFiles)
	for i, f := range files {
		if grant, ok := placement.ResidentGrantID(string(f.Content)); ok {
			val, err := r.value(ctx, spec.RunID, grant, true)
			if err != nil {
				return runner.SandboxSpec{}, err
			}
			files[i].Content = []byte(val)
		}
	}
	spec.SecretEnv, spec.ManagedFiles = env, files
	pc, err := json.Marshal(spec.ProxyConfig)
	if err != nil || !residentToken.Match(pc) {
		return spec, err
	}
	// runner.ProxyConfig has no JSON tags: a key is its Go field name.
	if pc, err = r.fillProxyFields(ctx, spec.RunID, pc, func(key string) string { return key }); err != nil {
		return runner.SandboxSpec{}, err
	}
	spec.ProxyConfig = runner.ProxyConfig{}
	return spec, json.Unmarshal(pc, &spec.ProxyConfig)
}

// proxyConfigFields maps each proxy.Config JSON key to its Go field name.
var proxyConfigFields = func() map[string]string {
	t := reflect.TypeFor[proxy.Config]()
	out := make(map[string]string, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		out[strings.Split(f.Tag.Get("json"), ",")[0]] = f.Name
	}
	return out
}()

// fillReplacedProxy re-applies resident values to the org's stripped stored
// proxy configuration on revive (ReplaceProxy).
func (r *Resident) fillReplacedProxy(ctx context.Context, cfg []byte) ([]byte, error) {
	if !residentToken.Match(cfg) {
		return cfg, nil
	}
	var head struct {
		RunID uuid.UUID `json:"run_id"`
	}
	if err := json.Unmarshal(cfg, &head); err != nil || head.RunID == uuid.Nil {
		return nil, runnerwire.Refuse("a proxy configuration with resident placeholders names no run")
	}
	return r.fillProxyFields(ctx, head.RunID, cfg, func(key string) string { return proxyConfigFields[key] })
}
