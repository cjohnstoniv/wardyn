// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// The local containment latch (wardyn-runnerd contain). The command writes a request into the inbox, a directory in
// the private offline state directory; the running manager latches each named run (or every run) on its next tick, or
// in New when serve was down, saves the latch durably and answers with a receipt. A latched run's proxy is stopped
// by CheckLapses like any contained run, every later lease for it is dropped, and the organisation learns of it from
// the next fresh session's STATE page. Nothing clears a latch but the run's retirement.
const (
	containInbox     = "contain"
	containReqSuffix = ".req"
	containDone      = ".done"
	// containReceiptKeep is how long an unread receipt stays: a contain that found no serve never reads its own.
	containReceiptKeep = time.Hour
	containReqLimit    = 1 << 20
)

// The receipt outcomes besides an error's text.
const (
	containStopped = "stopped"
	containPending = "pending"
	containUnknown = "unknown"
	containNoProxy = "no_proxy"
	containRetired = "retired"
)

// containRequest is one request file, as wardyn-runnerd contain writes it.
type containRequest struct {
	Runs        []uuid.UUID `json:"runs,omitempty"`
	All         bool        `json:"all,omitempty"`
	RequestedAt time.Time   `json:"requested_at"`
	UID         int         `json:"uid"`
}

// containReceipt is one request's answer: each targeted run's outcome, or why the request could not be read.
type containReceipt struct {
	Runs  map[uuid.UUID]string `json:"runs,omitempty"`
	Error string               `json:"error,omitempty"`
}

// latchState is the manager's latch bookkeeping, guarded by m.mu.
type latchState struct {
	// onLatch is told, outside m.mu, each time a pass latched a run while the manager is online: serve passes the
	// connection's RequireFresh, so the next session's STATE page carries the latch before READY.
	onLatch func()
	// waits are the requests applied and not yet answered: their receipts are written after the next CheckLapses.
	waits []containWait
	// leaseLogged is each latched run whose dropped lease was logged.
	leaseLogged map[uuid.UUID]bool
}

type containWait struct {
	id      uuid.UUID
	runs    []uuid.UUID
	unknown map[uuid.UUID]bool
	err     string
}

// SetOnLatch sets the callback a latch applied while online calls (once per pass, outside m.mu).
func (m *Manager) SetOnLatch(f func()) {
	m.mu.Lock()
	m.latch.onLatch = f
	m.mu.Unlock()
}

// flockExclusive takes the lifecycle lock, three tries 50 ms apart: contain's probe holds a shared lock for two system
// calls, and a starting serve must not fail on it. A second serve is still refused.
func flockExclusive(f *os.File) error {
	var err error
	for i := range 3 {
		if i > 0 {
			time.Sleep(50 * time.Millisecond)
		}
		if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return nil
		}
	}
	return err
}

// dropLatchedLease reports whether l is for a latched run, logging its first dropped lease. The caller holds m.mu.
func (m *Manager) dropLatchedLease(ctx context.Context, l runnerwire.Lease) bool {
	if m.state.Runs[l.RunID].LocalContain == nil {
		return false
	}
	if !m.latch.leaseLogged[l.RunID] {
		if m.latch.leaseLogged == nil {
			m.latch.leaseLogged = map[uuid.UUID]bool{}
		}
		m.latch.leaseLogged[l.RunID] = true
		slog.WarnContext(ctx, "runner drops leases for a run a local operator contained", "run_id", l.RunID)
	}
	return true
}

// applyContainRequests latches the runs each inbox request names, saves the latch, then deletes the request; its
// receipt waits for the next CheckLapses. A request that cannot be read is refused, logged and answered with why.
func (m *Manager) applyContainRequests(ctx context.Context) {
	reqs := m.readContainInbox(ctx)
	if len(reqs) == 0 {
		return
	}
	m.mu.Lock()
	latched := false
	var waits []containWait
	for _, q := range reqs {
		w := containWait{id: q.id, err: q.err, unknown: map[uuid.UUID]bool{}}
		if q.err == "" {
			var got bool
			w.runs, got = m.latchRuns(q.req, w.unknown)
			latched = latched || got
		}
		waits = append(waits, w)
	}
	var err error
	if latched {
		err = m.save()
	} else {
		err = m.ensureDurable()
	}
	if err != nil {
		// The latch stays in memory, so CheckLapses still stops the proxies; the requests stay for the next pass.
		m.mu.Unlock()
		slog.ErrorContext(ctx, "runner could not save a local containment; it retries next tick", "err", err)
		return
	}
	m.latch.waits = append(m.latch.waits, waits...)
	notify := latched && m.online && m.latch.onLatch != nil
	cb := m.latch.onLatch
	m.mu.Unlock()
	for _, q := range reqs {
		if err := m.root.Remove(containInbox + "/" + q.id.String() + containReqSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.WarnContext(ctx, "runner could not remove an applied containment request", "request", q.id, "err", err)
		}
	}
	if notify {
		cb()
	}
}

// latchRuns sets the latch on each run req names (every non-retired run for all) that has none, and answers the runs
// it targets and whether it latched one; a named run this runner does not hold goes into unknown. The caller holds
// m.mu.
func (m *Manager) latchRuns(req containRequest, unknown map[uuid.UUID]bool) (ids []uuid.UUID, latched bool) {
	scope := "run"
	ids = req.Runs
	if req.All {
		scope, ids = "all", nil
		for _, id := range m.runIDs() {
			if !m.state.Runs[id].Retired {
				ids = append(ids, id)
			}
		}
	}
	for _, id := range ids {
		r, ok := m.state.Runs[id]
		if !ok {
			unknown[id] = true
			continue
		}
		if r.LocalContain != nil || r.Retired {
			continue
		}
		r.LocalContain = &runnerwire.LocalContain{At: req.RequestedAt.UTC(), Scope: scope, UID: req.UID}
		m.state.Runs[id] = r
		latched = true
	}
	return ids, latched
}

type inboxRequest struct {
	id  uuid.UUID
	req containRequest
	err string
}

// readContainInbox reads every request in the inbox through the offline root. The inbox must be a private directory,
// and each request a regular 0600 file named <uuid>.req, never a symlink; anything else is refused and logged, and a
// refused request named <uuid>.req is answered with why and removed.
func (m *Manager) readContainInbox(ctx context.Context) []inboxRequest {
	st, err := m.root.Lstat(containInbox)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil && (!st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0) {
		err = errors.New("it is not a private directory")
	}
	if err != nil {
		m.logRetry(ctx, slog.LevelError, "runner refuses its containment inbox", err)
		return nil
	}
	dir, err := m.root.Open(containInbox)
	if err != nil {
		m.logRetry(ctx, slog.LevelError, "runner could not read its containment inbox", err)
		return nil
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		m.logRetry(ctx, slog.LevelError, "runner could not read its containment inbox", err)
		return nil
	}
	var out []inboxRequest
	for _, name := range names {
		if base, ok := strings.CutSuffix(name, containDone); ok {
			m.pruneReceipt(base, name)
			continue
		}
		base, ok := strings.CutSuffix(name, containReqSuffix)
		id, perr := uuid.Parse(base)
		if !ok || perr != nil || id.String() != base {
			continue // a request still being written (contain renames it into place), or not one of ours
		}
		q := inboxRequest{id: id}
		if q.req, err = m.readContainRequest(name); err != nil {
			slog.ErrorContext(ctx, "runner refuses a containment request", "request", id, "err", err)
			q.err = "refused: " + err.Error()
		}
		out = append(out, q)
	}
	return out
}

func (m *Manager) readContainRequest(name string) (containRequest, error) {
	var req containRequest
	path := containInbox + "/" + name
	link, err := m.root.Lstat(path)
	if err != nil {
		return req, err
	}
	if link.Mode()&os.ModeSymlink != 0 || !link.Mode().IsRegular() || link.Mode().Perm() != 0600 {
		return req, errors.New("the request is not a regular 0600 file")
	}
	f, err := m.root.Open(path)
	if err != nil {
		return req, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !os.SameFile(st, link) {
		return req, errors.New("the request changed while it was read")
	}
	if err := json.NewDecoder(io.LimitReader(f, containReqLimit)).Decode(&req); err != nil {
		return req, errors.New("the request is not valid JSON")
	}
	if req.All == (len(req.Runs) > 0) {
		return req, errors.New("the request names neither runs nor all, or both")
	}
	return req, nil
}

// pruneReceipt removes a receipt nobody read within containReceiptKeep.
func (m *Manager) pruneReceipt(base, name string) {
	if _, err := uuid.Parse(base); err != nil {
		return
	}
	if st, err := m.root.Lstat(containInbox + "/" + name); err == nil && m.since(st.ModTime()) > containReceiptKeep {
		_ = m.root.Remove(containInbox + "/" + name)
	}
}

// writeContainReceipts answers each applied request with each run's outcome after lapses, the CheckLapses result of
// this tick: stopped once the proxy's stop is confirmed, pending while the run is held containment-pending, or the
// containment error. A receipt that cannot be written is retried next tick.
func (m *Manager) writeContainReceipts(lapses error) {
	m.mu.Lock()
	waits := m.latch.waits
	m.latch.waits = nil
	receipts := make([]containReceipt, len(waits))
	for i, w := range waits {
		receipts[i] = containReceipt{Error: w.err}
		if w.err != "" {
			continue
		}
		receipts[i].Runs = map[uuid.UUID]string{}
		for _, id := range w.runs {
			receipts[i].Runs[id] = m.containOutcome(id, w.unknown[id], lapses)
		}
	}
	m.mu.Unlock()
	var retry []containWait
	for i, w := range waits {
		if err := m.writeReceipt(w.id, receipts[i]); err != nil {
			slog.Warn("runner could not write a containment receipt; it retries next tick", "request", w.id, "err", err)
			retry = append(retry, w)
		}
	}
	if len(retry) > 0 {
		m.mu.Lock()
		m.latch.waits = append(retry, m.latch.waits...)
		m.mu.Unlock()
	}
}

// containOutcome is one run's receipt outcome. The caller holds m.mu.
func (m *Manager) containOutcome(id uuid.UUID, unknown bool, lapses error) string {
	r := m.state.Runs[id]
	switch {
	case unknown:
		return containUnknown
	case r.ProxyStoppedAt != nil:
		return containStopped
	case r.ContainmentPending != nil:
		return containPending
	case r.Retired:
		return containRetired
	case r.Ref == "":
		return containNoProxy
	}
	var failed errContain
	for _, e := range joinedErrors(lapses) {
		if errors.As(e, &failed) && failed.run == id {
			return failed.err.Error()
		}
	}
	return "the proxy is not yet confirmed stopped"
}

func (m *Manager) writeReceipt(id uuid.UUID, rc containReceipt) error {
	b, err := json.Marshal(rc)
	if err != nil {
		return err
	}
	tmp := containInbox + "/" + id.String() + ".done.tmp"
	_ = m.root.Remove(tmp)
	f, err := m.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
		return err
	}
	return m.root.Rename(tmp, containInbox+"/"+id.String()+containDone)
}
