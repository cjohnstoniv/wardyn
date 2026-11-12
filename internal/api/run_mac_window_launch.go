// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The pending grant a create with desktop_window at_start leaves for the launch: who asked, and what they
// signed in with, taken from the cookie or token the create used. It is kept in the creator's own records under
// the run's id, so the launch (on this replica or after a pool queue's wait) and the windows it opens judge the
// person the create judged, and a creator cut off in between is refused. No record, no open.
const macWindowPendingKey = "mac_window_pending:"

// macWindowNoGrant is the audit reason of an at-start open that finds no pending grant (it is not on the wire).
const macWindowNoGrant = "no_pending_grant"

type macWindowPending struct {
	ActorType    types.ActorType `json:"actor_type"`
	Principal    string          `json:"principal"`
	Email        string          `json:"email,omitempty"`
	AuthorizedAt time.Time       `json:"authorized_at"`
	RevSeq       int64           `json:"rev_seq,omitempty"`
	Token        bool            `json:"token,omitempty"`
	Via          bool            `json:"via,omitempty"`
	// TokenID and CLISessionID are the credential the window is bound to (macWindowAuthority). Absent in a grant
	// kept before the binding existed, which then reads as no binding and is refused.
	TokenID      uuid.UUID `json:"token_id"`
	CLISessionID uuid.UUID `json:"cli_session_id"`
}

// errNoPersonRecords is the refusal of a window at start on a store that keeps no per-person records: the grant
// the launch opens under has nowhere to be kept.
var errNoPersonRecords = errors.New("the store keeps no per-person records")

// macWindowStorePending records a's grant for run id. An error means the create must not go on: it asked for a
// window the launch could not open.
func (s *Server) macWindowStorePending(ctx context.Context, runID uuid.UUID, a macWindowAuthority) error {
	st, ok := s.cfg.Store.(store.PrincipalPrefStore)
	if !ok {
		return errNoPersonRecords
	}
	raw, err := json.Marshal(macWindowPending{ActorType: a.ActorType, Principal: a.Principal, Email: a.Email,
		AuthorizedAt: a.Issue.At, RevSeq: a.Issue.Seq, Token: a.Token, Via: a.Via,
		TokenID: a.TokenID, CLISessionID: a.CLISessionID})
	if err != nil {
		return err
	}
	return st.PutPrincipalPref(ctx, a.Principal, macWindowPendingKey+runID.String(), raw)
}

// macWindowLoadPending is the pending grant of run, ok false when there is none for its creator. A store that
// cannot answer is an error.
func (s *Server) macWindowLoadPending(ctx context.Context, run types.AgentRun) (a macWindowAuthority, ok bool, err error) {
	st, can := s.cfg.Store.(store.PrincipalPrefStore)
	if !can || run.CreatedBy == "" {
		return a, false, nil
	}
	raw, err := st.GetPrincipalPref(ctx, run.CreatedBy, macWindowPendingKey+run.ID.String())
	if errors.Is(err, store.ErrNotFound) {
		return a, false, nil
	}
	if err != nil {
		return a, false, err
	}
	var p macWindowPending
	if json.Unmarshal(raw, &p) != nil || p.Principal == "" || p.Principal != run.CreatedBy {
		return a, false, nil
	}
	return macWindowAuthority{ActorType: p.ActorType, Principal: p.Principal, Email: p.Email,
		Issue: oidc.Issue{At: p.AuthorizedAt, Seq: p.RevSeq}, Token: p.Token, Via: p.Via,
		TokenID: p.TokenID, CLISessionID: p.CLISessionID}, true, nil
}

// macWindowForgetPending drops the pending grant of run once its launch has asked: a window that stays open
// judges its person by the grant it opened under (macWindow.auth), never by this record. A store that cannot
// delete one leaves it, which only costs a row.
func (s *Server) macWindowForgetPending(ctx context.Context, run types.AgentRun) {
	st, ok := s.cfg.Store.(store.PrincipalPrefDeleter)
	if !ok || run.CreatedBy == "" {
		return
	}
	if err := st.DeletePrincipalPref(ctx, run.CreatedBy, macWindowPendingKey+run.ID.String()); err != nil {
		slog.WarnContext(ctx, "wardynd: could not drop a run's pending window grant", "run_id", run.ID, "err", err)
	}
}

// macWindowRecordPending stores the grant of a create that asks for desktop_window, and says true after writing the
// refusal when it cannot: a create must not go on to ask for a window the launch could not open. Every door calls
// it: Review and the policy preview pass record false and store nothing, but refuse as the create would when the
// store keeps no per-person records; only a failed write itself is the create's alone.
func (s *Server) macWindowRecordPending(w http.ResponseWriter, r *http.Request, runID uuid.UUID, window string, actorType types.ActorType, actor string, record bool) bool {
	if window == "" {
		return false
	}
	if !record {
		if _, ok := s.cfg.Store.(store.PrincipalPrefStore); ok {
			return false
		}
		writeServerError(w, r, "record the window's grant", errNoPersonRecords)
		return true
	}
	if err := s.macWindowStorePending(r.Context(), runID, macWindowAuthorityFrom(r.Context(), actorType, actor, s.cfg.Now().UTC())); err != nil {
		writeServerError(w, r, "record the window's grant", err)
		return true
	}
	return false
}

// macWindowAtStartIfAsked opens the window a create asked for as the run starts: in the background, so the start
// of the agent does not wait for the Mac. A refusal is audited and the run goes on (macWindowAtStart).
func (s *Server) macWindowAtStartIfAsked(ctx context.Context, runID uuid.UUID, asked bool) {
	if asked {
		s.goBackground(func() { s.macWindowAtStart(context.WithoutCancel(ctx), runID) })
	}
}

// macWindowAtStartOrAfterPrep asks for the window a create asked for, as run reaches RUNNING: now, beside the
// agent's start, or, for an interactive macOS VM run, whose window is refused until its prep records done, from the
// returned hook, which startAgentsOrIdle runs once the prep is done. The hook is nil when nothing waits on the prep.
func (s *Server) macWindowAtStartOrAfterPrep(ctx context.Context, run types.AgentRun, interactive, asked bool) (onPrepared func()) {
	now, afterPrep := macWindowAtStartWhen(run.MachineKind, interactive, asked)
	s.macWindowAtStartIfAsked(ctx, run.ID, now)
	if !afterPrep {
		return nil
	}
	return func() { s.macWindowAtStartIfAsked(ctx, run.ID, true) }
}

// macWindowAtStartWhen says when an at-start window is asked for: now (as the run reaches RUNNING), after its prep
// (an interactive macOS VM run), or never (not asked). It is never both, so the pending grant is spent once.
func macWindowAtStartWhen(kind types.MachineKind, interactive, asked bool) (now, afterPrep bool) {
	if !asked {
		return false, false
	}
	if interactive && kind.IsMacOSVM() {
		return false, true
	}
	return true, false
}

// macWindowAtStart is the launch trigger: the run was created with desktop_window at_start and has just come
// up, so its creator is asked for a window as `wardyn window` would, under the grant the create left
// (macWindowStorePending), never under whatever the launch's own context carries. A refusal is audited and the
// run goes on: the run page and the CLI offer the window later. The run is read again here, since dispatch has
// set its recording level and its state since create. The open waits for the run to be up by where it is called:
// after the sandbox started and the run is RUNNING, or, for an interactive macOS VM run, once its prep has
// recorded done (runs_dispatch.go). It is asked once; a refusal is not retried, so a window never appears on a
// Mac the person did not just ask for. A
// refusal under a loaded grant is also kept as the grant principal's last close (not_opened, with its sentence),
// so the run page says why the window it asked for is not there.
func (s *Server) macWindowAtStart(ctx context.Context, runID uuid.UUID) {
	denied := func(reason string) {
		a := macWindowAuthority{ActorType: types.ActorSystem, Principal: "wardynd"}
		data := macWindowData(false, macWindowTriggerLaunch, nil)
		data["reason"] = reason
		s.auditMacWindowBegin(ctx, &runID, a, runID.String(), "denied", data)
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: could not read a run to open its window at start", "run_id", runID, "err", err)
		denied(reasonRunUnreadable)
		return
	}
	defer s.macWindowForgetPending(ctx, run) // the grant is spent whether the open succeeds or is refused
	a, ok, err := s.macWindowLoadPending(ctx, run)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "wardynd: could not read a run's pending window grant", "run_id", runID, "err", err)
		denied(reasonRunUnreadable)
		return
	case !ok:
		denied(macWindowNoGrant)
		return
	}
	ctx = s.macGUIEnvAwait(ctx, run) // the guest's settings are in, or failed, before the window is asked for
	if _, rf := s.openMacWindow(ctx, run, a, macWindowTriggerLaunch); rf != nil {
		slog.InfoContext(ctx, "wardynd: a run's window was not opened at start", "run_id", runID, "reason", macWindowRefusalReason(rf))
		s.macWindowStoreLastClose(ctx, a.Principal, runID, macWindowLastClose{Reason: macWindowEndedNotOpened, Message: macWindowRefusalSentence(rf)})
	}
}

// macWindowCreateRefusal is macWindowRefusal's rows that a create can answer, for a request with desktop_window
// at_start: rows 1, 4, 5 and 6, and the part of row 3 that is about who asks (a human with a principal, no
// delegated token, not an operator's run), so Review shows a refusal before launch. The rest need a run, a runner or a
// registry, and the launch hook asks the whole of macWindowRefusal again once the run is up. The recording level
// is resolved at dispatch; the caller passes the level resolved from the same layers (createRecordingChoice), so a
// locked organisation level or the caller's walls refuse the window at create, not at launch.
func (s *Server) macWindowCreateRefusal(ctx context.Context, r *http.Request, ceiling governanceCeiling, req *createRunRequest, recording types.RecordingLevel) *runRefusal {
	if rf := s.macWindowSiteRefusal(ctx); rf != nil {
		return rf
	}
	if at, principal := actorFromRequest(r); at != types.ActorHuman || principal == "" || ceiling.Operator ||
		macWindowAuthorityFrom(r.Context(), at, principal, s.cfg.Now().UTC()).Via {
		return macWindowNotYourMac("")
	}
	if rf := s.macWindowGovernanceRefusal(ctx, ceiling, uuid.Nil); rf != nil {
		return rf
	}
	if recording == types.RecordingScreenActions {
		return runError(http.StatusForbidden, reasonMacWindowUnrecorded, macWindowUnrecordedMsg)
	}
	if s.macOSDesktopSignInsOff(ctx, types.AgentRun{MachineKind: types.MachineKind(req.MachineKind), Agent: req.Agent}) {
		return runError(http.StatusForbidden, reasonDesktopSignInsOff, macOSDesktopSignInsOffMsg(req.Agent))
	}
	return nil
}

// stepMacWindow refuses a create that asks for a window at start that macWindowCreateRefusal would refuse, with
// its sentence, at create and Review; it is never dropped and the run never launches without the window it
// asked for. A create writes the denied takeover.begin row, with no run to name yet. Every refusal names the
// desktop_window field, so the console clears that choice on it and on nothing that only shares a reason.
func (s *Server) stepMacWindow(f *runFold, _ *foldRecorder) bool {
	if f.req.DesktopWindow == "" {
		return true
	}
	choice, rf := s.createRecordingChoice(f.r.Context(), f.r, f.spec.Recording, f.place, f.ceiling, f.policyID == nil)
	if rf == nil {
		rf = s.macWindowCreateRefusal(f.r.Context(), f.r, f.ceiling, f.req, choice.Resolve())
	}
	if rf == nil {
		return true
	}
	s.auditMacWindowCreateDenied(f.r, f.req, rf, f.mode == foldCreate)
	rf.body.Field = fieldDesktopWindow
	return !rf.write(s, f.w, f.r)
}

// auditMacWindowCreateDenied writes a create's denied takeover.begin row for rf, with no run to name yet. The
// dry doors pass create false and write none: the mode rides as a flag so every door makes the same calls.
func (s *Server) auditMacWindowCreateDenied(r *http.Request, req *createRunRequest, rf *runRefusal, create bool) {
	if !create {
		return
	}
	at, principal := actorFromRequest(r)
	data := macWindowData(req.Experience == types.ExperienceBackground, macWindowTriggerLaunch, nil)
	data["reason"] = macWindowRefusalReason(rf)
	outcome := "denied"
	if rf.err != nil {
		outcome = "failure" // a failed read is a server error, not a policy refusal
	}
	s.auditMacWindowBegin(r.Context(), nil, macWindowAuthority{ActorType: at, Principal: principal}, "create", outcome, data)
}

// fieldDesktopWindow is the request field every refusal of stepMacWindow names (errorBody.Field).
const fieldDesktopWindow = "desktop_window"

// desktopWindowRequestRefusal is the 400 for a desktop_window the request cannot carry; "" when it can.
func desktopWindowRequestRefusal(req createRunRequest) string {
	switch {
	case req.DesktopWindow == "":
		return ""
	case req.DesktopWindow != client.DesktopWindowAtStart:
		return `desktop_window must be "at_start" or absent.`
	case !types.MachineKind(req.MachineKind).IsMacOSVM():
		return "desktop_window is for a macOS VM run (machine_kind macos_vm)."
	}
	return ""
}
