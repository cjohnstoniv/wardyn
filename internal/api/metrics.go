// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// storePingTimeout bounds the Postgres reachability check shared by the /readyz
// readiness probe (handleReadyz) and the wardyn_store_up gauge below, so the two
// cannot drift into disagreeing about whether the store is up.
const storePingTimeout = 3 * time.Second

// metrics holds the control plane's scrape counters, served as Prometheus text
// exposition by GET /metrics (admin-gated — see routes()).
//
// Why this exists: the audit fanout (file/syslog/webhook, file on by default in
// compose) already carries run/approval/deny/mint counts to a SIEM, but nothing
// exposed them to a scrape target, and sandbox launch latency was measured
// nowhere at all.
//
// Why it is hand-rolled: the Prometheus text exposition format is a dozen
// Fprintf lines, while prometheus/client_golang would drag protobuf + ~5 more
// modules through this repo's licenses / SBOM / govulncheck / tidy gates — in a
// security product — to emit five counters. Same scrape, no supply chain.
//
// ponytail: ONE mutex for the whole set (a handful of increments per run is not
// a hot path) and no histogram — launch latency ships as a sum/count average.
// Add buckets the day someone actually needs a p99.
type metrics struct {
	mu           sync.Mutex
	runs         map[types.RunState]int64 // runs that reached a terminal state
	approvals    map[string]int64         // "approved" / "denied"
	egressDenies int64
	mints        int64
	launchSum    float64 // seconds, run creation -> RUNNING
	launchCount  int64

	// authFailedSuppressed counts auth.failed audit emits the rate limiter
	// DISCARDED, and it is the reason that limiter is safe to have. Without it
	// a burst of authentication failures produces a bounded ~1 row/sec no
	// matter how hard it is pushed, so a credential-stuffing run reads QUIETER
	// than a handful of typos — the audit volume flattens exactly when the
	// thing it describes accelerates. Counted rather than logged, because
	// nobody alerts on a log line they do not know to grep for, and because
	// this is precisely the shape that has to be graphable: audit rows flat and
	// this series climbing IS the attack.
	authFailedSuppressed int64
	// authStoreErrors counts requests an authentication lane could not decide
	// because its store read failed. Distinct from wardyn_store_up: that gauge
	// answers a PING (see writeHealthGauges), which a healthy pool passes while
	// one table denies a read or one statement times out.
	authStoreErrors int64
	// driveRefusals counts runs REFUSED their user drive, by reason. It is the
	// series that made a whole class of failure operable: a refused drive was
	// answered to the member as a 422 and recorded NOWHERE — no audit row (the
	// door's authz.denied covers the profile arm alone), no log line for five of
	// the six arms, and no request log at all (routes.go wires no logger). So
	// "nobody can mount their drive since the NAS moved" reached an operator as
	// a support ticket, if at all.
	//
	// By reason, and the label set is CLOSED (driveRefusalReason*): a reason is
	// a cardinality decision, and a free-form label here would be one series per
	// message. Never the drive, the subject or the path — those are the audit
	// log's and the slog line's, both of which this counter points at.
	driveRefusals map[string]int64
	// ssoRefreshOutcomes counts each control-plane AWS SSO renewal ATTEMPT
	// (refreshAWSSSOBlob's CreateToken call), by outcome — the same four the
	// harness.credential.refresh audit row's own branching already
	// distinguishes: success is "redeemed at AWS" — the increment
	// fires the instant CreateToken succeeds, BEFORE the re-Put; a persist
	// failure afterward is still audited failure/persist_error, but counts
	// here too, since the run was served from the renewed pair either way.
	// spent is the refresh token being newly discovered dead — it increments
	// ONCE per token, at the CreateToken call that first sees invalid_grant et
	// al.; every LATER dispatch on that same token exits at an earlier,
	// uncounted short-circuit (the in-memory dead-mark check, before
	// CreateToken is ever called again), so this series reads "how many
	// distinct sessions AWS retired," not "how many times someone hit a dead
	// one." transport_error is a transient failure that still served the run
	// from a still-valid token; unavailable is a transient failure with
	// nothing left to serve. BY OUTCOME, CLOSED set (ssoRefreshOutcomeValues):
	// a graphable "how often does renewal fail, and which way" that the audit
	// trail alone is not (nobody alerts on a log line they do not know to
	// grep for).
	ssoRefreshOutcomes map[string]int64

	// credentialReauthOutcomes counts each mid-run credential re-auth WORKFLOW's
	// outcome: requested when a lapsed credential opens a hold,
	// resolved when a sign-in answers it, and expired/cancelled when the row was
	// aged out or the run ended under it. timeout is the one the operator
	// actually tunes on — it means the sandbox's SDK was told to fail because
	// nobody signed in inside WARDYN_CREDENTIAL_REAUTH_TIMEOUT, and a series that
	// is mostly timeouts is a deployment whose people are not seeing the request.
	//
	// By outcome, CLOSED set (credentialReauthOutcomeValues) for the reason
	// driveRefusals gives: a label nobody enumerated is one series per string.
	//
	// Every label is counted at its own transition, which is the whole
	// correction: requested at the raise, resolved at the resolution, expired
	// where the sweeper ages a row out, cancelled where a terminal run cancels
	// one, timeout where the daemon ingests the sidecar's decision row. The
	// first shape bumped expired/cancelled at a later RESOLVE that happened to
	// meet a terminal row, which counts retries rather than outcomes (with the
	// measured ~30 s cadence, dozens per row) and never fired at all once the
	// sidecar had given up.
	credentialReauthOutcomes map[string]int64
	// credentialReauthWaitSum / Count are how long a re-auth request stayed
	// open — raised to resolved. Sum and count, i.e. an average, and no
	// histogram until someone needs a p99 (this type's ponytail note).
	//
	// It is the number an operator tunes the knob against: if the average wait
	// approaches WARDYN_CREDENTIAL_REAUTH_TIMEOUT, people are only just making
	// it, and the holds that DIDN'T make it are the timeouts beside them.
	// Measured at the RESOLUTION, in the control plane, because that is the one
	// party that sees both ends; the sidecar sees only its own budget.
	credentialReauthWaitSum   float64
	credentialReauthWaitCount int64

	// startWaitSum / startWaitCount are how long a sandbox that was still being
	// created spent on each SUBSTRATE reason — the series that turns an
	// anecdote ("127s and 131s, on two occasions") into something an operator can
	// graph. Same shape as launchSum/launchCount: sum and count, i.e. an average,
	// and no histogram until someone needs a p99 (see this type's ponytail note).
	//
	// By reason, with a CLOSED label set (startWaitReasons), for exactly the
	// argument driveRefusals makes: a substrate reason is not Wardyn's to
	// enumerate — a future kubelet can invent one — and a free-form label here
	// would be one series per string the platform ever says. Anything unknown
	// lands on "other", which is itself the signal that the vocabulary needs a row.
	startWaitSum   map[string]float64
	startWaitCount map[string]int64
}

// startWaited records one stretch a starting sandbox spent on one reason.
func (m *metrics) startWaited(reason string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.startWaitSum == nil {
		m.startWaitSum = map[string]float64{}
		m.startWaitCount = map[string]int64{}
	}
	label := startWaitReasonLabel(reason)
	m.startWaitSum[label] += d.Seconds()
	m.startWaitCount[label]++
}

// driveRefused records one run refused its user drive, by reason.
func (m *metrics) driveRefused(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.driveRefusals == nil {
		m.driveRefusals = map[string]int64{}
	}
	m.driveRefusals[reason]++
}

// ssoRefreshOutcomeValues is the closed label set ssoRefreshRecorded accepts,
// declared up front for the same reason driveRefusalReasons is: it seeds
// every series at zero on the first scrape rather than waiting for the first
// occurrence of each, and a caller passing anything outside this list is
// dropped rather than starting a new, uncounted series (see ssoRefreshRecorded).
var ssoRefreshOutcomeValues = []string{
	ssoRefreshOutcomeSuccess, ssoRefreshOutcomeSpent, ssoRefreshOutcomeTransportError, ssoRefreshOutcomeUnavailable,
}

const (
	ssoRefreshOutcomeSuccess        = "success"
	ssoRefreshOutcomeSpent          = "spent"
	ssoRefreshOutcomeTransportError = "transport_error"
	ssoRefreshOutcomeUnavailable    = "unavailable"
)

// credentialReauthOutcomeValues is the closed label set
// credentialReauthRecorded accepts.
var credentialReauthOutcomeValues = []string{
	credentialReauthOutcomeRequested, credentialReauthOutcomeResolved,
	credentialReauthOutcomeExpired, credentialReauthOutcomeCancelled,
	credentialReauthOutcomeTimeout,
}

const (
	credentialReauthOutcomeRequested = "requested"
	credentialReauthOutcomeResolved  = "resolved"
	credentialReauthOutcomeExpired   = "expired"
	credentialReauthOutcomeCancelled = "cancelled"
	// timeout: the proxy's hold ran out and the sandbox's call was failed.
	// Counted where the daemon INGESTS the sidecar's decision row for
	// credential:reauth-timeout (handlePostDecision), which is the one place
	// the control plane learns a hold expired — the expiry happens in the
	// sidecar and the approval row deliberately stays PENDING.
	credentialReauthOutcomeTimeout = "timeout"
)

// credentialReauthRecorded records one re-auth workflow transition. Silently
// drops anything outside the closed set, exactly as ssoRefreshRecorded does.
func (m *metrics) credentialReauthRecorded(outcome string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !slices.Contains(credentialReauthOutcomeValues, outcome) {
		return
	}
	if m.credentialReauthOutcomes == nil {
		m.credentialReauthOutcomes = map[string]int64{}
	}
	m.credentialReauthOutcomes[outcome]++
}

// credentialReauthResolved records one re-auth request answered, and how long
// it was open. A negative or absurd duration (a clock step, a row with no
// requested_at) is counted as an outcome but not as a wait.
func (m *metrics) credentialReauthResolved(waited time.Duration) {
	m.credentialReauthRecorded(credentialReauthOutcomeResolved)
	if waited <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.credentialReauthWaitSum += waited.Seconds()
	m.credentialReauthWaitCount++
}

// ssoRefreshRecorded records one control-plane AWS SSO renewal attempt's
// outcome. Silently drops anything outside ssoRefreshOutcomeValues — a typo'd
// label must not start an uncounted, un-zeroed series.
func (m *metrics) ssoRefreshRecorded(outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !slices.Contains(ssoRefreshOutcomeValues, outcome) {
		return
	}
	if m.ssoRefreshOutcomes == nil {
		m.ssoRefreshOutcomes = map[string]int64{}
	}
	m.ssoRefreshOutcomes[outcome]++
}

// authFailedSuppressedInc records one dropped auth.failed audit emit.
func (m *metrics) authFailedSuppressedInc() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.authFailedSuppressed++
}

// authStoreErrorInc records one authentication attempt abandoned on a store
// failure.
func (m *metrics) authStoreErrorInc() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.authStoreErrors++
}

func (m *metrics) runTerminal(st types.RunState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runs == nil {
		m.runs = map[types.RunState]int64{}
	}
	m.runs[st]++
}

func (m *metrics) approvalDecided(approve bool) {
	decision := "denied"
	if approve {
		decision = "approved"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.approvals == nil {
		m.approvals = map[string]int64{}
	}
	m.approvals[decision]++
}

func (m *metrics) egressDenied() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.egressDenies++
}

// Non-policy DENY rule_sources. An egress.Deny decision log carries one of these
// when nothing was denied by policy at all:
//
//   - builtin:dial-failed — emitted from exactly three sites in
//     internal/egress/proxy, all genuine dial failures on a request policy
//     ALLOWED, where the network lost it: proxy.go's forward-path round trip,
//     proxy.go's CONNECT tunnel dial, and llm_routes.go's brokered-LLM round
//     trip. RESOLVED (F065-gatewayvet): a fourth site — llm_routes.go's
//     gatewayTarget arm, on errGatewayVet, vetTrustedHost's GUARD refusal of the
//     configured model gateway — carries its own rule_source
//     (ruleSourceGatewayVetFailed, egress lane) rather than reusing this same
//     source, which would file a config problem the operator's own gateway can
//     never satisfy alongside failures the network actually caused. It is
//     DELIBERATELY absent from the exclusion list below — a guard refusal counts
//     as a denial like any other.
//   - egress.decisions.dropped:<n> — decisions.go's synthetic summary for
//     decision records the buffer had to drop. An audit-FIDELITY alert about a
//     wedged control plane, not a denial of anything; the count rides in the
//     rule_source, hence the prefix match.
//   - credential:reauth-timeout — the proxy held a sandbox's AWS SSO credential
//     exchange while its owner was asked to sign in again, and nobody signed in
//     before the budget ended. Policy allowed that host and allowed that
//     request; what ran out was a HUMAN's time. Counting it here would page
//     security for a person who went to lunch, on the one series whose HELP
//     promises "denial by policy". It keeps its egress.deny audit row and its
//     own wardyn_credential_reauth_total{outcome="timeout"}, which is the
//     series an operator actually wants for it.
const (
	ruleSourceDialFailed       = "builtin:dial-failed"
	ruleSourceDroppedDecisions = "egress.decisions.dropped:"
	// Mirrors internal/egress/proxy's ruleSourceCredentialReauthTimeout; the
	// two packages do not import each other, and the decision arrives here as
	// a string on the wire.
	ruleSourceCredentialReauthTimeout = "credential:reauth-timeout"
)

// isPolicyDeny reports whether an egress.Deny with this rule_source is a denial
// by policy — the thing wardyn_egress_denies_total's HELP string promises and
// the only thing an operator alerting on that series wants to be paged for.
//
// Exclusion, not an allowlist, on purpose: the policy/guard sources are open-
// ended (policy, approval:<id>, builtin:private-ip, policy:tool-deny,
// brokered:git-pat:denied, scan:blocked, site-config:*, …) and a new one is one
// feature away. An allowlist would silently UNDERCOUNT real denials — a security
// counter failing quiet — while this list fails toward counting: a source nobody
// classified still moves the series, and only the two known non-denials do not.
func isPolicyDeny(ruleSource string) bool {
	return ruleSource != ruleSourceDialFailed &&
		ruleSource != ruleSourceCredentialReauthTimeout &&
		!strings.HasPrefix(ruleSource, ruleSourceDroppedDecisions)
}

func (m *metrics) credentialMinted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mints++
}

func (m *metrics) sandboxLaunched(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.launchSum += d.Seconds()
	m.launchCount++
}

// write emits the counters in Prometheus text exposition format. Label VALUES
// are Wardyn enums (run states, approved/denied) — never operator input — so no
// escaping is needed here.
func (m *metrics) write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Fprint(w, "# HELP wardyn_runs_total Runs that reached a terminal state, by state.\n"+
		"# TYPE wardyn_runs_total counter\n")
	for st, n := range m.runs {
		fmt.Fprintf(w, "wardyn_runs_total{state=%q} %d\n", string(st), n)
	}
	fmt.Fprint(w, "# HELP wardyn_approval_decisions_total Approval decisions, by outcome.\n"+
		"# TYPE wardyn_approval_decisions_total counter\n")
	for decision, n := range m.approvals {
		fmt.Fprintf(w, "wardyn_approval_decisions_total{decision=%q} %d\n", decision, n)
	}
	fmt.Fprint(w, "# HELP wardyn_drive_refusals_total Runs refused their user drive, by reason.\n"+
		"# TYPE wardyn_drive_refusals_total counter\n")
	for _, reason := range driveRefusalReasons {
		fmt.Fprintf(w, "wardyn_drive_refusals_total{reason=%q} %d\n", reason, m.driveRefusals[reason])
	}
	fmt.Fprint(w, "# HELP wardyn_sso_refresh_total Control-plane AWS SSO CreateToken renewal attempts, by outcome.\n"+
		"# TYPE wardyn_sso_refresh_total counter\n")
	for _, outcome := range ssoRefreshOutcomeValues {
		fmt.Fprintf(w, "wardyn_sso_refresh_total{outcome=%q} %d\n", outcome, m.ssoRefreshOutcomes[outcome])
	}
	fmt.Fprint(w, "# HELP wardyn_credential_reauth_total Mid-run model-credential re-auth workflows, by outcome (a lapsed captured AWS SSO session held while its owner signs in again).\n"+
		"# TYPE wardyn_credential_reauth_total counter\n")
	for _, outcome := range credentialReauthOutcomeValues {
		fmt.Fprintf(w, "wardyn_credential_reauth_total{outcome=%q} %d\n", outcome, m.credentialReauthOutcomes[outcome])
	}
	fmt.Fprintf(w, "# HELP wardyn_credential_reauth_wait_seconds Time a mid-run model-credential re-auth request stayed open, from the raise to the sign-in that answered it.\n"+
		"# TYPE wardyn_credential_reauth_wait_seconds summary\n"+
		"wardyn_credential_reauth_wait_seconds_sum %g\nwardyn_credential_reauth_wait_seconds_count %d\n",
		m.credentialReauthWaitSum, m.credentialReauthWaitCount)
	// HELP text: DRAFT (M2 canon pending). M2 recommends the HELP-only
	// remediation (this wording change) over the filed alternative that also
	// splits the series into {reason="policy"|"dial_failed"|"decisions_dropped"};
	// no owner ruling yet (M2-canon-sheet.md §2/§6b), so the series itself is
	// unchanged — still one unlabeled counter, still isPolicyDeny-scoped.
	fmt.Fprintf(w, "# HELP wardyn_egress_denies_total Egress decisions ingested with decision=deny, by reason (proxy decision ingest).\n"+
		"# TYPE wardyn_egress_denies_total counter\nwardyn_egress_denies_total %d\n", m.egressDenies)
	fmt.Fprintf(w, "# HELP wardyn_credential_mints_total Credentials minted by the broker.\n"+
		"# TYPE wardyn_credential_mints_total counter\nwardyn_credential_mints_total %d\n", m.mints)
	fmt.Fprintf(w, "# HELP wardyn_auth_failed_suppressed_total Authentication failures whose auth.failed audit row was dropped by the rate limiter. Audit volume is capped at ~1/sec, so this series — not the audit trail — is what grows during a burst.\n"+
		"# TYPE wardyn_auth_failed_suppressed_total counter\nwardyn_auth_failed_suppressed_total %d\n", m.authFailedSuppressed)
	fmt.Fprintf(w, "# HELP wardyn_auth_store_errors_total Requests an authentication lane could not decide because its store read failed (answered 500). Not covered by wardyn_store_up, which only pings.\n"+
		"# TYPE wardyn_auth_store_errors_total counter\nwardyn_auth_store_errors_total %d\n", m.authStoreErrors)
	fmt.Fprint(w, "# HELP wardyn_run_start_wait_seconds Time a sandbox still being created spent waiting on each substrate reason (pulling an image, waiting for a node, a reference that will not pull).\n"+
		"# TYPE wardyn_run_start_wait_seconds summary\n")
	for _, reason := range startWaitReasons {
		fmt.Fprintf(w, "wardyn_run_start_wait_seconds_sum{reason=%q} %g\n", reason, m.startWaitSum[reason])
		fmt.Fprintf(w, "wardyn_run_start_wait_seconds_count{reason=%q} %d\n", reason, m.startWaitCount[reason])
	}
	// A summary with no quantiles: sum/count only, i.e. an average launch time.
	fmt.Fprintf(w, "# HELP wardyn_sandbox_launch_seconds Time from run creation to RUNNING.\n"+
		"# TYPE wardyn_sandbox_launch_seconds summary\n"+
		"wardyn_sandbox_launch_seconds_sum %g\nwardyn_sandbox_launch_seconds_count %d\n",
		m.launchSum, m.launchCount)
}

// handleMetrics serves the scrape surface. It is admin-only (mounted INSIDE the
// humanOrAdminAuth group, behind requireOperator — routes.go), on purpose: wardynd fails the public API closed when no
// admin token is configured (cmd/wardynd) and refuses capability disclosure on
// the anonymous /healthz, so an open /metrics would contradict that posture. A
// Prometheus scrape_config authenticates with two lines of `authorization:`.
//
// The exposition is composed IN FULL before a single byte reaches the
// ResponseWriter, and that ordering is the point rather than a style choice.
// writeHealthGauges below makes live store reads; streaming straight at the
// ResponseWriter commits 200 plus the whole counter block first, so anything
// that goes wrong afterwards can no longer change the status. The router's
// Recoverer then writes a 500 that lands nowhere and the scrape reads back as a
// HEALTHY 200 whose body simply stops mid-file — which Prometheus ingests as
// "those series do not exist any more", not as a failed scrape. That is
// strictly worse than a 5xx, because `up` stays 1 and an alert on an absent
// series is the only thing left that could notice. writeEbpfGroundtruthCounters
// already reasons about a malformed label taking the WHOLE scrape with it
// (healthz.go); a buffer is what makes that all-or-nothing true of a panic too.
// One small allocation per scrape, on a body of a few KB.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var body bytes.Buffer
	s.metrics.write(&body)
	s.writeHealthGauges(r, &body)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write(body.Bytes())
}

// writeHealthGauges appends the two LIVE gauges — sampled at scrape time rather
// than counted as events happen, because both describe a standing condition, not
// a rate.
//
// They exist because a store outage makes every counter above simply stop
// moving, which a scrape cannot tell apart from a quiet cluster: wardyn_store_up
// is the same Postgres ping /readyz makes (0 = the pod is out of the Service's
// endpoints and runs are failing their durable writes), and
// wardyn_audit_spool_lines is the backlog those failed writes are spooling to
// disk. A spool that grows and never drains back to 0 is the fallback doing
// its job while the drain loop is not doing its own — invisible until now
// outside a log line.
func (s *Server) writeHealthGauges(r *http.Request, w io.Writer) {
	// ONE bounded context for every store read this scrape makes. A scrape must
	// answer or fail; it must never hang, and a store that accepts a PING while
	// stalling a query is exactly the asymmetry wardyn_auth_store_errors_total
	// exists to describe — so the ground-truth read below gets the same deadline
	// the ping does rather than the caller's unbounded request context.
	ctx, cancel := context.WithTimeout(r.Context(), storePingTimeout)
	defer cancel()
	// Nil Store (tests, and only tests) matches /readyz: nothing to be down.
	up := 1
	if s.cfg.Store != nil {
		if err := s.cfg.Store.Ping(ctx); err != nil {
			up = 0
		}
	}
	fmt.Fprintf(w, "# HELP wardyn_store_up 1 when the control-plane Postgres answers a PING, 0 when it does not (the same check /readyz makes). Reachability only: a pool that pings can still fail an individual query — see wardyn_auth_store_errors_total.\n"+
		"# TYPE wardyn_store_up gauge\nwardyn_store_up %d\n", up)
	fmt.Fprintf(w, "# HELP wardyn_audit_spool_lines Audit events in the durable fallback spool, waiting to drain back into the store.\n"+
		"# TYPE wardyn_audit_spool_lines gauge\nwardyn_audit_spool_lines %d\n", s.cfg.AuditSpool.Lines())
	fmt.Fprintf(w, "# HELP wardyn_audit_spool_torn_total Spool lines dropped as unparseable (a torn tail from an ENOSPC/partial write).\n"+
		"# TYPE wardyn_audit_spool_torn_total counter\nwardyn_audit_spool_torn_total %d\n", s.cfg.AuditSpool.TornDrops())
	fmt.Fprintf(w, "# HELP wardyn_audit_spool_quarantined_total Spool lines moved aside after the store rejected them repeatedly; each one is an event missing from the queryable trail until it is re-fed.\n"+
		"# TYPE wardyn_audit_spool_quarantined_total counter\nwardyn_audit_spool_quarantined_total %d\n", s.cfg.AuditSpool.Quarantined())
	s.writeSinkDrops(w)
	// The eBPF sensor's cumulative counts, moved off the anonymous
	// /healthz onto this gated scrape where every other volume series lives.
	s.writeEbpfGroundtruthCounters(ctx, w)
}

// writeSinkDrops emits the per-SIEM-sink delivery-drop counter: events a
// webhook/syslog sink shed past its buffer or after retry exhaustion, which the
// audit Fanout counts but nothing exposed to a scrape target. Omitted entirely
// when no sinks are configured (AuditSinkDrops nil), so a deployment without SIEM
// carries no dead series. Sink names come from the sink types (webhook/syslog/
// file) — never operator input — but %q-quoted anyway.
func (s *Server) writeSinkDrops(w io.Writer) {
	if s.cfg.AuditSinkDrops == nil {
		return
	}
	drops := s.cfg.AuditSinkDrops()
	if len(drops) == 0 {
		return
	}
	names := slices.Sorted(maps.Keys(drops)) // deterministic scrape output
	fmt.Fprint(w, "# HELP wardyn_audit_sink_drops_total Audit events dropped by a SIEM sink (buffer overflow or retry exhaustion), by sink.\n"+
		"# TYPE wardyn_audit_sink_drops_total counter\n")
	for _, name := range names {
		fmt.Fprintf(w, "wardyn_audit_sink_drops_total{sink=%q} %d\n", name, drops[name])
	}
}

// RecordCredentialReauthExpired counts credential_reauth rows the approval
// sweeper aged out. Exported for cmd/wardynd's sweeper goroutine, which is the
// ONLY place this transition happens: by the time a row expires the sidecar
// that held for it gave up hours ago, so no resolve will ever meet it and
// counting at a resolve would count nothing at all.
func (s *Server) RecordCredentialReauthExpired(n int) {
	for i := 0; i < n; i++ {
		s.metrics.credentialReauthRecorded(credentialReauthOutcomeExpired)
	}
}
