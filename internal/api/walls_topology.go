// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GET /api/v1/walls/topology serves the Walls view's whole picture from one snapshot taken at as_of: the units,
// the runs live now or ended inside the window, the hosts they reached and the counts on each. A security caller
// reads the organisation; a unit admin reads the runs of their own units, frozen to them at create, and only
// totals for the rest. The first page builds the snapshot; a continuation pages through its runs.

// The bounds of one topology read (EST).
const (
	wallsTopoDefaultWindow    = 15 * time.Minute
	wallsTopoMinWindow        = time.Minute
	wallsTopoElsewhereWindow  = 15 * time.Minute // the floor of a unit admin's totals for the rest
	wallsTopoDefaultLimit     = 1000
	wallsTopoMaxHosts         = 500
	wallsTopoRunHostEdges     = 8  // run_host edges per run, with edges=full
	wallsTopoRunPages         = 10 // run list pages read before the snapshot is partial
	wallsTopoSnapshotTTL      = 120 * time.Second
	wallsTopoSnapshotsMax     = 64 // per replica, the oldest evicted
	wallsTopoSnapshotsCaller  = 4  // per caller, their oldest evicted
	wallsTopoFirstPerMin      = 6
	wallsTopoContinuePerMin   = 60
	wallsTopoCursorIDBytes    = 16
	wallsTopoCursorOffsetSize = 4
)

// The two scopes a topology is read at.
const (
	wallsTopoScopeOrg  = "org"
	wallsTopoScopeUnit = "unit_subtree"
)

// The egress modes a unit's composed ceiling gives.
const (
	wallsTopoModeAllowAll  = "allow_all"
	wallsTopoModeSealed    = "sealed"
	wallsTopoModeAllowlist = "allowlist"
	wallsTopoModeUnknown   = "unknown"
)

// wallsTopoActions are the decision rows the topology counts.
var wallsTopoActions = []string{"egress.allow", "egress.deny", "egress.hold"}

// wallsTopoQuery is one request's parsed query.
type wallsTopoQuery struct {
	window time.Duration
	full   bool // edges=full
	limit  int
	cursor *wallsTopoCursor
}

// wallsTopoCursor names a snapshot and the offset of the next run in it. It carries no seq and no time.
type wallsTopoCursor struct {
	id     [wallsTopoCursorIDBytes]byte
	offset int
}

func (c wallsTopoCursor) encode() string {
	var b [wallsTopoCursorIDBytes + wallsTopoCursorOffsetSize]byte
	copy(b[:], c.id[:])
	binary.BigEndian.PutUint32(b[wallsTopoCursorIDBytes:], uint32(c.offset))
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func wallsTopoDecodeCursor(raw string) (wallsTopoCursor, bool) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) != wallsTopoCursorIDBytes+wallsTopoCursorOffsetSize {
		return wallsTopoCursor{}, false
	}
	var c wallsTopoCursor
	copy(c.id[:], b[:wallsTopoCursorIDBytes])
	c.offset = int(binary.BigEndian.Uint32(b[wallsTopoCursorIDBytes:]))
	return c, true
}

// wallsTopoParseQuery reads window (a Go duration from 1m to 15m), edges (summary or full), limit (1 to 1000)
// and cursor. ok is false for any other value.
func wallsTopoParseQuery(v url.Values) (q wallsTopoQuery, ok bool) {
	q = wallsTopoQuery{window: wallsTopoDefaultWindow, limit: wallsTopoDefaultLimit}
	if raw := v.Get("window"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < wallsTopoMinWindow || d > wallsTopoDefaultWindow {
			return q, false
		}
		q.window = d
	}
	switch v.Get("edges") {
	case "", "summary":
	case "full":
		q.full = true
	default:
		return q, false
	}
	if raw := v.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > wallsTopoDefaultLimit {
			return q, false
		}
		q.limit = n
	}
	if raw := v.Get("cursor"); raw != "" {
		c, ok := wallsTopoDecodeCursor(raw)
		if !ok {
			return q, false
		}
		q.cursor = &c
	}
	return q, true
}

// wallsTopoSnapshot is one first page's whole read, kept so its continuations page through the same runs. It
// belongs to the caller who opened it, at the scope they had then.
type wallsTopoSnapshot struct {
	id        [wallsTopoCursorIDBytes]byte
	principal string
	scope     string // wallsTopoScopeKey at open
	expires   time.Time
	asOf      time.Time
	windowSec int
	partial   bool
	security  bool
	full      bool
	items     []WallsRunItem
	runs      []wallsTopoSnapRun // aligned with items
	runHost   map[uuid.UUID][]WallsEdge
}

// wallsTopoSnapRun is what a page reads of one snapshot run, so a snapshot keeps no whole run row and no task text.
// wallsEgressOf reads ID and whether WallPreset is No network; projectAttention (attention, mayDecide,
// governingRule, runAuthoredBy) reads ID, State, LostReason, LostAt, CreatedBy, HumanAuthor and UnitID.
type wallsTopoSnapRun struct {
	id, unitID             uuid.UUID
	hasUnit, noNetwork     bool
	state                  types.RunState
	lostReason             types.LostReason
	lostAt                 *time.Time
	createdBy, humanAuthor string
}

// wallsTopoSlim keeps of each run only what wallsTopoSnapRun names.
func wallsTopoSlim(runs []types.AgentRun) []wallsTopoSnapRun {
	out := make([]wallsTopoSnapRun, len(runs))
	for i, r := range runs {
		out[i] = wallsTopoSnapRun{id: r.ID, hasUnit: r.UnitID != nil, noNetwork: noNetworkPreset(r.WallPreset), state: r.State,
			lostReason: r.LostReason, lostAt: r.LostAt, createdBy: r.CreatedBy, humanAuthor: r.HumanAuthor}
		if r.UnitID != nil {
			out[i].unitID = *r.UnitID
		}
	}
	return out
}

// run is the minimal row a page's egress and attention reads take: the fields above, and a WallPreset naming the
// built-in No network preset only when the run had it.
func (sr wallsTopoSnapRun) run() types.AgentRun {
	r := types.AgentRun{ID: sr.id, State: sr.state, LostReason: sr.lostReason, LostAt: sr.lostAt, CreatedBy: sr.createdBy,
		HumanAuthor: sr.humanAuthor}
	if sr.hasUnit {
		r.UnitID = &sr.unitID
	}
	if sr.noNetwork {
		r.WallPreset = &types.WallPresetOnRun{ID: types.WallPresetNoNetwork, Scope: types.WallPresetScopeBuiltin}
	}
	return r
}

// wallsSnapshotCache holds the open topology snapshots of this replica: at most wallsTopoSnapshotsMax, and at
// most wallsTopoSnapshotsCaller per caller, the oldest evicted first. Its zero value is ready to use.
type wallsSnapshotCache struct {
	mu    sync.Mutex
	byID  map[[wallsTopoCursorIDBytes]byte]*wallsTopoSnapshot
	order [][wallsTopoCursorIDBytes]byte // oldest first
}

// put stores snap, dropping expired snapshots, then the caller's oldest and the replica's oldest over the caps.
func (c *wallsSnapshotCache) put(snap *wallsTopoSnapshot, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil {
		c.byID = map[[wallsTopoCursorIDBytes]byte]*wallsTopoSnapshot{}
	}
	c.order = slices.DeleteFunc(c.order, func(id [wallsTopoCursorIDBytes]byte) bool {
		if s := c.byID[id]; s == nil || !now.Before(s.expires) {
			delete(c.byID, id)
			return true
		}
		return false
	})
	mine := 0
	for _, id := range c.order {
		if c.byID[id].principal == snap.principal {
			mine++
		}
	}
	for i := 0; i < len(c.order) && mine >= wallsTopoSnapshotsCaller; {
		if id := c.order[i]; c.byID[id].principal == snap.principal {
			delete(c.byID, id)
			c.order = slices.Delete(c.order, i, i+1)
			mine--
			continue
		}
		i++
	}
	for len(c.order) >= wallsTopoSnapshotsMax {
		delete(c.byID, c.order[0])
		c.order = c.order[1:]
	}
	c.byID[snap.id] = snap
	c.order = append(c.order, snap.id)
}

// get is the live snapshot id names, or nil.
func (c *wallsSnapshotCache) get(id [wallsTopoCursorIDBytes]byte, now time.Time) *wallsTopoSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.byID[id]
	if s == nil || !now.Before(s.expires) {
		return nil
	}
	return s
}

// wallsTopoCaps is s.wallsCaps with each zero field taken from the defaults.
func (s *Server) wallsTopoCaps() wallsCaps {
	c, d := s.wallsCaps, wallsDefaultCaps()
	for _, f := range []struct{ got, def *int }{
		{&c.runs, &d.runs}, {&c.readLimit, &d.readLimit}, {&c.reads, &d.reads}, {&c.countRows, &d.countRows}, {&c.runReads, &d.runReads},
	} {
		if *f.got <= 0 {
			*f.got = *f.def
		}
	}
	return c
}

// wallsTopoScope is what one request may read: the security projection (every run), or a unit admin's units.
// A unit admin with no scope reads the empty set.
type wallsTopoScope struct {
	security bool
	units    []uuid.UUID
	in       map[uuid.UUID]bool
}

// key is the scope as a snapshot compares it: a continuation is served only at the scope its first page had.
func (sc wallsTopoScope) key() string {
	if sc.security {
		return wallsTopoScopeOrg
	}
	ids := make([]string, len(sc.units))
	for i, u := range sc.units {
		ids[i] = u.String()
	}
	slices.Sort(ids)
	return wallsTopoScopeUnit + ":" + strings.Join(ids, ",")
}

// wallsTopoScopeFor reads the caller's scope for this request: the projection switch is isSecurityOperator, the
// predicate GET /runners is gated on; anyone else is a unit admin, scoped to unitAuditScopeFor's units, read
// now and never cached. A nil unit scope is the empty set.
func (s *Server) wallsTopoScopeFor(r *http.Request) wallsTopoScope {
	if s.isSecurityOperator(r.Context()) {
		return wallsTopoScope{security: true}
	}
	sc := wallsTopoScope{in: map[uuid.UUID]bool{}}
	if us := s.unitAuditScopeFor(r, false, types.AgentRun{}); us != nil {
		for _, u := range us.units {
			if !sc.in[u] {
				sc.in[u] = true
				sc.units = append(sc.units, u)
			}
		}
	}
	return sc
}

// handleWallsTopology is GET /api/v1/walls/topology?window=15m&edges=summary|full&limit=1000&cursor=.
func (s *Server) handleWallsTopology(w http.ResponseWriter, r *http.Request) {
	q, ok := wallsTopoParseQuery(r.URL.Query())
	if !ok {
		writeErrorReason(w, http.StatusBadRequest, reasonWallsQueryInvalid,
			"window must be a duration from 1m to 15m, edges summary or full, limit from 1 to 1000, and cursor one this route returned")
		return
	}
	first, continuation, _ := s.wallsLimiters()
	if q.cursor != nil {
		if s.refuseWallsRate(w, r, continuation, wallsTopoContinuePerMin) {
			return
		}
		s.wallsTopoContinue(w, r, q)
		return
	}
	if s.refuseWallsRate(w, r, first, wallsTopoFirstPerMin) {
		return
	}
	reader, ok := s.cfg.Store.(store.WallsAuditReader)
	pager, paged := s.cfg.Store.(store.RunsFilteredPager)
	if !ok || !paged {
		writeErrorReason(w, http.StatusNotImplemented, reasonWallsViewUnavailable, "this server's store cannot serve the Walls view")
		return
	}
	snap, body, ok := s.wallsTopoBuild(w, r, reader, pager, q)
	if !ok {
		return
	}
	items, runHost, next := s.wallsTopoPage(r, snap, 0, q.limit)
	if next != "" {
		s.wallsSnapshots.put(snap, s.cfg.Now())
	}
	body.Runs = WallsRuns{Items: items, NextCursor: next}
	if snap.full {
		body.Edges = WallsEdges{RunHost: runHost, NextCursor: next}
	}
	writeJSON(w, http.StatusOK, body)
}

// wallsTopoGone is the one answer for every continuation that names no live snapshot of this caller at this
// scope: expired, evicted, never opened, another caller's, or opened at a scope the caller no longer has. The
// bytes are the same for each, so the answer tells nothing about another caller's snapshot.
func wallsTopoGone(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusConflict, reasonWallsSnapshotGone,
		"the topology snapshot this page belongs to is gone; fetch the first page again")
}

// wallsTopoContinue serves one continuation page: the snapshot's next runs and, with edges=full, their edges.
func (s *Server) wallsTopoContinue(w http.ResponseWriter, r *http.Request, q wallsTopoQuery) {
	snap := s.wallsSnapshots.get(q.cursor.id, s.cfg.Now())
	if snap == nil || snap.principal != principalFromRequest(r) || q.cursor.offset > len(snap.items) {
		wallsTopoGone(w)
		return
	}
	if snap.scope != s.wallsTopoScopeFor(r).key() {
		wallsTopoGone(w)
		return
	}
	items, runHost, next := s.wallsTopoPage(r, snap, q.cursor.offset, q.limit)
	body := WallsTopologyPage{AsOf: snap.asOf, Scope: wallsTopoScopeUnit, WindowSec: snap.windowSec, Partial: snap.partial,
		Runs: WallsRuns{Items: items, NextCursor: next}}
	if snap.security {
		body.Scope = wallsTopoScopeOrg
	}
	if snap.full {
		body.Edges = WallsEdges{RunHost: runHost, NextCursor: next}
	}
	writeJSON(w, http.StatusOK, body)
}

// wallsTopoPage is the run items of snap from offset, at most limit, with the per-page fields set (a security
// caller's egress and attention), their run_host edges with edges=full, and the cursor of the next page.
func (s *Server) wallsTopoPage(r *http.Request, snap *wallsTopoSnapshot, offset, limit int) ([]WallsRunItem, []WallsEdge, string) {
	end := min(offset+limit, len(snap.items))
	items := slices.Clone(snap.items[offset:end])
	runs := make([]types.AgentRun, end-offset)
	for i, sr := range snap.runs[offset:end] {
		runs[i] = sr.run()
	}
	if snap.security && len(runs) > 0 {
		doors := s.wallsEgressOf(r.Context(), runs)
		if err := s.projectAttention(r, runs, true); err != nil {
			slog.WarnContext(r.Context(), "wardyn: the Walls view could not read the runs' attention; no run item carries it", slog.Any("err", err))
		}
		for i := range items {
			items[i].Egress = doors[runs[i].ID]
			items[i].Attention = runs[i].Attention
		}
	}
	var runHost []WallsEdge
	if snap.full {
		runHost = []WallsEdge{}
		for _, run := range runs {
			runHost = append(runHost, snap.runHost[run.ID]...)
		}
	}
	next := ""
	if end < len(snap.items) {
		next = wallsTopoCursor{id: snap.id, offset: end}.encode()
	}
	return items, runHost, next
}

// wallsTopoBuild reads the whole first page and the snapshot behind it. ok is false when it wrote an error.
func (s *Server) wallsTopoBuild(w http.ResponseWriter, r *http.Request, reader store.WallsAuditReader, pager store.RunsFilteredPager,
	q wallsTopoQuery) (*wallsTopoSnapshot, WallsTopology, bool) {
	ctx := r.Context()
	now := s.cfg.Now()
	asOf := now.UTC()
	caps := s.wallsTopoCaps()
	sc := s.wallsTopoScopeFor(r)
	snap := &wallsTopoSnapshot{principal: principalFromRequest(r), scope: sc.key(), expires: now.Add(wallsTopoSnapshotTTL),
		asOf: asOf, windowSec: int(q.window.Seconds()), security: sc.security, full: q.full}
	if _, err := rand.Read(snap.id[:]); err != nil {
		writeServerError(w, r, "open a walls topology snapshot", err)
		return nil, WallsTopology{}, false
	}
	body := WallsTopology{AsOf: asOf, Scope: wallsTopoScopeUnit, WindowSec: snap.windowSec, Units: []WallsUnit{}, Hosts: []WallsHost{}}
	if sc.security {
		body.Scope = wallsTopoScopeOrg
	}
	empty := !sc.security && len(sc.units) == 0
	var runs []types.AgentRun
	if !empty {
		var err error
		if runs, snap.partial, err = s.wallsTopoRuns(ctx, pager, sc, q.window, caps.runs); err != nil {
			writeServerError(w, r, "list walls topology runs", err)
			return nil, WallsTopology{}, false
		}
	}
	snap.runs = wallsTopoSlim(runs)
	fold := wallsTopoNewFold(sc.security, runs)
	if len(runs) > 0 {
		partial, err := wallsTopoReadRows(ctx, reader, fold, asOf.Add(-q.window), caps)
		if err != nil {
			writeServerError(w, r, "read walls topology rows", err)
			return nil, WallsTopology{}, false
		}
		snap.partial = snap.partial || partial
	}
	hosts, served := fold.hosts()
	body.Hosts = hosts
	if !q.full {
		body.Edges = WallsEdges{UnitHost: fold.unitHostEdges(served)}
	}
	snap.runHost = fold.runHostEdges(served)
	if !empty {
		units, ok := s.wallsTopoUnits(w, r, sc, fold)
		if !ok {
			return nil, WallsTopology{}, false
		}
		body.Units = units
	}
	if !sc.security && !empty {
		body.Elsewhere = s.wallsTopoElsewhere(ctx, sc, asOf, caps)
	}
	var ownerKind map[uuid.UUID]types.RunnerOwnerKind
	if sc.security {
		kinds, partial, ok := s.wallsTopoOrgExtras(w, r, &body, runs)
		if !ok {
			return nil, WallsTopology{}, false
		}
		ownerKind, snap.partial = kinds, snap.partial || partial
	}
	snap.items = fold.items(ownerKind)
	body.Partial = snap.partial
	return snap, body, true
}

// wallsTopoRuns is the snapshot's runs: live now or ended inside window, in ListRunsFiltered's order (live first by
// created_at, then ended by end time, newest first). A unit admin's runs are those frozen to one of their units:
// a run with no unit is dropped even when the Default unit is theirs, since RunUnitScope reads it as Default.
// At most keep runs are kept, each once; partial is set past it, or when the page bound stops the read and a probe
// of one more row finds the list goes on (a failed probe counts as more).
func (s *Server) wallsTopoRuns(ctx context.Context, pager store.RunsFilteredPager, sc wallsTopoScope, window time.Duration,
	keep int) (runs []types.AgentRun, partial bool, err error) {
	f := store.RunFilter{EndedWithin: window, IncludeKilled: true}
	if !sc.security {
		f.Scope = &store.RunUnitScope{IDs: slices.Clone(sc.units)}
	}
	page := store.Page{Limit: keep + 1}
	seen := map[uuid.UUID]bool{}
	for range wallsTopoRunPages {
		got, err := pager.ListRunsFiltered(ctx, f, page)
		if err != nil {
			return nil, false, err
		}
		for _, run := range got {
			if seen[run.ID] || (!sc.security && (run.UnitID == nil || !sc.in[*run.UnitID])) {
				continue
			}
			seen[run.ID] = true
			runs = append(runs, run)
		}
		if len(runs) > keep {
			return runs[:keep], true, nil
		}
		if len(got) < page.Limit {
			return runs, false, nil
		}
		page.Offset += page.Limit
	}
	more, err := pager.ListRunsFiltered(ctx, f, store.Page{Limit: 1, Offset: page.Offset})
	return runs, err != nil || len(more) > 0, nil
}

// wallsTopoReadRows pages the window's decision rows of the fold's runs into it, until a read returns the cursor
// it was sent (nothing left to scan) or after caps.reads reads. Only then does a probe of one row from the last
// cursor decide partial: set when it scans a row, or when it fails; unset when the rows ended exactly at the cap.
func wallsTopoReadRows(ctx context.Context, reader store.WallsAuditReader, fold *wallsTopoFold, since time.Time, caps wallsCaps) (partial bool, err error) {
	q := store.DecisionQuery{Actions: wallsTopoActions, Since: since, RunIDs: fold.runIDs(), Limit: caps.readLimit}
	for range caps.reads {
		rows, next, err := reader.AuditWallsRows(ctx, q)
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			fold.add(row)
		}
		if next == q.After {
			return false, nil
		}
		q.After = next
	}
	q.Limit = 1
	_, next, err := reader.AuditWallsRows(ctx, q)
	return err != nil || next != q.After, nil
}

// wallsTopoCount is one tally of classified rows: weighted for a security caller, one per row for a unit admin.
type wallsTopoCount struct {
	allowed, refused, guest, held, fault, fidelity, toolRule, unclassified, control, config, late int64
}

// add counts n for v. A security tally keeps a late row in late only; a unit admin's counts it in its class, as
// GET /audit does. guest is part of refused.
func (c *wallsTopoCount) add(v wallsVerdict, n int64, security bool) {
	if security && v.Late {
		c.late += n
		return
	}
	switch v.Class {
	case wallsAllowed:
		c.allowed += n
	case wallsRefused:
		c.refused += n
		if v.Guest {
			c.guest += n
		}
	case wallsHeld:
		c.held += n
	case wallsFault:
		c.fault += n
	case wallsFidelity:
		c.fidelity += n
	case wallsToolRule:
		c.toolRule += n
	case wallsControl:
		c.control += n
	case wallsConfig:
		c.config += n
	default:
		c.unclassified += n
	}
}

func (c *wallsTopoCount) total() int64 {
	return c.allowed + c.refused + c.held + c.fault + c.fidelity + c.toolRule + c.unclassified + c.control + c.config + c.late
}

// wire is the tally as the caller's projection serves it: *WallsCounters for a security caller, else
// *WallsUnitCounters, which has no control, tool_rule, config or late field.
func (c *wallsTopoCount) wire(security bool) any {
	if security {
		return &WallsCounters{Allowed: c.allowed, Refused: c.refused, Guest: c.guest, Held: c.held, Fault: c.fault, Fidelity: c.fidelity,
			ToolRule: c.toolRule, Unclassified: c.unclassified, Control: c.control, Config: c.config, Late: c.late}
	}
	return &WallsUnitCounters{Allowed: c.allowed, Refused: c.refused, Guest: c.guest, Held: c.held, Fault: c.fault, Fidelity: c.fidelity,
		Unclassified: c.unclassified}
}

// wallsTopoHostKey is one destination.
type wallsTopoHostKey struct {
	host string
	port int
}

// wallsTopoEdgeKey is one edge: a unit's or a run's traffic to one destination.
type wallsTopoEdgeKey struct {
	from uuid.UUID
	dest wallsTopoHostKey
}

// wallsTopoFold tallies the snapshot's rows by unit, run, host and edge.
type wallsTopoFold struct {
	security  bool
	runs      []types.AgentRun
	runByID   map[uuid.UUID]*types.AgentRun
	unit      map[uuid.UUID]*wallsTopoCount
	run       map[uuid.UUID]*wallsTopoCount
	host      map[wallsTopoHostKey]*wallsTopoCount
	unitHost  map[wallsTopoEdgeKey]*wallsTopoCount
	runHost   map[wallsTopoEdgeKey]*wallsTopoCount
	hostShown map[wallsTopoHostKey]bool // a unit admin's shape check, by destination
}

func wallsTopoNewFold(security bool, runs []types.AgentRun) *wallsTopoFold {
	f := &wallsTopoFold{security: security, runs: runs, runByID: make(map[uuid.UUID]*types.AgentRun, len(runs)),
		unit: map[uuid.UUID]*wallsTopoCount{}, run: map[uuid.UUID]*wallsTopoCount{}, host: map[wallsTopoHostKey]*wallsTopoCount{},
		unitHost: map[wallsTopoEdgeKey]*wallsTopoCount{}, runHost: map[wallsTopoEdgeKey]*wallsTopoCount{}, hostShown: map[wallsTopoHostKey]bool{}}
	for i := range runs {
		f.runByID[runs[i].ID] = &runs[i]
	}
	return f
}

func (f *wallsTopoFold) runIDs() []uuid.UUID {
	ids := make([]uuid.UUID, len(f.runs))
	for i, run := range f.runs {
		ids[i] = run.ID
	}
	return ids
}

// wallsTopoUnitOf is the unit a run's counts go to: its frozen unit, or the Default unit for a run with none (only a
// security caller reads such a run).
func wallsTopoUnitOf(run *types.AgentRun) uuid.UUID {
	if run.UnitID == nil {
		return store.DefaultUnitID
	}
	return *run.UnitID
}

func wallsTopoTally(m map[uuid.UUID]*wallsTopoCount, k uuid.UUID) *wallsTopoCount {
	c := m[k]
	if c == nil {
		c = &wallsTopoCount{}
		m[k] = c
	}
	return c
}

func wallsTopoEdgeTally(m map[wallsTopoEdgeKey]*wallsTopoCount, k wallsTopoEdgeKey) *wallsTopoCount {
	c := m[k]
	if c == nil {
		c = &wallsTopoCount{}
		m[k] = c
	}
	return c
}

// add counts one row. A security caller's counts are weighted and keep every class; the control plane's own
// host and the upstream start rows still never reach hosts or edges. A unit admin's counts are one per row and
// leave those rows out entirely, and a host reaches them only in the shape GET /audit's unit view serves it.
func (f *wallsTopoFold) add(row store.DecisionRow) {
	run := f.runByID[row.RunID]
	if run == nil {
		return
	}
	v := wallsClassify(row)
	hidden := v.controlPlaneHost() || v.Class == wallsConfig
	if hidden && !f.security {
		return
	}
	n := int64(1)
	if f.security {
		n = row.Weight
	}
	unit := wallsTopoUnitOf(run)
	wallsTopoTally(f.unit, unit).add(v, n, f.security)
	wallsTopoTally(f.run, run.ID).add(v, n, f.security)
	if hidden || row.Host == "" {
		return
	}
	dest := wallsTopoHostKey{host: row.Host, port: row.Port}
	if !f.security && !f.shown(dest) {
		return
	}
	c := f.host[dest]
	if c == nil {
		c = &wallsTopoCount{}
		f.host[dest] = c
	}
	c.add(v, n, f.security)
	wallsTopoEdgeTally(f.unitHost, wallsTopoEdgeKey{from: unit, dest: dest}).add(v, n, f.security)
	wallsTopoEdgeTally(f.runHost, wallsTopoEdgeKey{from: run.ID, dest: dest}).add(v, n, f.security)
}

// shown is whether the unit view would serve dest's host: unitViewData keeps host in {"host": h, "port": p}.
func (f *wallsTopoFold) shown(dest wallsTopoHostKey) bool {
	if ok, seen := f.hostShown[dest]; seen {
		return ok
	}
	ok := false
	if raw, err := json.Marshal(map[string]any{"host": dest.host, "port": dest.port}); err == nil {
		var kept map[string]json.RawMessage
		if json.Unmarshal(unitViewData(raw), &kept) == nil {
			_, ok = kept["host"]
		}
	}
	f.hostShown[dest] = ok
	return ok
}

// wallsTopoByCount orders destinations by count, highest first, then host and port.
func wallsTopoByCount(a, b wallsTopoHostKey, ca, cb int64) int {
	if c := cmp.Compare(cb, ca); c != 0 {
		return c
	}
	if c := strings.Compare(a.host, b.host); c != 0 {
		return c
	}
	return cmp.Compare(a.port, b.port)
}

// hosts is the top wallsTopoMaxHosts destinations by count, and the set of them, which every edge lands on.
func (f *wallsTopoFold) hosts() ([]WallsHost, map[wallsTopoHostKey]bool) {
	keys := make([]wallsTopoHostKey, 0, len(f.host))
	for k := range f.host {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b wallsTopoHostKey) int { return wallsTopoByCount(a, b, f.host[a].total(), f.host[b].total()) })
	if len(keys) > wallsTopoMaxHosts {
		keys = keys[:wallsTopoMaxHosts]
	}
	out := make([]WallsHost, len(keys))
	served := make(map[wallsTopoHostKey]bool, len(keys))
	for i, k := range keys {
		out[i] = WallsHost{Host: k.host, Port: k.port, Counters: f.host[k].wire(f.security)}
		served[k] = true
	}
	return out, served
}

// unitHostEdges is every unit's edge to a served host, by unit id, then count.
func (f *wallsTopoFold) unitHostEdges(served map[wallsTopoHostKey]bool) []WallsEdge {
	keys := make([]wallsTopoEdgeKey, 0, len(f.unitHost))
	for k := range f.unitHost {
		if served[k.dest] {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b wallsTopoEdgeKey) int {
		if c := strings.Compare(a.from.String(), b.from.String()); c != 0 {
			return c
		}
		return wallsTopoByCount(a.dest, b.dest, f.unitHost[a].total(), f.unitHost[b].total())
	})
	out := make([]WallsEdge, len(keys))
	for i, k := range keys {
		unit := k.from
		out[i] = WallsEdge{UnitID: &unit, Host: k.dest.host, Port: k.dest.port, Counters: f.unitHost[k].wire(f.security)}
	}
	return out
}

// runHostEdges is, per run, its top wallsTopoRunHostEdges edges to served hosts, by count.
func (f *wallsTopoFold) runHostEdges(served map[wallsTopoHostKey]bool) map[uuid.UUID][]WallsEdge {
	byRun := map[uuid.UUID][]wallsTopoEdgeKey{}
	for k := range f.runHost {
		if served[k.dest] {
			byRun[k.from] = append(byRun[k.from], k)
		}
	}
	out := make(map[uuid.UUID][]WallsEdge, len(byRun))
	for run, keys := range byRun {
		slices.SortFunc(keys, func(a, b wallsTopoEdgeKey) int {
			return wallsTopoByCount(a.dest, b.dest, f.runHost[a].total(), f.runHost[b].total())
		})
		if len(keys) > wallsTopoRunHostEdges {
			keys = keys[:wallsTopoRunHostEdges]
		}
		edges := make([]WallsEdge, len(keys))
		for i, k := range keys {
			id := k.from
			edges[i] = WallsEdge{RunID: &id, Host: k.dest.host, Port: k.dest.port, Counters: f.runHost[k].wire(f.security)}
		}
		out[run] = edges
	}
	return out
}

// items is the snapshot's run items in run order. Every caller gets the run-item list and its counters; a
// security caller also gets restriction_ids and the hosting runner's owner_kind (egress and attention are set
// per page).
func (f *wallsTopoFold) items(ownerKind map[uuid.UUID]types.RunnerOwnerKind) []WallsRunItem {
	out := make([]WallsRunItem, len(f.runs))
	for i, run := range f.runs {
		c := f.run[run.ID]
		if c == nil {
			c = &wallsTopoCount{}
		}
		item := WallsRunItem{ID: run.ID, UnitID: run.UnitID, State: run.State, ConfinementClass: run.ConfinementClass,
			Placement: run.Placement, RunnerID: run.RunnerID, RunnerPoolID: run.RunnerPoolID, MachineKind: run.MachineKind,
			CreatedBy: run.CreatedBy, CreatedAt: run.CreatedAt, Counters: c.wire(f.security)}
		if f.security {
			item.RestrictionIDs = run.RestrictionIDs
			if run.RunnerID != nil {
				item.OwnerKind = ownerKind[*run.RunnerID]
			}
		}
		out[i] = item
	}
	return out
}

// unitRuns is each unit's runs in the snapshot: live (not terminal), held (with a held row in the window) and
// the count in each state.
func (f *wallsTopoFold) unitRuns() map[uuid.UUID]*WallsUnitRuns {
	out := map[uuid.UUID]*WallsUnitRuns{}
	for _, run := range f.runs {
		unit := wallsTopoUnitOf(&run)
		u := out[unit]
		if u == nil {
			u = &WallsUnitRuns{ByState: map[types.RunState]int{}}
			out[unit] = u
		}
		if slices.Contains(types.NonTerminalRunStates, run.State) {
			u.Live++
		}
		if c := f.run[run.ID]; c != nil && c.held > 0 {
			u.Held++
		}
		u.ByState[run.State]++
	}
	return out
}

// wallsTopoEgressMode is a unit's egress mode from its composed ceiling, and, when it did not compose, unknown
// with why.
func wallsTopoEgressMode(u unitResolution) (mode, why string) {
	switch {
	case u.Err != nil:
		return wallsTopoModeUnknown, adminResolveError(u.Err)
	case u.Resolved == nil:
		return wallsTopoModeUnknown, "the unit's walls cannot be composed"
	case u.Resolved.Ceiling.AllowAllEgress:
		return wallsTopoModeAllowAll, ""
	case len(u.Resolved.Ceiling.AllowedDomains) == 0:
		return wallsTopoModeSealed, ""
	}
	return wallsTopoModeAllowlist, ""
}

// wallsTopoUnits is units[]. A security caller gets every unit, with its error when it did not compose. A unit
// admin gets the units of their subtrees, and each unit above a subtree root as its id, name, parent, depth and
// an unknown egress mode, nothing more (the names GET /governance already gives them).
func (s *Server) wallsTopoUnits(w http.ResponseWriter, r *http.Request, sc wallsTopoScope, fold *wallsTopoFold) ([]WallsUnit, bool) {
	ctx := r.Context()
	resolved, err := s.resolveAllUnits(ctx)
	if err != nil {
		writeServerError(w, r, "list governance profiles", err)
		return nil, false
	}
	runs := fold.unitRuns()
	var subjects map[uuid.UUID][]WallsSubject
	if sc.security {
		subjects = s.wallsTopoSubjects(ctx)
	}
	out := []WallsUnit{}
	for _, u := range resolved {
		if !sc.security && !sc.in[u.Row.ID] {
			continue
		}
		mode, why := wallsTopoEgressMode(u)
		wu := WallsUnit{ID: u.Row.ID, Name: u.Row.Name, ParentID: u.Row.ParentID, Kind: u.Row.Kind, Mode: u.Row.Mode,
			Depth: u.Row.Depth, EgressMode: mode, Runs: wallsTopoRunsOf(runs, u.Row.ID)}
		c := fold.unit[u.Row.ID]
		if c == nil {
			c = &wallsTopoCount{}
		}
		wu.Egress = c.wire(sc.security)
		if sc.security {
			wu.Error, wu.Subjects = why, subjects[u.Row.ID]
		}
		out = append(out, wu)
	}
	if sc.security {
		return out, true
	}
	ancestors, err := s.wallsTopoAncestors(ctx, resolved, sc)
	if err != nil {
		writeServerError(w, r, "read unit admin grants", err)
		return nil, false
	}
	return append(out, ancestors...), true
}

// wallsTopoSubjects is, per unit, the subjects its memberships bind, by type and then subject; nil, logged, when
// the memberships cannot be read.
func (s *Server) wallsTopoSubjects(ctx context.Context) map[uuid.UUID][]WallsSubject {
	ms, err := s.cfg.Store.ListGovernanceMemberships(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardyn: the Walls view could not list unit memberships; subjects are left out", slog.Any("err", err))
		return nil
	}
	out := map[uuid.UUID][]WallsSubject{}
	for _, m := range ms {
		out[m.UnitID] = append(out[m.UnitID], WallsSubject{SubjectType: m.SubjectType, Subject: m.Subject})
	}
	for _, list := range out {
		slices.SortFunc(list, func(a, b WallsSubject) int {
			if c := strings.Compare(string(a.SubjectType), string(b.SubjectType)); c != 0 {
				return c
			}
			return strings.Compare(a.Subject, b.Subject)
		})
	}
	return out
}

func wallsTopoRunsOf(runs map[uuid.UUID]*WallsUnitRuns, unit uuid.UUID) *WallsUnitRuns {
	if u := runs[unit]; u != nil {
		return u
	}
	return &WallsUnitRuns{ByState: map[types.RunState]int{}}
}

// wallsTopoAncestors is each unit above a unit admin's subtree roots and outside their units, as
// scopeGovernanceRead names them (ancestorNames), in the resolved list's order.
func (s *Server) wallsTopoAncestors(ctx context.Context, resolved []unitResolution, sc wallsTopoScope) ([]WallsUnit, error) {
	grants, err := s.callerUnitGrants(ctx)
	if err != nil {
		return nil, err
	}
	limit, err := s.unitDepthLimit(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]types.GovernanceUnit, len(resolved))
	for i, u := range resolved {
		rows[i] = u.Row
	}
	_, roots := subtreeUnits(rows, grants, limit)
	names := ancestorNames(rows, roots, sc.in, limit)
	out := []WallsUnit{}
	for _, row := range rows {
		if name, ok := names[row.ID]; ok {
			out = append(out, WallsUnit{ID: row.ID, Name: name, ParentID: row.ParentID, Depth: row.Depth, EgressMode: wallsTopoModeUnknown})
		}
	}
	return out, nil
}

// wallsTopoElsewhere is a unit admin's totals for the runs outside their units with a row in the last 15
// minutes, from a count-only read: no row, host or run id of those runs is read. It is nil, logged, when the
// store cannot count or the read fails.
func (s *Server) wallsTopoElsewhere(ctx context.Context, sc wallsTopoScope, asOf time.Time, caps wallsCaps) *WallsElsewhere {
	counter, ok := s.cfg.Store.(store.WallsAuditCounter)
	if !ok {
		slog.WarnContext(ctx, "wardyn: the Walls view's store cannot count rows; elsewhere is left out")
		return nil
	}
	counts, err := counter.AuditWallsCounts(ctx, wallsTopoActions, asOf.Add(-wallsTopoElsewhereWindow), slices.Clone(sc.units), caps.countRows)
	if err != nil {
		slog.WarnContext(ctx, "wardyn: the Walls view could not count the rows outside the caller's units; elsewhere is left out",
			slog.Any("err", err))
		return nil
	}
	out := &WallsElsewhere{}
	for _, c := range counts {
		if c.InScope {
			continue
		}
		switch c.Action {
		case "":
			out.Runs, out.Live = c.Runs, c.Live
		case "egress.hold":
			out.Held = c.Runs
		case "egress.deny":
			out.NotAllowed = c.Rows
		case "egress.allow":
			out.Allowed = c.Rows
		}
	}
	return out
}
