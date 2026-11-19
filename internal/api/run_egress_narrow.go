// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A live egress edit on a Kubernetes run does not replace the run's proxy, which the agent pod pins by
// address. It adds the edit's hosts to the running proxy's deny list (runner.ProxyNarrower) and records
// that as a policy generation and a proxy start, confirmed by the same readback wait as a Docker
// edit's. Every outcome is one of three: nothing changed (a 4xx, or 502 kept current), the edit
// confirmed (200), or the start failed and was contained, which on Kubernetes ends the run.
//
// The stored config is written, as the edit's generation, before the proxy is told: the proxy's list
// is always a subset of the stored one. The list only grows, so a late or repeated delivery never
// widens anything. The edit's hosts are refused from the moment the proxy swaps its list.

const (
	// narrowRouteTimeout bounds the probe that tells a Kubernetes run from a Docker one.
	narrowRouteTimeout = 5 * time.Second
	// narrowStepsTimeout bounds the reads, the advance and the begin that run under the run's lock.
	narrowStepsTimeout = 45 * time.Second
	// narrowDeliverTimeout bounds the delivery. It is not cancelled by a lost lock: a delivered list is
	// always a subset of the stored one, and cancelling would only turn a likely success into a cut run.
	narrowDeliverTimeout = 15 * time.Second
	// narrowRunnerRouteTimeout and narrowRunnerDeliverTimeout are a runner's bounds: a wire round trip and the runner's
	// pod reads on top of its own probe (5 s) and its probe-then-apply (5 s + 15 s). The probe's 8 s is also the most
	// a revive after it can take inside the edit's call budget (TestEgressEditBudget_CoversEveryWindowAfterTheClaim).
	narrowRunnerRouteTimeout   = 8 * time.Second
	narrowRunnerDeliverTimeout = 25 * time.Second
)

const (
	narrowOldPodMsg    = "This run's proxy was started before 0.10.0-rc.20 or runs an older image, so its network can't be narrowed while it runs. Start a new run with the narrower policy."
	narrowRetryMsg     = "The run's proxy is not taking live edits right now (it may be restarting), so nothing changed. Try again in a few seconds."
	narrowSupersedeMsg = "Another edit of this run came after this one and carries its hosts; that edit's own readback confirms them."
	// narrowRunnerOldPodMsg is a runner's Unsupported: its proxy pod lacks the live deny list or its image the
	// narrowing, the pod is not running, or the runner has no matching record of the base it started it with.
	narrowRunnerOldPodMsg = "This run's proxy can't be narrowed while it runs, so nothing changed: a runner older than 0.10.1 or an older image started it, it is not running, or its runner cannot prove it still runs the config it was started with. Start a new run with the narrower policy, and update the runner first if it is older than 0.10.1."
	// narrowRunnerAwayMsg names the runner of an edit nothing reached because it is offline, or has not reported
	// its capabilities since wardynd started, so whether it narrows in place is not known yet.
	narrowRunnerAwayMsg = "The runner on %s is not connected or has not reported what it supports since Wardyn restarted, so nothing changed. Try again in a few seconds."
	// narrowRunnerPodMsg names a connected runner whose own read of the run's proxy pod failed before any exec.
	narrowRunnerPodMsg = "The runner on %s could not reach this run's proxy pod; try again, and if it keeps failing, check that the pod is running."
)

// egressEditInPlace marks an edit that narrowed the proxy in place instead of replacing it.
type egressEditInPlace struct{ proxyWasBehind bool }

// narrowEdit is one in-place edit while it holds the run's lock.
type narrowEdit struct {
	s      *Server
	n      runner.ProxyNarrower
	run    types.AgentRun
	stored []byte // the run's stored config before the edit: what an undo puts back
	denies []string
	want   string // the hash of the config the proxy will enforce once it holds denies; a runner's is its answer's
	gen    int
	start  proxyStartRec
	// onRunner: the run is on a runner, which proves the proxy's base itself (BaseChecked) and answers the
	// hash of the bytes its proxy enforces, since the org never holds those bytes.
	onRunner bool
}

// narrowRunEgress applies edit by narrowing run's proxy in place. It returns a nil egressEditInPlace
// exactly when the revive path should run instead: a substrate that does not narrow in place,
// including a runner whose caps lack ProxyNarrow.
func (s *Server) narrowRunEgress(r *http.Request, run types.AgentRun, edit liveEgressEdit) (reviveResult, *reviveError, *egressEditInPlace) {
	n, ok := s.cfg.Runner.(runner.ProxyNarrower)
	if !ok {
		return reviveResult{}, nil, nil
	}
	ctx := r.Context()
	actorType, actor := actorFromRequest(r)
	inPlace := &egressEditInPlace{}
	// Local mode mints for the host operator, not for the run's owner, so only the principal that
	// created the run may edit it. This comes before any exec.
	if localPrincipalFromContext(ctx) != "" && actor != run.CreatedBy {
		s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(),
			"denied", mustJSON(map[string]any{"subject": run.CreatedBy, "reason": reasonReviveLocalModeNotOwner})))
		return reviveResult{}, reviveRefused(http.StatusForbidden, reasonReviveLocalModeNotOwner, "in local mode only the run's owner can revive it"), inPlace
	}
	pctx, cancel := context.WithTimeout(ctx, narrowBound(run, narrowRouteTimeout, narrowRunnerRouteTimeout))
	_, err := n.ProxyNarrowState(pctx, run.SandboxRef)
	cancel()
	if errors.Is(err, runner.ErrNarrowNotOffered) {
		return reviveResult{}, nil, nil
	}
	if err != nil {
		return reviveResult{}, s.narrowProbeRefusal(ctx, run, err), inPlace
	}
	if rerr := narrowEntryLengths(edit); rerr != nil {
		return reviveResult{}, rerr, inPlace
	}

	lctx, unlock, err := s.lockRunOp(ctx, run.ID)
	if err != nil {
		return reviveResult{}, reviveRefused(http.StatusServiceUnavailable, reasonLockUnavailable, lockUnavailableMsg), inPlace
	}
	defer unlock()
	sctx, cancelSteps := context.WithTimeout(lctx, narrowStepsTimeout)
	defer cancelSteps()

	rd, rerr := s.readNarrowSnapshot(sctx, n, run)
	if rerr != nil {
		return reviveResult{}, rerr, inPlace
	}
	behind, rerr := narrowStateRow(ctx, run, rd)
	if rerr != nil {
		return reviveResult{}, rerr, inPlace
	}
	inPlace.proxyWasBehind = behind
	denies, rerr := narrowDenies(rd.cfg, edit)
	if rerr != nil {
		return reviveResult{}, rerr, inPlace
	}
	policy, rerr := narrowOracle(ctx, run, rd.stored, rd.cfg, edit)
	if rerr != nil {
		return reviveResult{}, rerr, inPlace
	}
	// The stored config is the proxy's copy, which never carries data_out or recording: the generation
	// takes the run's recorded rules, so an edit after a restart that raised them keeps them.
	if rerr := s.reviveCarryRecordedRules(sctx, run.ID, &policy); rerr != nil {
		return reviveResult{}, rerr, inPlace
	}
	next, err := proxy.WithLiveDenies(rd.stored, denies)
	if err != nil {
		return reviveResult{}, reviveRefused(http.StatusBadGateway, reasonReviveProxyKeptCurrent,
			loggedMsg(ctx, "the edit could not be added to the run's stored config, so nothing changed", err)), inPlace
	}
	ne := &narrowEdit{s: s, n: n, run: run, stored: rd.stored, denies: denies, onRunner: run.RunnerID != nil}
	if !ne.onRunner {
		ne.want = proxy.ConfigSHA256(next)
	}
	res, rerr := ne.write(ctx, sctx, policy, next, rd)
	if rerr != nil {
		return res, rerr, inPlace
	}
	res, rerr = ne.deliver(ctx)
	return res, rerr, inPlace
}

// narrowBound is onRunner for a run on a runner, and hosted for any other.
func narrowBound(run types.AgentRun, hosted, onRunner time.Duration) time.Duration {
	if run.RunnerID != nil {
		return onRunner
	}
	return hosted
}

// narrowRunnerName names the runner's computer from its last caps, else its row, as egressEditRunnerToUpdate
// does: the caps are held in memory only, so after a restart the row may be all there is.
func (s *Server) narrowRunnerName(ctx context.Context, id uuid.UUID) string {
	if caps, ok := s.runnerLastCaps(id); ok && caps.MachineName != "" {
		return caps.MachineName
	}
	if rs, ok := s.cfg.Store.(runnerGetter); ok {
		if row, err := rs.GetRunner(ctx, id); err == nil && row.MachineName != "" {
			return row.MachineName
		}
	}
	return "its computer"
}

// narrowRunnerNotReached is the answer text for a runner run's edit that reached nothing, by cause: the runner
// away (offline, or its caps not known since this server started), or a connected runner that could not
// reach the proxy pod (its namespace lookup, pod read or executor build failed).
func (s *Server) narrowRunnerNotReached(ctx context.Context, run types.AgentRun, err error) string {
	id := *run.RunnerID
	msg := narrowRunnerPodMsg
	if _, known := s.runnerLastCaps(id); !known || errors.Is(err, runner.ErrRunnerOffline) {
		msg = narrowRunnerAwayMsg
	}
	return fmt.Sprintf(msg, s.narrowRunnerName(ctx, id))
}

// narrowProbeRefusal is the answer for a probe or locked read of the proxy that failed.
func (s *Server) narrowProbeRefusal(ctx context.Context, run types.AgentRun, err error) *reviveError {
	onRunner := run.RunnerID != nil
	switch {
	case errors.Is(err, runner.ErrNarrowUnsupported) && onRunner:
		return reviveRefused(http.StatusConflict, reasonEgressEditUnsupported, narrowRunnerOldPodMsg)
	case errors.Is(err, runner.ErrNarrowUnsupported):
		return reviveRefused(http.StatusConflict, reasonEgressEditUnsupported, narrowOldPodMsg)
	case errors.Is(err, runner.ErrNarrowNotListening):
		return reviveRefused(http.StatusBadGateway, reasonReviveProxyKeptCurrent, narrowRetryMsg)
	case onRunner && (errors.Is(err, runner.ErrNarrowNotSent) || errors.Is(err, runner.ErrRunnerOffline)):
		slog.InfoContext(ctx, "api: a runner run's proxy was not reached for an egress edit", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return reviveRefused(http.StatusBadGateway, reasonReviveProxyKeptCurrent, s.narrowRunnerNotReached(ctx, run, err))
	case errors.Is(err, runner.ErrNarrowNotSent):
		return reviveRefused(http.StatusBadGateway, reasonReviveProxyKeptCurrent, narrowRetryMsg)
	}
	return reviveRefused(http.StatusBadGateway, reasonReviveProxyKeptCurrent,
		loggedMsg(ctx, "the run's proxy could not be read, so nothing changed", err))
}

// narrowEntryLengths refuses an entry no proxy could match. No real host is that long.
func narrowEntryLengths(edit liveEgressEdit) *reviveError {
	for _, f := range []struct {
		field string
		hosts []string
	}{{"remove_hosts", edit.remove}, {"deny_hosts", edit.deny}} {
		for i, h := range f.hosts {
			if len(h) > proxy.MaxLiveDenyEntryBytes {
				return reviveRefused(http.StatusBadRequest, reasonEgressEditInvalid,
					fmt.Sprintf("%s[%d]: an entry is at most %d characters on Kubernetes", f.field, i, proxy.MaxLiveDenyEntryBytes))
			}
		}
	}
	return nil
}

// narrowSnapshot is what an in-place edit reads under the run's lock. The expected generation comes
// first: the advance's contract is that it is read before the config it stores, so a generation another
// holder advanced in between makes this edit's write miss.
type narrowSnapshot struct {
	expected store.PolicySlot
	stored   []byte
	cfg      *proxy.Config
	ps       types.RunPolicyState
	active   store.ProxyStart
	found    bool
	probe    runner.ProxyNarrowState
}

func (s *Server) readNarrowSnapshot(sctx context.Context, n runner.ProxyNarrower, run types.AgentRun) (narrowSnapshot, *reviveError) {
	var rd narrowSnapshot
	unreadable := func(what string, err error) *reviveError {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveConfigUnreadable, loggedMsg(sctx, what, err))
	}
	gens, hasGens := s.cfg.Store.(store.RunPolicyGenerations)
	starts, hasStarts := s.cfg.Store.(store.RunProxyStarts)
	if !hasGens || !hasStarts {
		return rd, narrowUnsupported()
	}
	var rerr *reviveError
	if rd.expected, rerr = s.reviveExpectedGeneration(sctx, run.ID); rerr != nil {
		return rd, rerr
	}
	var err error
	if rd.stored, err = s.loadRunProxyConfig(sctx, run.ID); err != nil {
		return rd, unreadable("read the run's stored proxy config", err)
	}
	if rd.cfg, err = proxy.LoadConfigBytes(rd.stored); err != nil {
		return rd, reviveRefused(http.StatusConflict, reasonReviveConfigDoesNotLoad, "the run's proxy config does not load: "+err.Error())
	}
	if rd.ps, _, err = gens.RunPolicyState(sctx, run.ID); err != nil {
		return rd, unreadable("read the run's policy generation", err)
	}
	if rd.active, rd.found, err = starts.ActiveProxyStart(sctx, run.ID); err != nil {
		return rd, unreadable("read the run's proxy start", err)
	}
	if rd.probe, err = n.ProxyNarrowState(sctx, run.SandboxRef); err != nil {
		return rd, s.narrowProbeRefusal(sctx, run, err)
	}
	return rd, nil
}

func narrowUnsupported() *reviveError {
	return reviveRefused(http.StatusConflict, reasonEgressEditUnsupported,
		"This deployment cannot confirm the policy a proxy enforces, so a live egress edit is refused and nothing changed. Start a new run with the narrower policy.")
}

// narrowStateRow checks the active start and the proxy's own list against the stored one. It refuses
// what a new edit cannot safely build on, and says whether the proxy is behind the stored list.
//
//	no start, or one in a state no edit starts from   409 egress_edit_unsupported
//	failed                                            409 egress_edit_run_not_live
//	unconfirmed (a legacy mode)                       409 egress_edit_legacy_proxy
//	pending with a deadline                           409 egress_edit_in_progress
//	proxy base differs from the stored config's       409 egress_edit_unsupported
//	proxy denies hosts the store did not record       409 egress_edit_unsupported
//	enforced and behind, or pending with no deadline  go on; the delivery writes the whole list again
//	enforced and equal                                go on
func narrowStateRow(ctx context.Context, run types.AgentRun, rd narrowSnapshot) (behind bool, rerr *reviveError) {
	switch {
	case !rd.found:
		return false, narrowUnsupported()
	case rd.active.State == types.ProxyStartFailed:
		return false, reviveRefused(http.StatusConflict, reasonEgressEditRunNotLive,
			"the run's proxy did not confirm its policy and is being stopped")
	case rd.active.State == types.ProxyStartUnconfirmed:
		return false, reviveRefused(http.StatusConflict, reasonEgressEditLegacyProxy,
			"This run's proxy never confirmed its policy, so an edit could not be confirmed safely and nothing changed. Start a new run with the narrower policy.")
	case rd.active.State == types.ProxyStartPending && rd.active.Deadline != nil:
		return false, reviveRefused(http.StatusConflict, reasonEgressEditInProgress,
			"an egress edit of this run is already in progress; try again when it answers")
	case rd.active.State != types.ProxyStartPending && rd.active.State != types.ProxyStartEnforced:
		return false, narrowUnsupported()
	}
	// A runner's proxy carries keys the org never holds, so only the runner can prove its base: it compares
	// the proxy's answer with the base it recorded at create.
	var baseOK bool
	if run.RunnerID != nil {
		baseOK = rd.probe.BaseChecked
	} else {
		base, err := proxy.LiveDenyBaseSHA256(rd.stored)
		baseOK = err == nil && rd.probe.BaseSHA256 == base
	}
	if !baseOK {
		return false, reviveRefused(http.StatusConflict, reasonEgressEditUnsupported,
			"This run's proxy was not started from the config Wardyn stored for it, so nothing changed. Start a new run with the narrower policy.")
	}
	recorded := rd.cfg.LiveDenies
	if extra := notIn(rd.probe.LiveDenies, recorded); len(extra) > 0 {
		slog.WarnContext(ctx, "api: a run's proxy denies hosts Wardyn did not record",
			slog.String("run_id", run.ID.String()), slog.Any("hosts", extra))
		return false, reviveRefused(http.StatusConflict, reasonEgressEditUnsupported,
			"This run's proxy denies hosts Wardyn did not record ("+listHosts(extra)+"), so nothing changed.")
	}
	behind = len(rd.probe.LiveDenies) < len(recorded)
	if behind {
		slog.WarnContext(ctx, "api: a run's proxy holds fewer live denies than Wardyn recorded; the edit applies them all",
			slog.String("run_id", run.ID.String()), slog.Int("proxy", len(rd.probe.LiveDenies)), slog.Int("recorded", len(recorded)))
	}
	return behind, nil
}

// notIn lists the entries of a that b does not hold.
func notIn(a, b []string) []string {
	var out []string
	for _, h := range a {
		if !slices.Contains(b, h) {
			out = append(out, h)
		}
	}
	return out
}

// listHosts names at most five hosts, so an answer stays short however long the list is.
func listHosts(hosts []string) string {
	const shown = 5
	if len(hosts) <= shown {
		return strings.Join(hosts, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(hosts[:shown], ", "), len(hosts)-shown)
}

// narrowDenies is the list the proxy will hold: the recorded one and the edit's hosts, normalized. An
// edit that takes it past the proxy's bound is refused. The edit's hosts passed the length check and
// ValidDomainEntry, and the recorded ones passed the config load (readNarrowSnapshot), which refuses one
// in a non-standard IP spelling as ValidDomainEntry now does, so the bound is the only error left here.
func narrowDenies(cfg *proxy.Config, edit liveEgressEdit) ([]string, *reviveError) {
	denies, err := proxy.NormalizeLiveDenies(slices.Concat(cfg.LiveDenies, edit.remove, edit.deny))
	if err != nil {
		return nil, reviveRefused(http.StatusConflict, reasonEgressEditUnsupported,
			fmt.Sprintf("This run's proxy holds at most %s live denies, so nothing changed. Start a new run with the narrower policy.", withThousands(proxy.MaxLiveDenies)))
	}
	return denies, nil
}

func withThousands(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%d,%03d", n/1000, n%1000)
	}
	return fmt.Sprint(n)
}

// narrowOracle answers what a Docker edit would: the same refusals, and a refusal wherever a Docker
// edit would change more than the policy's host verdicts (a credential lane, a via_org route, an MCP
// server or an intercepted host the edit would take away), since the running proxy can only deny more
// hosts. It returns the generation's policy. full is the stored config put through the edit's
// re-assertion; model is the same config with only the policy's own change. They differ exactly when
// the re-assertion did more than that. Each is its own decode: the removal edits the allowlist in place.
func narrowOracle(ctx context.Context, run types.AgentRun, stored []byte, cfg *proxy.Config, edit liveEgressEdit) (types.RunPolicySpec, *reviveError) {
	full, ferr := proxy.LoadConfigBytes(stored)
	model, merr := proxy.LoadConfigBytes(stored)
	if err := errors.Join(ferr, merr); err != nil {
		return types.RunPolicySpec{}, reviveRefused(http.StatusServiceUnavailable, reasonReviveConfigUnreadable,
			loggedMsg(ctx, "decode the run's stored proxy config", err))
	}
	for _, c := range []*proxy.Config{full, model} {
		c.LiveDenies = nil
		unionCeilingDenies(&c.Policy, cfg.LiveDenies)
	}
	re, rerr := reassertProxyCeiling(run, full, ownerCeiling{}, nil, edit)
	if rerr != nil {
		return types.RunPolicySpec{}, rerr
	}
	model.Policy.AllowedDomains = slices.DeleteFunc(model.Policy.AllowedDomains, func(d string) bool {
		return slices.Contains(edit.remove, strings.ToLower(strings.TrimSpace(d)))
	})
	unionCeilingDenies(&model.Policy, slices.Concat(edit.remove, edit.deny))

	jf, ferr := json.Marshal(full)
	jm, merr := json.Marshal(model)
	taken := slices.Concat(re.droppedLane, re.droppedInjection, narrowTakenAway(full, model))
	if err := errors.Join(ferr, merr); err != nil {
		return types.RunPolicySpec{}, reviveRefused(http.StatusServiceUnavailable, reasonReviveConfigUnreadable,
			loggedMsg(ctx, "encode the run's stored proxy config", err))
	}
	if len(taken) > 0 || !bytes.Equal(jf, jm) {
		what := "part of the run's network setup"
		if len(taken) > 0 {
			what = listHosts(taken)
		}
		return types.RunPolicySpec{}, reviveRefused(http.StatusConflict, reasonEgressEditUnsupported,
			fmt.Sprintf("On Kubernetes an edit can't take away %s while the run runs, so nothing changed. Leave %s out of the edit, or start a new run with the narrower policy.",
				what, listHosts(slices.Concat(edit.remove, edit.deny))))
	}
	return model.Policy, nil
}

// narrowTakenAway names the via_org routes, MCP servers and intercepted hosts the re-assertion dropped
// that the model keeps.
func narrowTakenAway(full, model *proxy.Config) []string {
	var out []string
	for _, rt := range model.ViaOrgRoutes {
		if !slices.ContainsFunc(full.ViaOrgRoutes, func(f proxy.ViaOrgRoute) bool { return f.Host == rt.Host }) {
			out = append(out, rt.Host)
		}
	}
	for _, sv := range model.MCPServers {
		if !slices.ContainsFunc(full.MCPServers, func(f proxy.MCPServerConfig) bool { return f.Key == sv.Key }) {
			out = append(out, sv.Key)
		}
	}
	for _, h := range model.MITMHosts {
		if !slices.Contains(full.MITMHosts, h) {
			out = append(out, h)
		}
	}
	return out
}

// write stores the edit's config as its own policy generation, then records its pending start. Nothing
// reaches the proxy before this is durable.
func (ne *narrowEdit) write(ctx, sctx context.Context, policy types.RunPolicySpec, next []byte, rd narrowSnapshot) (reviveResult, *reviveError) {
	s := ne.s
	// The marker is never a token id: it only lets the reconcile recognise this write, and no stamp matches it.
	marker := "edit-" + uuid.NewString()
	gen, err := s.advanceRunPolicyGeneration(sctx, ne.run, policy, "", renewedTokens{}, marker, rd.expected, next)
	if errors.Is(err, store.ErrRunPolicyMoved) {
		return reviveResult{}, egressEditSuperseded(false)
	}
	if err != nil {
		g, rerr := s.reconcileLostAdvance(sctx, ne.run, policy, marker, rd.expected, ne.stored, err)
		if errors.Is(rerr, errPolicyNotAbandoned) {
			return narrowLost(g, rerr)
		}
		return reviveResult{}, reviveRefused(http.StatusBadGateway, reasonReviveProxyKeptCurrent,
			loggedMsg(ctx, "the edit's policy could not be recorded, so nothing changed", rerr))
	}
	ne.gen = gen
	jti := firstOr(rd.ps.LiveTokenIDs, rd.active.JTI)
	err = ne.begin(sctx, jti)
	if err == nil {
		err = ne.rebeginOnRenewal(sctx, jti)
	}
	if err != nil {
		return ne.undo(ctx, err, "the edit's start could not be recorded, so nothing changed")
	}
	return reviveResult{}, nil
}

// begin records the edit's pending start on token jti. A start that would not be confirmed (a legacy
// mode) cannot carry an edit.
func (ne *narrowEdit) begin(sctx context.Context, jti string) error {
	start, err := ne.s.beginProxyStart(sctx, ne.run, ne.gen, jti, true)
	if err != nil {
		return err
	}
	ne.start = start
	if start.mode != store.ProxyStartExpected {
		return errors.New("the start would not be confirmed")
	}
	return nil
}

// rebeginOnRenewal records the start again when a renewal landed between the reads and moved the run's
// token: a begin under the same generation resets the start.
func (ne *narrowEdit) rebeginOnRenewal(sctx context.Context, jti string) error {
	gens, ok := ne.s.cfg.Store.(store.RunPolicyGenerations)
	if !ok {
		return nil
	}
	ps, _, err := gens.RunPolicyState(sctx, ne.run.ID)
	if err != nil {
		return err
	}
	if now := firstOr(ps.LiveTokenIDs, jti); now != jti {
		return ne.begin(sctx, now)
	}
	return nil
}

func firstOr(ids []string, fallback string) string {
	if len(ids) > 0 {
		return ids[0]
	}
	return fallback
}

// narrowLost is the result of a write the run cannot take back: a generation that stays current, or a
// start that failed. The edit's wait contains it, which on Kubernetes ends the run.
func narrowLost(gen int, why error) (reviveResult, *reviveError) {
	return reviveResult{generation: gen, stored: true},
		&reviveError{status: http.StatusServiceUnavailable, reason: reasonRevivePolicyUnconfirmedLost, lost: true, msg: why.Error()}
}

// deliver tells the proxy and answers the start by what it said.
func (ne *narrowEdit) deliver(ctx context.Context) (reviveResult, *reviveError) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), narrowBound(ne.run, narrowDeliverTimeout, narrowRunnerDeliverTimeout))
	after, nerr := ne.n.NarrowProxy(dctx, ne.run.SandboxRef, ne.denies)
	cancel()
	// The answer takes a bound of its own, so a slow earlier step cannot leave it unwritable.
	actx, cancelAnswer := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	defer cancelAnswer()
	switch {
	case errors.Is(nerr, runner.ErrNarrowRefused), errors.Is(nerr, runner.ErrNarrowUnsupported),
		errors.Is(nerr, runner.ErrNarrowNotListening), errors.Is(nerr, runner.ErrNarrowNotSent):
		// Nothing was delivered, so the start is abandoned and the generation undone.
		msg := "the run's proxy refused the edit, so nothing changed"
		switch {
		case ne.onRunner && errors.Is(nerr, runner.ErrNarrowNotSent):
			msg = ne.s.narrowRunnerNotReached(ctx, ne.run, nerr)
		case errors.Is(nerr, runner.ErrNarrowNotListening) || errors.Is(nerr, runner.ErrNarrowNotSent):
			msg = narrowRetryMsg
		}
		return ne.undo(ctx, nerr, msg)
	case nerr == nil && ne.onRunner && after.BaseChecked && after.ConfigSHA256 != "" && slices.Equal(after.LiveDenies, ne.denies):
		ne.want = after.ConfigSHA256 // the runner's hash of the bytes its proxy now enforces, as for create and replace_proxy
		ok, why, _ := ne.s.proxyStartAnswered(actx, ne.run.ID, ne.start, ne.want)
		if ok {
			return reviveResult{generation: ne.gen, stored: true}, nil
		}
		return narrowLost(ne.gen, errors.New(why))
	case nerr == nil && !ne.onRunner && after.ConfigSHA256 == ne.want && slices.Equal(after.LiveDenies, ne.denies):
		ok, why, _ := ne.s.proxyStartAnswered(actx, ne.run.ID, ne.start, ne.want)
		if ok {
			return reviveResult{generation: ne.gen, stored: true}, nil
		}
		return narrowLost(ne.gen, errors.New(why))
	}
	// The proxy may hold the edit, or less, or more than it was sent. The start fails with no hash, and is
	// never abandoned: the stored generation is the narrower one only while the proxy may have taken it.
	_, why, _ := ne.s.proxyStartAnswered(actx, ne.run.ID, ne.start, "")
	return narrowLost(ne.gen, fmt.Errorf("the proxy's answer could not be confirmed (%s): %v", why, nerr))
}

// undo abandons the edit's start and takes back its generation, storing the config it started from,
// after a failure that left the proxy untouched. msg is the answer when the undo lands.
func (ne *narrowEdit) undo(ctx context.Context, cause error, msg string) (reviveResult, *reviveError) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	defer cancel()
	// Whether or not a begin landed, so a start recorded and then lost to an error is not left pending.
	ne.s.abandonProxyStart(actx, ne.run.ID, proxyStartRec{active: true, gen: ne.gen})
	noop, aerr := ne.s.abandonRunPolicyGeneration(actx, ne.run, ne.gen, ne.stored)
	switch {
	case aerr == nil && noop == nil:
		if errors.Is(cause, runner.ErrNarrowNotListening) || errors.Is(cause, runner.ErrNarrowNotSent) {
			return reviveResult{}, reviveRefused(http.StatusBadGateway, reasonReviveProxyKeptCurrent, msg)
		}
		return reviveResult{}, reviveRefused(http.StatusBadGateway, reasonReviveProxyKeptCurrent, loggedMsg(ctx, msg, cause))
	case aerr == nil && noop.Current != ne.gen:
		// A newer edit stands. It advanced from this edit's stored config, so it carries this edit's hosts.
		return reviveResult{}, &reviveError{status: http.StatusServiceUnavailable, reason: reasonEgressEditSuperseded, msg: narrowSupersedeMsg}
	}
	// The generation stays current, narrower than the untouched proxy: the wait fences its stop on the
	// newest slot, which is what the store compares.
	fence := ne.gen
	if aerr == nil {
		fence = noop.Newest
		aerr = errPolicyNotAbandoned
	}
	return narrowLost(fence, fmt.Errorf("%w: %v", aerr, cause))
}

// withNarrowFate is withRunFate for an in-place edit. Only the two answers that name a new proxy change
// their words: there is none to name. The stored fate is appended as withRunFate does, an unresolved
// stop with no recorded fate says the run will be ended (a Kubernetes stop ends the run), and the
// image cause is never appended, since it cannot hold once the proxy answered the narrowing.
func (s *Server) withNarrowFate(ctx context.Context, run types.AgentRun, rerr *reviveError) *reviveError {
	if rerr == nil || (rerr.reason != reasonEgressEditUnconfirmed && rerr.reason != reasonEgressEditContainmentUnresolved) {
		return rerr
	}
	out := *rerr
	out.msg = strings.Replace(out.msg, "the run's new proxy did not confirm the edited policy", "the run's proxy did not confirm the narrowed policy", 1)
	switch fate, read := s.readRunFate(ctx, run.ID); {
	case fate != "":
		out.msg += fate
	case read && rerr.reason == reasonEgressEditContainmentUnresolved:
		out.msg += "; once that is done, the run will be ended"
	}
	return &out
}
