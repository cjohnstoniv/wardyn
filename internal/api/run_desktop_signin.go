// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/publicsuffix"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Desktop sign-ins (issue #2972), off unless WARDYN_DESKTOP_SIGNINS=on. The
// run's owner signs in to websites in a browser of their own inside the run's
// desktop: uid 1001 in the desktop image, every file and socket in a 0700
// tmpfs store that dies with the container, its traffic on the proxy's
// sign-in lane, which never terminates TLS or reads a URL. The agent is
// stopped for the whole sign-in. A login reaches the agent only when the
// person shares one site, which copies that site's cookies into the agent's
// browser; after that the agent holds them, within its egress, until the run
// ends. This is a uid boundary inside one sandbox, not a sandbox boundary.
//
//	POST /runs/{id}/desktop-signin           begin: stop the agent, start the person's display
//	POST /runs/{id}/desktop-signin/shares    {"site": "<host>"}: share one site's login
//	POST /runs/{id}/desktop-signin/end       end: close the display, resume the agent
//
// The display opens through the UI gateway as the reserved app "_signin"
// (run_desktop_signin_relay.go).

const (
	// desktopSigninApp is the UI gateway's name for the sign-in display. Not
	// a slug, so no policy can declare it (validateUIApps).
	desktopSigninApp = "_signin"
	// desktopUIApp is the desktop image's app: a run without it has no
	// sign-in store or sign-in lane.
	desktopUIApp = "novnc"
	// desktopSigninWindow is how long a sign-in stays open (the 0.10.0 ruling).
	desktopSigninWindow = 30 * time.Minute
	// desktopSigninExport caps one share (the 0.10.0 ruling: 512 KiB).
	desktopSigninExport = 512 << 10
	// signinBinary is the image's helper, by absolute path: never PATH-resolved.
	signinBinary = "/usr/local/bin/wardyn-signin"
	// signinExecTimeout bounds one helper exec (start waits up to 45 s).
	signinExecTimeout = 60 * time.Second
)

// End reasons, in desktop_signins.end_reason and the desktop_signin.close row.
const (
	signinEndDone     = "done"
	signinEndExpired  = "expired"
	signinEndRunEnded = "run_ended"
	signinEndFailed   = "failed"
)

// signinProxyAddr is the run proxy's sign-in listener as the sandbox names it.
var signinProxyAddr = net.JoinHostPort("wardyn-proxy", fmt.Sprint(runner.SigninListenPort))

// desktopSigninsFor reports whether a run with this policy gets the sign-in
// store and lane: the knob is on and the policy declares the desktop.
func (s *Server) desktopSigninsFor(policy types.RunPolicySpec) bool {
	return s.cfg.DesktopSignins && slices.ContainsFunc(policy.UIApps, func(a types.UIApp) bool { return a.Name == desktopUIApp })
}

func (s *Server) desktopSigninStore() (store.DesktopSigninStore, bool) {
	st, ok := s.cfg.Store.(store.DesktopSigninStore)
	return st, ok && s.cfg.DesktopSignins && s.cfg.Runner != nil
}

// signinEntry is who may begin, share in and end a run's sign-in: a named
// person who created the run. Not a super admin on someone else's run, not
// the admin token, not a service identity. DSK-B's admitRunEntry replaces
// this when it lands.
var signinEntry = func(run types.AgentRun, actorType types.ActorType, principal string) bool {
	return actorType == types.ActorHuman && principal != "" && principal != adminTokenPrincipal &&
		!isServicePrincipal(principal) && run.CreatedBy == principal
}

// signinRun loads the run for a sign-in door and answers every refusal that
// comes before the sign-in's own state.
func (s *Server) signinRun(w http.ResponseWriter, r *http.Request) (types.AgentRun, store.DesktopSigninStore, bool) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return types.AgentRun{}, nil, false
	}
	run, ok := s.getRunForEntry(w, r, id)
	if !ok {
		return types.AgentRun{}, nil, false
	}
	st, on := s.desktopSigninStore()
	if !on {
		writeErrorReason(w, http.StatusNotImplemented, reasonDesktopSigninsOff, "desktop sign-ins are off on this deployment (WARDYN_DESKTOP_SIGNINS)")
		return types.AgentRun{}, nil, false
	}
	actorType, principal := actorFromRequest(r)
	if !signinEntry(run, actorType, principal) {
		s.refuseRunOwnerOnly(w, r, run)
		return types.AgentRun{}, nil, false
	}
	if s.refuseBackgroundOnly(w, r, run, "desktop sign-in") {
		return types.AgentRun{}, nil, false
	}
	if run.State != types.RunRunning || run.SandboxRef == "" || runIsKept(run) {
		writeErrorReason(w, http.StatusConflict, reasonDesktopSigninNotRunning, "the run is not running, so it has no desktop to sign in to")
		return types.AgentRun{}, nil, false
	}
	return run, st, true
}

type desktopSigninBeginResponse struct {
	SigninID  uuid.UUID `json:"signin_id"`
	ExpiresAt time.Time `json:"expires_at"`
	// App is the UI gateway app that shows the sign-in display.
	App string `json:"app"`
}

// handleBeginDesktopSignin stops the agent, proves it stopped, and starts the
// person's display. The display never starts unless the stop is proven, and
// any failure after the stop resumes the agent before it answers.
func (s *Server) handleBeginDesktopSignin(w http.ResponseWriter, r *http.Request) {
	run, st, ok := s.signinRun(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	_, principal := actorFromRequest(r)
	apps, err := s.effectiveUIApps(ctx, run.ID)
	if err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopSigninUnavailable, "the run's policy could not be read")
		return
	}
	if !slices.ContainsFunc(apps, func(a types.UIApp) bool { return a.Name == desktopUIApp }) {
		writeErrorReason(w, http.StatusConflict, reasonDesktopSigninNoDesktop, "this run has no desktop: its policy declares no \""+desktopUIApp+"\" app")
		return
	}
	if err := s.thawForExec(ctx, run, types.ActorHuman, principal, "desktop_signin"); err != nil {
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopSigninPauseFailed, "the run is paused and could not be resumed for the sign-in; try again")
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeErrorReason(w, http.StatusInternalServerError, reasonDesktopSigninUnavailable, "the sign-in could not be started")
		return
	}
	cred := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(cred))
	now := s.cfg.Now()
	si := types.DesktopSignin{ID: uuid.New(), RunID: run.ID, Holder: principal, CredentialHash: sum[:],
		BeganAt: now, ExpiresAt: now.Add(desktopSigninWindow)}
	if err := st.BeginDesktopSignin(ctx, si); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeErrorReason(w, http.StatusConflict, reasonDesktopSigninOpen, "a sign-in is already open on this run")
			return
		}
		slog.ErrorContext(ctx, "wardynd: begin desktop sign-in", "run_id", run.ID, "err", err)
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopSigninUnavailable, "the sign-in could not be recorded")
		return
	}
	fail := func(status int, reason, msg, stage string, cause error) {
		slog.WarnContext(ctx, "wardynd: desktop sign-in did not start", "run_id", run.ID, "stage", stage, "err", cause)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorHuman, principal, "desktop_signin.begin", run.ID.String(), "failure",
			mustJSON(map[string]any{"signin_id": si.ID, "stage": stage})))
		if errors.Is(cause, runner.ErrSigninUnsupported) {
			status, reason, msg = http.StatusConflict, reasonDesktopSigninUnsupported, "this run's runner cannot run a desktop sign-in (in-process Docker only)"
		}
		writeErrorReason(w, status, reason, msg)
	}

	// 1. The agent: pid 1 from outside its namespace, then every other
	// process of its uid, proven stopped by the helper.
	se, _ := s.cfg.Runner.(runner.SigninExecer)
	if se == nil {
		_, _ = st.EndDesktopSignin(ctx, si.ID, signinEndFailed, s.cfg.Now())
		fail(0, "", "", "pause", runner.ErrSigninUnsupported)
		return
	}
	if err := se.SignalMain(ctx, run.SandboxRef, "SIGSTOP"); err != nil {
		_, _ = st.EndDesktopSignin(ctx, si.ID, signinEndFailed, s.cfg.Now())
		fail(http.StatusBadGateway, reasonDesktopSigninPauseFailed, "the agent could not be stopped, so the sign-in did not start", "pause", err)
		return
	}
	pauseRec, err := s.signinExec(ctx, run.SandboxRef, false, nil, 1<<20, "pause-agent")
	if err != nil {
		// pause-agent resumes whatever it stopped when its proof fails.
		_ = se.SignalMain(context.WithoutCancel(ctx), run.SandboxRef, "SIGCONT")
		_, _ = st.EndDesktopSignin(ctx, si.ID, signinEndFailed, s.cfg.Now())
		fail(http.StatusBadGateway, reasonDesktopSigninPauseFailed, "the agent could not be proven stopped, so the sign-in did not start; the agent was resumed", "pause", err)
		return
	}
	if !json.Valid(pauseRec) {
		pauseRec = []byte(`{}`)
	}
	si.PauseRecord = pauseRec
	if err := st.SetDesktopSigninPauseRecord(ctx, si.ID, pauseRec); err != nil {
		s.rollbackDesktopSignin(ctx, run, si, principal)
		fail(http.StatusServiceUnavailable, reasonDesktopSigninUnavailable, "the sign-in could not be recorded; the agent was resumed", "record", err)
		return
	}

	// 2. The person's display, with the lane credential on stdin.
	startCfg, _ := json.Marshal(map[string]string{"credential": cred, "proxy": signinProxyAddr})
	if _, err := s.signinExec(ctx, run.SandboxRef, true, startCfg, 64<<10, "start"); err != nil {
		s.rollbackDesktopSignin(ctx, run, si, principal)
		fail(http.StatusBadGateway, reasonDesktopSigninStartFailed, "the sign-in display did not start; the agent was resumed", "start", err)
		return
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorHuman, principal, "desktop_signin.begin", run.ID.String(), "success",
		mustJSON(map[string]any{"signin_id": si.ID, "expires_at": si.ExpiresAt})))
	writeJSON(w, http.StatusCreated, desktopSigninBeginResponse{SigninID: si.ID, ExpiresAt: si.ExpiresAt, App: desktopSigninApp})
}

// rollbackDesktopSignin ends a sign-in that failed after the agent stopped.
// When the end itself cannot finish, the row is left for the sweep.
func (s *Server) rollbackDesktopSignin(ctx context.Context, run types.AgentRun, si types.DesktopSignin, principal string) {
	_ = s.finishDesktopSignin(context.WithoutCancel(ctx), run, si, signinEndFailed, types.ActorHuman, principal)
}

type desktopSigninShareRequest struct {
	Site string `json:"site"`
}

type desktopSigninShareResponse struct {
	Site    string `json:"site"`
	Cookies int    `json:"cookies"`
}

// signinSite reduces a host to the registrable domain a share is for. An IP,
// a public suffix and anything that is not a host name are refused.
func signinSite(raw string) (string, bool) {
	h := strings.TrimRight(strings.ToLower(strings.TrimSpace(raw)), ".")
	if h == "" || net.ParseIP(h) != nil || strings.ContainsAny(h, "/:@ ") {
		return "", false
	}
	site, err := publicsuffix.EffectiveTLDPlusOne(h)
	if err != nil {
		return "", false
	}
	return site, true
}

// signinCookie is the part of a shared cookie the server reads: its domain,
// to re-filter what the sandbox's export returned.
type signinCookie struct {
	Domain string `json:"domain"`
}

func cookieForSite(domain, site string) bool {
	d := strings.ToLower(strings.TrimPrefix(domain, "."))
	return d == site || strings.HasSuffix(d, "."+site)
}

// handleShareDesktopSignin copies one site's cookies from the person's browser
// to the agent's. The bytes pass through wardynd's memory once and are never
// stored or logged; the server re-filters them to the site whatever the
// person's side returned.
func (s *Server) handleShareDesktopSignin(w http.ResponseWriter, r *http.Request) {
	run, st, ok := s.signinRun(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	_, principal := actorFromRequest(r)
	var req desktopSigninShareRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	site, ok := signinSite(req.Site)
	if !ok {
		writeErrorReason(w, http.StatusBadRequest, reasonDesktopSigninSiteInvalid, "site must be a host name under a public suffix, such as example.com")
		return
	}
	si, ok := s.heldSignin(w, ctx, st, run, principal)
	if !ok {
		return
	}
	audit := func(outcome string, cookies int, stage string) {
		d := map[string]any{"signin_id": si.ID, "site": site, "cookies": cookies}
		if stage != "" {
			d["stage"] = stage
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorHuman, principal, "desktop_signin.transfer", site, outcome, mustJSON(d)))
	}
	out, err := s.signinExec(ctx, run.SandboxRef, true, nil, desktopSigninExport+1, "export", "--site", site)
	if err != nil {
		audit("failure", 0, "export")
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopSigninShareFailed, "the site's login could not be read from the sign-in browser; nothing was shared")
		return
	}
	var cookies []json.RawMessage
	if json.Unmarshal(out, &cookies) != nil {
		audit("failure", 0, "export")
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopSigninShareFailed, "the sign-in browser's answer was unreadable; nothing was shared")
		return
	}
	kept := []json.RawMessage{}
	for _, c := range cookies {
		var ck signinCookie
		if json.Unmarshal(c, &ck) == nil && cookieForSite(ck.Domain, site) {
			kept = append(kept, c)
		}
	}
	body, _ := json.Marshal(kept)
	if _, err := s.signinExec(ctx, run.SandboxRef, false, body, 4<<10, "import", "--site", site); err != nil {
		audit("failure", 0, "import")
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopSigninShareFailed, "the login could not be written for the agent; nothing was shared")
		return
	}
	sh := types.DesktopSigninShare{ID: uuid.New(), SigninID: si.ID, RunID: run.ID, Site: site, CookieCount: len(kept),
		SharedBy: principal, SharedAt: s.cfg.Now()}
	if err := st.RecordDesktopSigninShare(ctx, sh); err != nil {
		slog.ErrorContext(ctx, "wardynd: record desktop sign-in share", "run_id", run.ID, "err", err)
	}
	audit("success", len(kept), "")
	writeJSON(w, http.StatusOK, desktopSigninShareResponse{Site: site, Cookies: len(kept)})
}

// heldSignin is the run's open sign-in when principal holds it.
func (s *Server) heldSignin(w http.ResponseWriter, ctx context.Context, st store.DesktopSigninStore, run types.AgentRun, principal string) (types.DesktopSignin, bool) {
	si, err := st.OpenDesktopSignin(ctx, run.ID)
	if errors.Is(err, store.ErrNotFound) || err == nil && si.EndRequested != "" {
		writeErrorReason(w, http.StatusConflict, reasonDesktopSigninNotOpen, "no sign-in is open on this run")
		return types.DesktopSignin{}, false
	}
	if err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopSigninUnavailable, "the run's sign-in could not be read")
		return types.DesktopSignin{}, false
	}
	if si.Holder != principal {
		writeErrorReason(w, http.StatusForbidden, reasonDesktopSigninNotHolder, "this sign-in was begun by someone else")
		return types.DesktopSignin{}, false
	}
	return si, true
}

// handleEndDesktopSignin closes the person's display and resumes the agent.
// When the display cannot be proven closed the agent stays stopped and the
// sweep retries.
func (s *Server) handleEndDesktopSignin(w http.ResponseWriter, r *http.Request) {
	run, st, ok := s.signinRun(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	_, principal := actorFromRequest(r)
	si, ok := s.heldSignin(w, ctx, st, run, principal)
	if !ok {
		return
	}
	if err := s.finishDesktopSignin(ctx, run, si, signinEndDone, types.ActorHuman, principal); err != nil {
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopSigninEndFailed, "the sign-in display could not be proven closed; the agent stays stopped and Wardyn retries")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// finishDesktopSignin ends a sign-in in order: the person's processes are
// killed and proven gone, then the agent is resumed, then the row closes.
// Any failure marks the row for the sweep and leaves the agent stopped.
func (s *Server) finishDesktopSignin(ctx context.Context, run types.AgentRun, si types.DesktopSignin, reason string, actorType types.ActorType, actor string) error {
	st, ok := s.desktopSigninStore()
	if !ok {
		return errors.New("desktop sign-ins are off")
	}
	failed := func(stage string, err error) error {
		slog.WarnContext(ctx, "wardynd: desktop sign-in did not end", "run_id", run.ID, "stage", stage, "err", err)
		_ = st.RequestDesktopSigninEnd(ctx, si.ID, reason)
		s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "desktop_signin.close", run.ID.String(), "failure",
			mustJSON(map[string]any{"signin_id": si.ID, "reason": reason, "stage": stage, "agent_resumed": false})))
		return err
	}
	if _, err := s.signinExec(ctx, run.SandboxRef, true, nil, 4<<10, "stop"); err != nil {
		if errors.Is(err, runner.ErrSandboxGone) {
			return s.closeDesktopSignin(ctx, st, run, si, signinEndRunEnded, actorType, actor, false)
		}
		return failed("stop", err)
	}
	rec := si.PauseRecord
	if len(rec) == 0 {
		rec = []byte(`{}`)
	}
	if _, err := s.signinExec(ctx, run.SandboxRef, false, rec, 4<<10, "resume-agent"); err != nil {
		return failed("resume", err)
	}
	if se, ok := s.cfg.Runner.(runner.SigninExecer); ok {
		if err := se.SignalMain(ctx, run.SandboxRef, "SIGCONT"); err != nil {
			return failed("resume", err)
		}
	}
	return s.closeDesktopSignin(ctx, st, run, si, reason, actorType, actor, true)
}

func (s *Server) closeDesktopSignin(ctx context.Context, st store.DesktopSigninStore, run types.AgentRun, si types.DesktopSignin,
	reason string, actorType types.ActorType, actor string, resumed bool) error {
	ended, err := st.EndDesktopSignin(ctx, si.ID, reason, s.cfg.Now())
	if err != nil {
		return err
	}
	if !ended {
		return nil
	}
	shares, _ := st.CountDesktopSigninShares(ctx, si.ID)
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "desktop_signin.close", run.ID.String(), "success",
		mustJSON(map[string]any{"signin_id": si.ID, "reason": reason, "shares": shares, "agent_resumed": resumed})))
	return nil
}

// signinExec runs one wardyn-signin subcommand in the sandbox, as the person
// (asPerson) or as the container's own user (the agent), with stdin, and
// returns its stdout. A non-zero exit, or more than maxOut bytes, is an error.
func (s *Server) signinExec(ctx context.Context, ref string, asPerson bool, stdin []byte, maxOut int, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, signinExecTimeout)
	defer cancel()
	spec := runner.ExecSpec{Argv: append([]string{signinBinary}, args...)}
	var (
		sess *runner.ExecSession
		err  error
	)
	if asPerson {
		se, ok := s.cfg.Runner.(runner.SigninExecer)
		if !ok {
			return nil, runner.ErrSigninUnsupported
		}
		sess, err = se.ExecStreamAs(ctx, ref, runner.SigninPersonUser, spec)
	} else {
		sess, err = s.cfg.Runner.ExecStream(ctx, ref, spec)
	}
	if err != nil {
		return nil, err
	}
	if sess.Close != nil {
		defer func() { _ = sess.Close() }()
	}
	if sess.Stdin != nil {
		go func() {
			if len(stdin) > 0 {
				_, _ = sess.Stdin.Write(stdin)
			}
			_ = sess.Stdin.Close()
		}()
	}
	errOut := make(chan []byte, 1)
	go func() {
		var b []byte
		if sess.Stderr != nil {
			b, _ = io.ReadAll(io.LimitReader(sess.Stderr, 4<<10))
			_, _ = io.Copy(io.Discard, sess.Stderr)
		}
		errOut <- b
	}()
	out, _ := io.ReadAll(io.LimitReader(sess.Stdout, int64(maxOut)+1))
	_, _ = io.Copy(io.Discard, sess.Stdout)
	stderr := <-errOut
	code, err := sess.Wait()
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("wardyn-signin %s exited %d: %s", args[0], code, strings.TrimSpace(string(stderr)))
	}
	if len(out) > maxOut {
		return nil, fmt.Errorf("wardyn-signin %s: output over %d bytes", args[0], maxOut)
	}
	return out, nil
}

// handleInternalDesktopSigninCheck answers the run proxy's sign-in lane: 204
// when the credential is the run's open, unexpired sign-in's, 403 otherwise.
func (s *Server) handleInternalDesktopSigninCheck(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunClaims, "missing run claims")
		return
	}
	st, on := s.desktopSigninStore()
	var body struct {
		Credential string `json:"credential"`
	}
	if !on || json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body) != nil || body.Credential == "" {
		writeErrorReason(w, http.StatusForbidden, reasonDesktopSigninCredentialRefused, "not an open sign-in's credential")
		return
	}
	sum := sha256.Sum256([]byte(body.Credential))
	open, err := st.DesktopSigninCredentialOpen(r.Context(), claims.RunID, sum[:], s.cfg.Now())
	if err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopSigninUnavailable, "the run's sign-in could not be read")
		return
	}
	if !open {
		writeErrorReason(w, http.StatusForbidden, reasonDesktopSigninCredentialRefused, "not an open sign-in's credential")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sweepDesktopSignins ends every sign-in whose run ended, whose window
// passed, or whose end could not finish before. One pass of the reconciler.
func (s *Server) sweepDesktopSignins(ctx context.Context) error {
	st, on := s.desktopSigninStore()
	if !on {
		return nil
	}
	ctx, end, ok := s.beginLeaderSweep(ctx)
	if !ok {
		return nil
	}
	defer end()
	open, err := st.ListOpenDesktopSignins(ctx)
	if err != nil {
		return err
	}
	now := s.cfg.Now()
	for _, si := range open {
		run, err := s.cfg.Store.GetRun(ctx, si.RunID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil || run.State != types.RunRunning || run.SandboxRef == "" || runIsKept(run) {
			_ = s.closeDesktopSignin(ctx, st, run, si, signinEndRunEnded, types.ActorSystem, "wardynd", false)
			continue
		}
		reason := si.EndRequested
		if reason == "" && !now.Before(si.ExpiresAt) {
			reason = signinEndExpired
		}
		if reason != "" {
			_ = s.finishDesktopSignin(ctx, run, si, reason, types.ActorSystem, "wardynd")
		}
	}
	return nil
}

// inDesktopSignin reports whether the run has an open sign-in, reading the
// set once per sweep into *cache. An unreadable set counts every run in one.
func (s *Server) inDesktopSignin(ctx context.Context, cache *map[uuid.UUID]bool, runID uuid.UUID) bool {
	st, on := s.desktopSigninStore()
	if !on {
		return false
	}
	if *cache == nil {
		m, err := st.RunsWithOpenDesktopSignin(ctx)
		if err != nil {
			slog.WarnContext(ctx, "wardynd: open desktop sign-ins unreadable; no run is frozen this pass", "err", err)
			return true
		}
		*cache = m
	}
	return (*cache)[runID]
}
