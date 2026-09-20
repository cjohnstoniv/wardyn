// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// One live sign-in sandbox per person.
//
// The report: a person whose first sign-in died half-way starts a second one,
// completes it, and is told "The sandbox reported a capture the server does not
// have — sign in again" — the thing they just did. The mechanism is the ABORTED
// run outliving the retry. Nothing on the client ends it: of the pane's error
// arms only the unreadable one leaves a run alive, and the pane unmounting
// without Cancel leaves one every time (harness-login-pane.tsx has no cleanup
// effect). That orphan lives to harnessLoginIdleCap — thirty minutes — with
// `aws sso login` still polling the device code, and the self-running
// image finishes its sign-in UNATTENDED: when the human approves in the old
// tab (or in both), the old run's helper PUTs its capture, legitimately, and
// possibly after the new run's. The new run's own status read then sees a row
// stamped with SOMEONE ELSE'S run id and refuses a credential that works.
//
// The fix is server-side and it is here rather than in the pane for three
// reasons: a client-side kill covers only the client that is still running (not
// a closed tab, not a second browser); it races the concurrency quota, which
// binds login runs — a member with max_concurrent_runs 1 would get "too many
// runs at once" from their own orphan; and a person is not a party to their own
// audit trail. So a NEW sign-in ends that person's older ones, before the new
// run is created, in the one place every client path goes through.
//
// It is deliberately narrow: the CALLER'S OWN runs, of the login task, on the
// same agent. It is not a sweep and it never touches anyone else's run.
const (
	// supersedeReasonNewLogin rides the superseded run's run.kill DATA (see
	// docs/AUDIT-ACTIONS.md `run.kill`). It is carried OUTSIDE the cascade's
	// error map on purpose: that map is what decides outcome=failure, so a
	// reason put in it would audit every supersede as a failed kill and add a
	// run.revoke row for a run that was torn down perfectly.
	supersedeReasonNewLogin = "superseded_by_new_login"
	// supersedeCASAttempts bounds the re-read below. The only way the KILLED CAS
	// loses is a dispatch forward-transition (PENDING->STARTING->RUNNING) landing
	// between the read and the write, which can happen at most twice for one run
	// and never repeatedly — a bound, not a retry policy.
	supersedeCASAttempts = 3
)

// supersedeCallerLoginRuns kills actor's other non-terminal login runs on this
// agent. Called by launchHarnessLoginRun BEFORE newStepRun, which is where the
// concurrency quota is counted (stepRunCeilingLimits -> CountActiveRunsBy): the
// kill wins the KILLED state change synchronously, so the slot the orphan held
// is free by the time the new run asks for one.
//
// Best effort by design — it never returns an error and never blocks the launch.
// A person who cannot reach their own sign-in because a store read failed is
// worse off than one whose stale sandbox outlives the retry, and the capture PUT
// has its own belt: handleUploadSSOToken refuses a KILLED run
// (refuseReasonRunKilled), because /internal/sso-token/ stays usable by a
// terminal run for five minutes and RevokeRun is best-effort. Failures are
// logged and audited (the run.kill row carries the failing step) rather than
// propagated.
func (s *Server) supersedeCallerLoginRuns(ctx context.Context, actor, agent string, newRunID uuid.UUID) {
	if s.cfg.Store == nil || actor == "" {
		return
	}
	live, err := s.liveLoginRunsBy(ctx, actor, agent)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not look up this person's live sign-in sandboxes; not superseding",
			slog.String("agent", agent), slog.Any("error", err))
		return
	}
	for _, run := range live {
		s.supersedeOneLoginRun(ctx, run, actor, newRunID)
	}
}

// supersedeOneLoginRun runs the kill cascade over one orphaned login run,
// re-reading on a lost CAS: the run may be mid-dispatch (PENDING->STARTING), and
// a supersede that shrugged at a lost CAS would leave exactly the run it exists
// to end.
func (s *Server) supersedeOneLoginRun(ctx context.Context, run types.AgentRun, actor string, newRunID uuid.UUID) {
	for attempt := 0; attempt < supersedeCASAttempts; attempt++ {
		applied, killData, err := s.killRunCascade(ctx, run, types.ActorSystem, "wardynd",
			// WHO is attributed: the server, not the person. They asked for a new
			// sign-in, not for a kill — `superseded_for` names whose sandbox it was,
			// `superseded_by_run` the sign-in that replaced it, so the row answers
			// both "why did my box disappear" and "which one took over" without a
			// join back through harness.login.started.
			map[string]any{
				"reason":            supersedeReasonNewLogin,
				"superseded_for":    actor,
				"superseded_by_run": newRunID.String(),
			})
		if err != nil {
			slog.WarnContext(ctx, "wardynd: could not supersede a live sign-in sandbox",
				slog.String("run_id", run.ID.String()), slog.Any("error", err))
			return
		}
		if applied {
			if len(killData) > 0 {
				// The state is KILLED and the row already carries the failing step;
				// the new sign-in PROCEEDS — the upload belt is what makes that safe.
				slog.WarnContext(ctx, "wardynd: superseded sign-in sandbox was not fully torn down",
					slog.String("run_id", run.ID.String()), slog.Any("errors", killData))
			}
			return
		}
		// Lost the CAS to a forward transition. Re-read and try from the state it
		// actually holds; if it ended on its own there is nothing left to do.
		got, gerr := s.cfg.Store.GetRun(ctx, run.ID)
		if gerr != nil || isTerminalRunState(got.State) {
			return
		}
		run = got
	}
	// Out of attempts: the run kept winning forward transitions. Say so — the
	// caller's retry can still meet "too many runs at once" from this very run,
	// and a quota refusal with nothing in the log is unanswerable.
	slog.WarnContext(ctx, "wardynd: gave up superseding a live sign-in sandbox after repeated state changes",
		slog.String("run_id", run.ID.String()), slog.Int("attempts", supersedeCASAttempts))
}

// supersedeOlderLoginRuns is the SECOND pass, run immediately after the new
// run's row exists — and it is what makes "one live sign-in per person" hold
// under CONCURRENCY in all but one named interleaving.
//
// The pass above reads the live runs and only THEN is the new row inserted, so
// two launches in flight for one person (a double-click, or the console and a
// `wdn_` token on the same subject) each read the other as not-yet-existing:
// neither is superseded, both stay live, and the field report's defect is back
// until a third launch. A mutex would not fix it — wardynd runs replicated, and
// an in-process lock is not a lock at all on the second replica.
//
// So the tie-break is DETERMINISTIC instead, and every replica computes the same
// answer with no coordination: a launch supersedes only the caller's own login
// runs that come BEFORE its own in loginRunPrecedes' total order. Two properties
// follow, and they are the whole design — the first of them holds in all but one
// named interleaving, which is stated rather than claimed away:
//
//   - two becomes unlikely, NOT IMPOSSIBLE — and the residue is named rather
//     than papered over. Whenever a pass sees BOTH rows it ends the earlier one,
//     which covers the ordinary double-click: the second launch's first pass, or
//     one of the two second passes, sees the sibling. What defeats it is
//     created_at being stamped IN-PROCESS by newStepRun BEFORE the insert, so
//     timestamp order and INSERT order can disagree. Take X, whose clock runs
//     behind (a second replica, or a stall between newStepRun and CreateRun),
//     and Y: both first passes see nothing; Y inserts and its second pass runs
//     before X's insert, so it sees only itself; X then inserts and its pass
//     sees both, but Y does not PRECEDE X, so it kills nothing. Two live
//     sign-ins survive — and neither is KILLED, so the capture PUT's guard does
//     not apply to that pair either. A third sign-in clears it. Closing it needs
//     a per-actor advisory lock around insert + pass; that is not built, and it
//     is a LOW residual of a LOW finding.
//   - NEVER ZERO. A launch's own run can only be ended by a pass that ran after
//     its insert, and the LAST insert is followed only by its own pass, which
//     never takes itself. There is always a survivor. The one shape that could
//     have broken it — a launch whose OWN run was already superseded going on to
//     supersede a live sibling — is why the live-set check below exists: such a
//     run returns without touching anything.
//
// Best effort, like the first pass, and for the same reason: the capture PUT's
// KILLED guard is the belt, and nobody is locked out of signing in because a
// store read failed.
func (s *Server) supersedeOlderLoginRuns(ctx context.Context, actor, agent string, newRun types.AgentRun) {
	if s.cfg.Store == nil || actor == "" {
		return
	}
	live, err := s.liveLoginRunsBy(ctx, actor, agent)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not re-check this person's live sign-in sandboxes after the new one was created",
			slog.String("agent", agent), slog.Any("error", err))
		return
	}
	stillLive := false
	for _, run := range live {
		if run.ID == newRun.ID {
			stillLive = true
			break
		}
	}
	if !stillLive {
		// This launch's own run is already terminal — a sibling launch superseded
		// it. A dead run must not supersede a live one, or two launches end with
		// nothing signed in.
		return
	}
	for _, run := range live {
		if !loginRunPrecedes(run, newRun) {
			continue
		}
		s.supersedeOneLoginRun(ctx, run, actor, newRun.ID)
	}
}

// loginRunPrecedes is the total order above: created_at, then the run id as the
// tie-break for two rows stamped on the same clock reading. Total and
// replica-independent — both fields are stored on the row, so every caller
// orders the same pair the same way — and it answers false for a run against
// itself, which is what keeps a launch from superseding the run it just made.
func loginRunPrecedes(a, b types.AgentRun) bool {
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.ID.String() < b.ID.String()
	}
	return a.CreatedAt.Before(b.CreatedAt)
}

// liveLoginRunsBy answers "which of this person's login sandboxes are still
// live", through the optional store seam.
//
// The seam, not the core Store interface: widening Store would make every test
// double and embedding in the tree implement a query none of them are about
// (the RunsByCreatorPager rule). A store WITHOUT it is simply not superseded —
// no unbounded-ListRuns fallback, deliberately: this runs on a route every
// store-less embedding and half the package's doubles drive, and a scan of a
// whole run table to find at most one row is the cost ActiveRunsAtWorkspacePath
// was written to stop paying. The real store is PG, which implements it; the
// capture PUT's KILLED guard is the belt either way.
//
// Selected by creator + task + agent: `provider` is not a column on a run (it
// lives only in the harness.login.started audit datum), and the agent is what
// separates the AWS SSO login box from the subscription one.
func (s *Server) liveLoginRunsBy(ctx context.Context, actor, agent string) ([]types.AgentRun, error) {
	reader, ok := s.cfg.Store.(store.ActiveRunsByCreatorReader)
	if !ok {
		return nil, nil
	}
	return reader.ActiveRunsByCreator(ctx, actor, harnessLoginTask, agent)
}
