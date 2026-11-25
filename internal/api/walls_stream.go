// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Walls view's two live streams: GET /api/v1/walls/stream (the organisation, or the caller's units) and
// GET /api/v1/runs/{id}/walls/stream (one run). Each sends one frame event a second, even when nothing happened,
// and a notice event when an approval, a run's policy or a credential changes. The rows come from the replica's one
// sampler (walls_sampler.go) and are projected here for each caller, so a caller sees only what GET /audit would
// show them. The routes keep the gates API-1's mount lines give them; these handlers add the limits, the
// per-request authority checks and the projection.

const (
	wallsOpensPerMin   = 12 // stream opens per principal per replica, both streams together
	wallsOpenBurst     = 6
	wallsOrgStreamsMax = 2 // organisation streams per principal per replica
	wallsFrameRuns     = 500
	wallsFrameHosts    = 8
	wallsFrameSamples  = 5
	wallsBeatNotices   = 32 // notices one stream is sent per beat
	wallsFrameWindow   = 1000
)

// wallsLiveState is the live streams' state on one replica: the open limiter, the count of organisation streams
// each principal holds, and the sampler. It lives in wallsState (walls_routes.go).
type wallsLiveState struct {
	once        sync.Once
	openLimiter principalLimiter
	mu          sync.Mutex
	orgStreams  map[runEventStreamer]int
	sampler     wallsSampler
}

// wallsViewKind is how much of a beat a caller sees.
type wallsViewKind uint8

const (
	wallsViewFull wallsViewKind = iota // the security tier and a run's owner: weighted counts and every sample field
	wallsViewUnit                      // a unit admin: one count per row, as GET /audit lists them, and the unit view's fields
)

// wallsView is one stream's caller, as the projection reads them.
type wallsView struct {
	Kind  wallsViewKind
	Run   uuid.UUID          // the run of a run's stream; zero on the organisation's
	Units map[uuid.UUID]bool // wallsViewUnit only: the caller's units now
}

// sees is whether a run frozen to unit is in the view. A run with no unit is in no unit admin's view.
func (v wallsView) sees(run uuid.UUID, unit *uuid.UUID) bool {
	if v.Run != uuid.Nil && run != v.Run {
		return false
	}
	return v.Kind == wallsViewFull || (unit != nil && v.Units[*unit])
}

// The wire types.

// WallsFrame is one beat for one caller.
type WallsFrame struct {
	T        time.Time           `json:"t"`
	WindowMS int                 `json:"window_ms"`
	Stale    bool                `json:"stale,omitempty"` // the beat's reads failed: nothing in it is known
	Runs     []WallsFrameRun     `json:"runs"`
	Overflow *WallsFrameOverflow `json:"overflow,omitempty"`
	Fidelity *WallsFrameFidelity `json:"fidelity,omitempty"` // full view only
}

// WallsFrameRun is one run's traffic in one beat. Counters is *WallsCounters for the full view and
// *WallsUnitCounters for a unit admin's.
type WallsFrameRun struct {
	RunID    uuid.UUID          `json:"run_id"`
	Counters any                `json:"counters"`
	Hosts    []WallsFrameHost   `json:"hosts"`
	Samples  []WallsFrameSample `json:"samples"`
}

// WallsFrameHost is one destination of a run in one beat.
type WallsFrameHost struct {
	Host  string `json:"host"`
	Port  int    `json:"port,omitempty"`
	Count int64  `json:"count"`
}

// WallsFrameSample is one decision of a run in one beat.
type WallsFrameSample struct {
	ID            uuid.UUID `json:"id"`
	Class         string    `json:"class"`
	Host          string    `json:"host,omitempty"`
	Port          int       `json:"port,omitempty"`
	RefusalReason string    `json:"refusal_reason,omitempty"` // full view only
	PolicyContact string    `json:"policy_contact,omitempty"` // full view only
	MatchedRule   string    `json:"matched_rule,omitempty"`   // full view only
	ApprovalID    string    `json:"approval_id,omitempty"`
}

// WallsFrameOverflow is the runs past the frame's cap, counted together.
type WallsFrameOverflow struct {
	Runs     int `json:"runs"`
	Counters any `json:"counters"`
}

// WallsFrameFidelity is how many decisions the beat's runs lost before they were recorded.
type WallsFrameFidelity struct {
	NotRecorded int64 `json:"not_recorded"`
}

// WallsNotice says an approval, a run's policy or a credential changed. It carries no id, so a reconnect never
// resumes on one.
type WallsNotice struct {
	Action     string    `json:"action"`
	RunID      uuid.UUID `json:"run_id"`
	ApprovalID string    `json:"approval_id,omitempty"`
}

// wallsOpenLimiter is the limiter on stream opens, with its rate, burst and table size set once.
func (s *Server) wallsOpenLimiter() *principalLimiter {
	s.wallsLive.once.Do(func() {
		l := &s.wallsLive.openLimiter
		l.rate, l.burst, l.max = wallsOpensPerMin/60.0, wallsOpenBurst, preflightLimiterMaxPeople
	})
	return &s.wallsLive.openLimiter
}

// wallsFreshScope is r with a new unit-scope memo. requireUnitScope's memo answers every grant read from what held
// when the stream opened, so a read on the original request would never see a grant revoked since.
func wallsFreshScope(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), unitScopeMemoKey{}, &unitScopeMemo{
		in: map[unitScopeKey]bool{}, grants: map[string][]types.GovernanceUnitAdmin{},
	}))
}

// wallsReadable is whether the store can serve a live stream: decision rows and runs by id.
func (s *Server) wallsReadable() bool {
	_, rows := s.cfg.Store.(store.WallsAuditReader)
	_, runs := s.cfg.Store.(store.RunSweepReader)
	return rows && runs
}

// wallsUnavailable answers 501 for a store that cannot serve a live stream.
func wallsUnavailable(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusNotImplemented, reasonWallsViewUnavailable, "this Walls view route is not available on this server yet")
}

// handleRunWallsStream serves GET /api/v1/runs/{id}/walls/stream: a run's owner and the security tier get the full
// view, a unit admin of the run's unit gets the unit view of that one run.
func (s *Server) handleRunWallsStream(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.wallsRunReadable)
	if !ok {
		return
	}
	if !s.wallsReadable() {
		wallsUnavailable(w)
		return
	}
	view := wallsView{Kind: wallsViewFull, Run: id}
	var refresh func(*http.Request) (wallsView, bool)
	if !s.ownsRunOrAdmin(r, run) {
		// The gate let a unit admin of the run's unit in, so the run has a unit.
		view = wallsView{Kind: wallsViewUnit, Run: id, Units: map[uuid.UUID]bool{*run.UnitID: true}}
		refresh = func(fresh *http.Request) (wallsView, bool) { return view, s.wallsUnitRunScope(fresh, run) }
	}
	if s.refuseWallsRate(w, r, s.wallsOpenLimiter(), wallsOpensPerMin) {
		return
	}
	actor, principal := actorFromRequest(r)
	release, ok := s.runEvents.openStream(runEventStreamer{actor, principal})
	if !ok {
		s.refuse(w, r, authz.Deny(authz.ReasonEventStreamCap, id.String(),
			fmt.Sprintf("too many open event streams (max %d) — close one and retry", maxRunEventStreams)).OnRun(id))
		return
	}
	defer release()
	s.wallsServe(w, r, view, refresh)
}

// handleWallsStream serves GET /api/v1/walls/stream behind requireUnitScope: the security tier gets every run, a
// unit admin the runs of their units.
func (s *Server) handleWallsStream(w http.ResponseWriter, r *http.Request) {
	if !s.wallsReadable() {
		wallsUnavailable(w)
		return
	}
	view := wallsView{Kind: wallsViewFull}
	var refresh func(*http.Request) (wallsView, bool)
	if s.isUnitAdminCaller(r.Context()) {
		units, _ := s.wallsCallerUnits(wallsFreshScope(r))
		view = wallsView{Kind: wallsViewUnit, Units: units}
		refresh = func(fresh *http.Request) (wallsView, bool) {
			units, ok := s.wallsCallerUnits(fresh)
			return wallsView{Kind: wallsViewUnit, Units: units}, ok
		}
	}
	if s.refuseWallsRate(w, r, s.wallsOpenLimiter(), wallsOpensPerMin) {
		return
	}
	actor, principal := actorFromRequest(r)
	who := runEventStreamer{actor, principal}
	release, ok := s.runEvents.openStream(who)
	if !ok {
		s.refuse(w, r, authz.Deny(authz.ReasonEventStreamCap, "walls",
			fmt.Sprintf("too many open event streams (max %d) — close one and retry", maxRunEventStreams)))
		return
	}
	defer release()
	live := &s.wallsLive
	live.mu.Lock()
	if live.orgStreams[who] >= wallsOrgStreamsMax {
		live.mu.Unlock()
		s.refuse(w, r, authz.Deny(authz.ReasonEventStreamCap, "walls",
			fmt.Sprintf("too many open Walls view organisation streams (max %d) — close one and retry", wallsOrgStreamsMax)))
		return
	}
	if live.orgStreams == nil {
		live.orgStreams = map[runEventStreamer]int{}
	}
	live.orgStreams[who]++
	live.mu.Unlock()
	defer func() {
		live.mu.Lock()
		defer live.mu.Unlock()
		if live.orgStreams[who]--; live.orgStreams[who] <= 0 {
			delete(live.orgStreams, who)
		}
	}()
	s.wallsServe(w, r, view, refresh)
}

// wallsCallerUnits is the caller's units now, read through fresh. ok is false when there is no scope: no grant, a
// grant read that failed, or no unit in the subtrees. The set is then empty, and a stream that is already open
// ends rather than keep serving what it served.
func (s *Server) wallsCallerUnits(fresh *http.Request) (units map[uuid.UUID]bool, ok bool) {
	scope := s.unitAuditScopeFor(fresh, false, types.AgentRun{})
	units = map[uuid.UUID]bool{}
	if scope == nil {
		return units, false
	}
	for _, u := range scope.units {
		units[u] = true
	}
	return units, true
}

// wallsServe holds one stream open: it writes the beats the caller has not been sent, then waits for the next, the
// refresh, the hold, the client or the daemon. refresh re-reads a unit admin's scope on a request with a fresh
// memo and reports whether the stream goes on; a full view is never re-decided, as a run's event stream is not.
func (s *Server) wallsServe(w http.ResponseWriter, r *http.Request, view wallsView, refresh func(*http.Request) (wallsView, bool)) {
	_, principal := actorFromRequest(r)
	sampler := &s.wallsLive.sampler
	release := sampler.subscribe(s)
	defer release()
	nonce := sampler.nonce()
	cursor, gap := sampler.headN(), false
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		n, ok := strings.CutPrefix(id, nonce+".")
		v, err := strconv.ParseUint(n, 10, 64)
		cursor, gap = v, !ok || err != nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	openedAt := s.cfg.Now().UTC()
	daemon := s.cfg.BaseCtx
	if daemon == nil {
		daemon = r.Context()
	}
	hold := time.NewTimer(cmp.Or(s.runEvents.hold, runEventsHold))
	defer hold.Stop()
	tick := time.NewTicker(cmp.Or(s.runEvents.beat, runEventsBeat))
	defer tick.Stop()
	for {
		beats, head, wake, behind := sampler.since(cursor)
		_ = rc.SetWriteDeadline(time.Now().Add(cmp.Or(s.runEvents.write, runEventsWrite)))
		if gap || behind {
			if _, err := fmt.Fprintf(w, "id: %s.%d\nevent: gap\ndata: {}\n\n", nonce, head); err != nil {
				return
			}
			cursor, gap = head, false
		} else {
			for _, b := range beats {
				if _, err := w.Write(wallsEvents(nonce, b, view)); err != nil {
					return
				}
				cursor = b.N
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
		_ = rc.SetWriteDeadline(time.Time{})
		select {
		case <-wake:
		case <-tick.C:
			if s.sessionRevokedSince(r, principal, openedAt) || s.delegationEndedSince(r) {
				return
			}
			if refresh != nil {
				next, ok := refresh(wallsFreshScope(r))
				if !ok {
					return
				}
				view = next
			}
		case <-hold.C:
			return
		case <-r.Context().Done():
			return
		case <-daemon.Done():
			return
		}
	}
}

// wallsEvents is the bytes one beat writes to one stream: its frame, then each notice. Only the frame has an id.
func wallsEvents(nonce string, b wallsBeat, v wallsView) []byte {
	var out bytes.Buffer
	if frame, err := json.Marshal(wallsProject(b, v)); err == nil {
		fmt.Fprintf(&out, "id: %s.%d\nevent: frame\ndata: %s\n\n", nonce, b.N, frame)
	}
	for _, n := range wallsNotices(b, v) {
		if data, err := json.Marshal(n); err == nil {
			fmt.Fprintf(&out, "event: notice\ndata: %s\n\n", data)
		}
	}
	return out.Bytes()
}

// wallsNotices is the beat's notices whose run is in view, in beat order, at most wallsBeatNotices. A unit admin
// reads a notice's action, run and approval id on GET /audit already, and the approval id only in the shape the unit
// view serves it.
func wallsNotices(b wallsBeat, v wallsView) []WallsNotice {
	out := []WallsNotice{}
	for _, n := range b.Notices {
		if len(out) == wallsBeatNotices {
			break
		}
		if !v.sees(n.RunID, n.Unit) {
			continue
		}
		approval := n.ApprovalID
		if v.Kind == wallsViewUnit {
			approval = ""
			if kept := wallsUnitData(map[string]any{"approval_id": n.ApprovalID}); kept != nil {
				_ = json.Unmarshal(kept["approval_id"], &approval)
			}
		}
		out = append(out, WallsNotice{Action: n.Action, RunID: n.RunID, ApprovalID: approval})
	}
	return out
}

// wallsUnitData is data as the unit view serves it (unitViewData): the keys it keeps, with the values it keeps.
func wallsUnitData(data map[string]any) map[string]json.RawMessage {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	var kept map[string]json.RawMessage
	if json.Unmarshal(unitViewData(raw), &kept) != nil {
		return nil
	}
	return kept
}

// wallsProjected is one run in view, before the frame's caps.
type wallsProjected struct {
	id    uuid.UUID
	fold  *wallsRunFold
	total int64
}

// wallsProject is the frame b makes for v: the runs in view, busiest first, capped at wallsFrameRuns with the rest
// counted together in overflow. The caps apply after the view, so a run outside it can never push a run inside it
// into overflow.
func wallsProject(b wallsBeat, v wallsView) WallsFrame {
	frame := WallsFrame{T: b.T, WindowMS: wallsFrameWindow, Stale: b.Stale, Runs: []WallsFrameRun{}}
	var in []wallsProjected
	for id, f := range b.Folds {
		if !v.sees(id, f.Meta.UnitID) {
			continue
		}
		total := f.Counters.total()
		if v.Kind == wallsViewUnit {
			total = f.Unit.total()
		}
		in = append(in, wallsProjected{id, f, total})
	}
	slices.SortFunc(in, func(a, b wallsProjected) int {
		return cmp.Or(cmp.Compare(b.total, a.total), bytes.Compare(a.id[:], b.id[:]))
	})
	var fidelity int64
	var full WallsCounters
	var unit WallsUnitCounters
	for i, p := range in {
		fidelity += p.fold.Counters.Fidelity
		if i < wallsFrameRuns {
			frame.Runs = append(frame.Runs, wallsProjectRun(p, v))
			continue
		}
		if v.Kind == wallsViewUnit {
			unit.sum(p.fold.Unit)
		} else {
			full.sum(p.fold.Counters)
		}
	}
	if rest := len(in) - wallsFrameRuns; rest > 0 {
		frame.Overflow = &WallsFrameOverflow{Runs: rest}
		if v.Kind == wallsViewUnit {
			frame.Overflow.Counters = &unit
		} else {
			frame.Overflow.Counters = &full
		}
	}
	if v.Kind == wallsViewFull && fidelity > 0 {
		frame.Fidelity = &WallsFrameFidelity{NotRecorded: fidelity}
	}
	return frame
}

// wallsProjectRun is one run's entry in the frame.
func wallsProjectRun(p wallsProjected, v wallsView) WallsFrameRun {
	run := WallsFrameRun{RunID: p.id, Hosts: []WallsFrameHost{}, Samples: []WallsFrameSample{}}
	hosts := slices.Clone(p.fold.Hosts)
	if v.Kind == wallsViewUnit {
		c := p.fold.Unit
		run.Counters = &c
		slices.SortFunc(hosts, func(a, b wallsHostFold) int {
			return cmp.Or(cmp.Compare(b.Rows, a.Rows), cmp.Compare(a.Host, b.Host), cmp.Compare(a.Port, b.Port))
		})
		for _, h := range hosts {
			if len(run.Hosts) == wallsFrameHosts {
				break
			}
			if _, shown := wallsUnitData(map[string]any{"host": h.Host, "port": h.Port})["host"]; shown {
				run.Hosts = append(run.Hosts, WallsFrameHost{Host: h.Host, Port: h.Port, Count: h.Rows})
			}
		}
		for _, s := range p.fold.Samples {
			if len(run.Samples) == wallsFrameSamples {
				break
			}
			run.Samples = append(run.Samples, wallsUnitSample(s))
		}
		return run
	}
	c := p.fold.Counters
	run.Counters = &c
	slices.SortFunc(hosts, func(a, b wallsHostFold) int {
		return cmp.Or(cmp.Compare(b.Weight, a.Weight), cmp.Compare(a.Host, b.Host), cmp.Compare(a.Port, b.Port))
	})
	for _, h := range hosts[:min(len(hosts), wallsFrameHosts)] {
		run.Hosts = append(run.Hosts, WallsFrameHost{Host: h.Host, Port: h.Port, Count: h.Weight})
	}
	// Refused first, then the rest, each in seq order. A late row is no sample.
	for _, refused := range []bool{true, false} {
		for _, s := range p.fold.Samples {
			if len(run.Samples) == wallsFrameSamples {
				break
			}
			if s.Late || (s.Class == wallsRefused) != refused {
				continue
			}
			run.Samples = append(run.Samples, WallsFrameSample{ID: s.ID, Class: string(s.Class), Host: s.Host, Port: s.Port,
				RefusalReason: s.Reason, PolicyContact: s.PolicyContact, MatchedRule: s.MatchedRule, ApprovalID: s.ApprovalID})
		}
	}
	return run
}

// wallsUnitSample is s as a unit admin is shown it: its id and class, and the host, port and approval id the unit
// view keeps. Never a refusal reason, a policy contact or a matched rule.
func wallsUnitSample(s wallsSample) WallsFrameSample {
	out := WallsFrameSample{ID: s.ID, Class: string(s.Class)}
	kept := wallsUnitData(map[string]any{"host": s.Host, "port": s.Port, "approval_id": s.ApprovalID})
	_ = json.Unmarshal(kept["host"], &out.Host)
	_ = json.Unmarshal(kept["port"], &out.Port)
	_ = json.Unmarshal(kept["approval_id"], &out.ApprovalID)
	return out
}

// total is the decisions a run's full counters hold; Guest is part of Refused.
func (c WallsCounters) total() int64 {
	return c.Allowed + c.Refused + c.Held + c.Fault + c.Fidelity + c.ToolRule + c.Unclassified + c.Control + c.Config + c.Late
}

// total is the rows a run's unit counters hold; Guest is part of Refused.
func (c WallsUnitCounters) total() int64 {
	return c.Allowed + c.Refused + c.Held + c.Fault + c.Fidelity + c.Unclassified
}

// sum adds o to c.
func (c *WallsCounters) sum(o WallsCounters) {
	c.Allowed += o.Allowed
	c.Refused += o.Refused
	c.Guest += o.Guest
	c.Held += o.Held
	c.Fault += o.Fault
	c.Fidelity += o.Fidelity
	c.ToolRule += o.ToolRule
	c.Unclassified += o.Unclassified
	c.Control += o.Control
	c.Config += o.Config
	c.Late += o.Late
}

// sum adds o to c.
func (c *WallsUnitCounters) sum(o WallsUnitCounters) {
	c.Allowed += o.Allowed
	c.Refused += o.Refused
	c.Guest += o.Guest
	c.Held += o.Held
	c.Fault += o.Fault
	c.Fidelity += o.Fidelity
	c.Unclassified += o.Unclassified
}
