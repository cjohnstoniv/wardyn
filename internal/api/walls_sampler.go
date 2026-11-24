// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// The Walls view's live streams share one sampler per replica. While a stream is open it reads every decision row
// and every notice row once a second, in seq order, folds them per run without any viewer's scope, and keeps the
// last wallsRingBeats beats in a ring. Each stream projects the beats it is sent for its own caller
// (walls_stream.go), so what one caller may not see never decides what another is shown.

const (
	wallsTick           = time.Second      // one beat
	wallsRingBeats      = 120              // the beats a Last-Event-ID can resume within
	wallsRingMaxEntries = 200_000          // folds, host entries and samples across the ring
	wallsBeatLimit      = 5000             // rows read per beat
	wallsSamplerIdle    = 30 * time.Second // the sampler stops this long after its last stream closes
	wallsBeatTimeout    = 5 * time.Second  // one beat's reads end after this, and the beat is marked stale
	wallsRunMetaMax     = 50_000           // cached run to unit and creator entries
	wallsWarnEvery      = time.Minute      // a failed read is logged at most this often
	wallsFoldSamples    = 5                // samples one fold keeps of each kind
)

// wallsNoticeActions maps a stored action to its notice's wire action.
var wallsNoticeActions = map[string]string{
	"approval.decide": "decide", "approval.expire": "expire", "approval.cancel": "cancel",
	"run.policy.generation": "policy.generation", "credential.mint": "credential.mint", "credential.revoke": "revoke",
}

// wallsSampleActions is every action one beat reads: the three decision actions (the accessor adds egress.pending
// beside egress.hold) and the notice actions.
var wallsSampleActions = append([]string{"egress.allow", "egress.deny", "egress.hold"}, slices.Sorted(maps.Keys(wallsNoticeActions))...)

// wallsRunMeta is what the sampler keeps of a run: both are frozen when it is created.
type wallsRunMeta struct {
	UnitID    *uuid.UUID
	CreatedBy string
}

// wallsHostFold is one destination's tally in a fold: Weight counts decisions, Rows counts audit rows.
type wallsHostFold struct {
	Host         string
	Port         int
	Weight, Rows int64
}

// wallsSample is one decision row kept for a frame. It copies only the fields a frame may show: never an owner,
// a workload or a contact state.
type wallsSample struct {
	ID            uuid.UUID
	Action        string // as stored, for the unit view's envelope
	Class         wallsClass
	Late          bool
	Host          string
	Port          int
	Reason        string // the row's refusal reason
	PolicyContact string // the refusing layer's name
	MatchedRule   string
	ApprovalID    string
	DecidedAt     time.Time
}

// wallsRunFold is one run's rows in one beat, before any caller's view is applied.
type wallsRunFold struct {
	Meta     wallsRunMeta
	Counters WallsCounters     // by Weight; a late row counts only in Late
	Unit     WallsUnitCounters // one per row; control-plane-host and config rows left out
	Hosts    []wallsHostFold   // one entry per (host, port); config and control-plane-host rows left out
	Samples  []wallsSample     // the first 5 non-late refused rows, then the first 5 other rows that are neither config nor control plane, in seq order

	hostAt         map[wallsTopoHostKey]int // index into Hosts
	refused, other int                      // samples kept of each kind
}

// wallsNotice is a stored notice row: an approval, a run's policy or a credential changed.
type wallsNotice struct {
	Action     string
	RunID      uuid.UUID
	ApprovalID string
	Unit       *uuid.UUID // the run's unit, so a unit admin's stream can tell whether the run is theirs
}

// wallsBeat is one second of rows.
type wallsBeat struct {
	N       uint64
	T       time.Time // the beat's start, UTC, truncated to the second
	Stale   bool      // the row read or the run read failed: nothing in this beat is known
	Folds   map[uuid.UUID]*wallsRunFold
	Notices []wallsNotice
}

// entries is what the beat holds, for the ring's volume bound.
func (b wallsBeat) entries() int {
	n := len(b.Folds) + len(b.Notices)
	for _, f := range b.Folds {
		n += len(f.Hosts) + len(f.Samples)
	}
	return n
}

// wallsSampler is one replica's reader of decision rows. The zero value is ready to use.
type wallsSampler struct {
	mu        sync.Mutex
	ring      []wallsBeat
	volume    int // entries across the ring
	cursor    store.DecisionCursor
	floor     time.Time
	born      time.Time // when this state first ran; beat numbers never fall behind the whole ticks since
	lastN     uint64
	lostN     uint64 // the highest beat number cleared or evicted from the ring; a resume below it missed a beat
	subs      int
	idleSince time.Time
	running   bool
	runMeta   map[uuid.UUID]wallsRunMeta
	wake      chan struct{}
	warned    time.Time

	nonceOnce sync.Once
	nonceStr  string

	tick, idle time.Duration // zero means wallsTick and wallsSamplerIdle; tests only
	manual     bool          // tests only: subscribe starts no loop; the test calls beatOnce
	maxVolume  int           // tests only: zero means wallsRingMaxEntries
}

// subscribe counts one open stream and, for the first one with no loop running, starts the loop. The returned
// release is safe to call more than once.
func (w *wallsSampler) subscribe(s *Server) (release func()) {
	now := s.cfg.Now().UTC()
	w.mu.Lock()
	w.subs++
	if w.born.IsZero() {
		w.born = now
	}
	start := !w.running && !w.manual
	if start {
		w.running = true
		if w.floor.IsZero() {
			w.floor = now
		}
	}
	w.mu.Unlock()
	if start {
		ctx := s.cfg.BaseCtx
		if ctx == nil {
			ctx = context.Background()
		}
		s.goBackground(func() { w.run(ctx, s) })
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.subs--; w.subs == 0 {
				w.idleSince = time.Now()
			}
		})
	}
}

// run beats every tick until ctx is done or no stream has been open for the idle time.
func (w *wallsSampler) run(ctx context.Context, s *Server) {
	ticker := time.NewTicker(cmp.Or(w.tick, wallsTick))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.stop()
			return
		case <-ticker.C:
		}
		if w.stopIfIdle(cmp.Or(w.idle, wallsSamplerIdle)) {
			return
		}
		w.beatOnce(ctx, s)
	}
}

// stopIfIdle ends the loop's state when no stream has been open for idle. The check and the clearing share one
// hold of the lock, so a stream that subscribes meanwhile either keeps the loop or starts a new one.
func (w *wallsSampler) stopIfIdle(idle time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.subs > 0 || time.Since(w.idleSince) < idle {
		return false
	}
	w.clearLocked()
	return true
}

// stop clears the loop's state because the daemon is going away.
func (w *wallsSampler) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.clearLocked()
}

// clearLocked drops the ring, the cursor and the floor, and marks the loop not running. The beat number, the
// nonce and the start time stay, so a number is never reused after a stop; lostN rises to the head, so a resume from
// before the stop gets a gap.
func (w *wallsSampler) clearLocked() {
	w.lostN = w.lastN
	w.ring, w.volume = nil, 0
	w.cursor, w.floor = store.DecisionCursor{}, time.Time{}
	w.running = false
}

// nonce is this sampler's id in event ids, made once.
func (w *wallsSampler) nonce() string {
	w.nonceOnce.Do(func() {
		var b [12]byte
		_, _ = rand.Read(b[:])
		w.nonceStr = base64.RawURLEncoding.EncodeToString(b[:])
	})
	return w.nonceStr
}

// headN is the number of the newest beat made, which a stream that sent no Last-Event-ID starts after.
func (w *wallsSampler) headN() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastN
}

// since returns the stored beats with N above after, in order, with the head's N and the channel that is closed
// when the next beat is published. gap is true when after is below lostN, so a beat after it is gone, or above the
// head, a beat never made, so the caller cannot be told what it missed. Beat numbers are not dense, so a number
// the ring never held between two kept beats is not a gap.
func (w *wallsSampler) since(after uint64) (beats []wallsBeat, head uint64, wake <-chan struct{}, gap bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wake == nil {
		w.wake = make(chan struct{})
	}
	head, wake = w.lastN, w.wake
	if after > head || after < w.lostN {
		return nil, head, wake, true
	}
	if len(w.ring) == 0 {
		return nil, head, wake, false
	}
	i, _ := slices.BinarySearchFunc(w.ring, after, func(b wallsBeat, n uint64) int { return cmp.Compare(b.N, n+1) })
	return slices.Clone(w.ring[i:]), head, wake, false
}

// beatOnce reads one beat's rows and publishes it. A store that cannot read decision rows or runs makes no beat:
// the handlers have already answered 501.
func (w *wallsSampler) beatOnce(ctx context.Context, s *Server) {
	reader, ok := s.cfg.Store.(store.WallsAuditReader)
	if !ok {
		return
	}
	runs, ok := s.cfg.Store.(store.RunSweepReader)
	if !ok {
		return
	}
	start := s.cfg.Now().UTC()
	w.mu.Lock()
	if w.born.IsZero() {
		w.born = start
	}
	if w.floor.IsZero() {
		w.floor = start
	}
	floor, cursor := w.floor, w.cursor
	w.mu.Unlock()

	readCtx, cancelRead := context.WithTimeout(ctx, wallsBeatTimeout)
	defer cancelRead()
	rows, next, err := reader.AuditWallsRows(readCtx, store.DecisionQuery{
		Actions: wallsSampleActions, Since: floor, After: cursor, Limit: wallsBeatLimit,
	})
	var meta map[uuid.UUID]wallsRunMeta
	if err == nil {
		meta, err = w.runMetaFor(readCtx, runs, rows)
	}
	if err != nil {
		w.warn(ctx, "wardyn: the Walls view's live streams could not read a beat; its frames are marked stale", err)
		w.publish(start, wallsBeat{Stale: true, Folds: map[uuid.UUID]*wallsRunFold{}}, nil)
		return
	}
	beat := wallsFoldBeat(rows, meta)
	w.publish(start, beat, &wallsRead{rows: rows, next: next})
}

// wallsRead is a beat's rows and the cursor their read returned, which publish keeps for the next beat.
type wallsRead struct {
	rows []store.DecisionRow
	next store.DecisionCursor
}

// warn logs err at most once a minute.
func (w *wallsSampler) warn(ctx context.Context, msg string, err error) {
	w.mu.Lock()
	due := time.Since(w.warned) >= wallsWarnEvery
	if due {
		w.warned = time.Now()
	}
	w.mu.Unlock()
	if due {
		slog.WarnContext(ctx, msg, slog.Any("err", err))
	}
}

// runMetaFor is the unit and creator of every run in rows, reading the ones the cache lacks in one call.
func (w *wallsSampler) runMetaFor(ctx context.Context, runs store.RunSweepReader, rows []store.DecisionRow) (map[uuid.UUID]wallsRunMeta, error) {
	out := map[uuid.UUID]wallsRunMeta{}
	var missing []uuid.UUID
	w.mu.Lock()
	for _, r := range rows {
		if r.RunID == uuid.Nil {
			continue
		}
		if _, seen := out[r.RunID]; seen {
			continue
		}
		if m, ok := w.runMeta[r.RunID]; ok {
			out[r.RunID] = m
			continue
		}
		out[r.RunID] = wallsRunMeta{}
		missing = append(missing, r.RunID)
	}
	w.mu.Unlock()
	if len(missing) == 0 {
		return out, nil
	}
	found, err := runs.GetRunsByID(ctx, missing)
	if err != nil {
		return nil, err
	}
	for _, run := range found {
		m := wallsRunMeta{CreatedBy: run.CreatedBy}
		if run.UnitID != nil {
			u := *run.UnitID
			m.UnitID = &u
		}
		out[run.ID] = m
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.runMeta == nil || len(w.runMeta)+len(missing) > wallsRunMetaMax {
		w.runMeta = map[uuid.UUID]wallsRunMeta{}
	}
	for _, id := range missing {
		w.runMeta[id] = out[id]
	}
	return out, nil
}

// wallsFoldBeat folds rows into one beat's per-run tallies and its notices. A notice row is never classified and
// never counted. A row with no run is skipped.
func wallsFoldBeat(rows []store.DecisionRow, meta map[uuid.UUID]wallsRunMeta) wallsBeat {
	beat := wallsBeat{Folds: map[uuid.UUID]*wallsRunFold{}}
	type noticeKey struct {
		action, approval string
		run              uuid.UUID
	}
	seen := map[noticeKey]bool{}
	for _, row := range rows {
		if row.RunID == uuid.Nil {
			continue
		}
		m := meta[row.RunID]
		if wire, ok := wallsNoticeActions[row.Action]; ok {
			k := noticeKey{wire, row.ApprovalID, row.RunID}
			if !seen[k] {
				seen[k] = true
				beat.Notices = append(beat.Notices, wallsNotice{Action: wire, RunID: row.RunID, ApprovalID: row.ApprovalID, Unit: m.UnitID})
			}
			continue
		}
		if !strings.HasPrefix(row.Action, "egress.") {
			continue
		}
		fold := beat.Folds[row.RunID]
		if fold == nil {
			fold = &wallsRunFold{Meta: m}
			beat.Folds[row.RunID] = fold
		}
		fold.add(row, wallsClassify(row))
	}
	return beat
}

// add counts one decision row under its verdict.
func (f *wallsRunFold) add(row store.DecisionRow, v wallsVerdict) {
	f.Counters.add(v, row.Weight)
	if v.controlPlaneHost() || v.Class == wallsConfig {
		return
	}
	f.Unit.add(v)
	if row.Host != "" {
		key := wallsTopoHostKey{host: row.Host, port: row.Port}
		i, ok := f.hostAt[key]
		if !ok {
			if f.hostAt == nil {
				f.hostAt = map[wallsTopoHostKey]int{}
			}
			i = len(f.Hosts)
			f.hostAt[key] = i
			f.Hosts = append(f.Hosts, wallsHostFold{Host: row.Host, Port: row.Port})
		}
		f.Hosts[i].Weight += row.Weight
		f.Hosts[i].Rows++
	}
	sample := wallsSample{ID: row.ID, Action: row.Action, Class: v.Class, Late: v.Late, Host: row.Host, Port: row.Port,
		Reason: row.Reason, PolicyContact: row.LayerName, MatchedRule: row.MatchedRule, ApprovalID: row.ApprovalID, DecidedAt: row.DecidedAt}
	switch {
	case v.Class == wallsRefused && !v.Late && f.refused < wallsFoldSamples:
		f.refused++
		f.Samples = append(f.Samples, sample)
	case f.other < wallsFoldSamples:
		f.other++
		f.Samples = append(f.Samples, sample)
	}
}

// add counts n decisions under v. A late row counts only in Late, and Guest is the part of Refused a guest VM made.
func (c *WallsCounters) add(v wallsVerdict, n int64) {
	if v.Late {
		c.Late += n
		return
	}
	switch v.Class {
	case wallsAllowed:
		c.Allowed += n
	case wallsRefused:
		c.Refused += n
		if v.Guest {
			c.Guest += n
		}
	case wallsHeld:
		c.Held += n
	case wallsFault:
		c.Fault += n
	case wallsFidelity:
		c.Fidelity += n
	case wallsToolRule:
		c.ToolRule += n
	case wallsControl:
		c.Control += n
	case wallsConfig:
		c.Config += n
	default:
		c.Unclassified += n
	}
}

// add counts one row under v, late or not, as GET /audit lists it. The caller has already left out the rows
// naming the control plane's host and the upstream proxy's start rows.
func (c *WallsUnitCounters) add(v wallsVerdict) {
	switch v.Class {
	case wallsAllowed:
		c.Allowed++
	case wallsRefused:
		c.Refused++
		if v.Guest {
			c.Guest++
		}
	case wallsHeld:
		c.Held++
	case wallsFault:
		c.Fault++
	case wallsFidelity:
		c.Fidelity++
	default:
		c.Unclassified++
	}
}

// publish appends b to the ring as the next beat, then wakes every stream. read is the rows' read, whose cursor
// and floor the next beat starts from; a stale beat has none, so both stay and the rows are read again.
func (w *wallsSampler) publish(start time.Time, b wallsBeat, read *wallsRead) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var ticks uint64
	if d := start.Sub(w.born); d > 0 {
		ticks = uint64(d / cmp.Or(w.tick, wallsTick))
	}
	b.N, b.T = max(w.lastN+1, ticks), start.UTC().Truncate(time.Second)
	w.lastN = b.N
	if read != nil {
		w.cursor = read.next
		if n := len(read.rows); n > 0 {
			w.floor = read.rows[n-1].RecordedAt
		}
	}
	w.ring = append(w.ring, b)
	w.volume += b.entries()
	limit := cmp.Or(w.maxVolume, wallsRingMaxEntries)
	for len(w.ring) > wallsRingBeats || (w.volume > limit && len(w.ring) > 1) {
		w.volume -= w.ring[0].entries()
		w.lostN = w.ring[0].N
		w.ring[0] = wallsBeat{}
		w.ring = w.ring[1:]
	}
	if w.wake != nil {
		close(w.wake)
	}
	w.wake = make(chan struct{})
}
