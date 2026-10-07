// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GET /runs/{id}/sign-in finds a waiting AWS sign-in after the browser tab that
// started it was lost. Nothing server-side stores the device code (the console
// scrapes it off the attach stream), so this reads the sign-in pane once,
// bounded, and answers only what a strict parse of the latest attempt finds.
//
// The pane is sandbox-controlled text: none of it reaches an error, a log line
// or an audit row, and nothing is stored. Only the run's owner may read the
// answer (getRunForEntry): approving the page binds the approver's cloud
// identity to the owner's stored session.

const (
	signInStateWaiting    = "waiting"
	signInStateNotWaiting = "not_waiting"
	// signInPaneMaxBytes caps the capture before it is parsed. The pane is
	// cleared when the sign-in starts and holds a few lines per attempt, so a
	// capture over the cap is refused as unreadable rather than parsed in part.
	signInPaneMaxBytes = 64 << 10
	// signInCaptureAction is the row a stored capture writes under its sign-in
	// run's id (handleUploadSSOToken). The row, not the stored blob, is the
	// evidence: every secret-store read writes a secret.read row of its own.
	signInCaptureAction = "harness.credential.capture"
)

// runSignInResponse is the whole answer: the URL and code only when waiting.
type runSignInResponse struct {
	State           string `json:"state"`
	VerificationURL string `json:"verification_url,omitempty"`
	UserCode        string `json:"user_code,omitempty"`
}

func (s *Server) handleRunSignIn(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunForEntry(w, r, id)
	if !ok {
		return
	}
	if run.Task != harnessLoginTask || run.Agent != awsSSOAgent {
		writeErrorReason(w, http.StatusConflict, reasonRunSignInNotAWS,
			"this run is not an AWS sign-in: only an AWS sign-in run waits on a device code")
		return
	}
	notWaiting := runSignInResponse{State: signInStateNotWaiting}
	if run.State != types.RunRunning || run.SandboxRef == "" {
		writeJSON(w, http.StatusOK, notWaiting)
		return
	}
	// The pane is a hint and the capture is the evidence: once this run stored
	// its capture the pane may still read as waiting for the kill grace.
	captured, err := s.signInCaptured(r.Context(), run.ID)
	if err != nil {
		s.refuseSignInUnreadable(w, r, run.ID, "capture_unknown")
		return
	}
	if captured {
		writeJSON(w, http.StatusOK, notWaiting)
		return
	}
	pane, reason := s.readSignInPane(r.Context(), run)
	switch reason {
	case "":
	case "sandbox_gone":
		writeJSON(w, http.StatusOK, notWaiting)
		return
	default:
		s.refuseSignInUnreadable(w, r, run.ID, reason)
		return
	}
	if link, code, ok := parseSignInPane(pane); ok {
		writeJSON(w, http.StatusOK, runSignInResponse{State: signInStateWaiting, VerificationURL: link, UserCode: code})
		return
	}
	writeJSON(w, http.StatusOK, notWaiting)
}

// refuseSignInUnreadable logs why (a fixed word, never pane text) and answers 503.
func (s *Server) refuseSignInUnreadable(w http.ResponseWriter, r *http.Request, runID uuid.UUID, why string) {
	slog.WarnContext(r.Context(), "wardynd: could not read a sign-in run's pane",
		slog.String("run_id", runID.String()), slog.String("reason", why))
	writeErrorReason(w, http.StatusServiceUnavailable, reasonRunSignInUnreadable,
		"could not check whether this sign-in is waiting: read it again shortly, or open its terminal")
}

// signInCaptured reports whether this run stored its capture, from the
// capture's own audit row.
func (s *Server) signInCaptured(ctx context.Context, runID uuid.UUID) (bool, error) {
	m, ok := s.cfg.Store.(store.RunAuditMatcher)
	if !ok {
		return false, errors.New("the store cannot probe audit rows")
	}
	return m.HasRunAuditEvent(ctx, runID, store.AuditFilter{Action: signInCaptureAction, Outcome: "success"})
}

// readSignInPane captures the pane once, on capturePane's exec, drain and
// close pattern and its time bound, and never its writer: a sign-in run is
// unrecordable, so the bytes are parsed in memory and dropped. reason is a
// fixed word, never pane content.
func (s *Server) readSignInPane(ctx context.Context, run types.AgentRun) (pane []byte, reason string) {
	if s.cfg.Runner == nil {
		return nil, "no_runner"
	}
	readCtx, cancel := context.WithTimeout(ctx, s.paneSnapshotBound())
	defer cancel()
	sess, err := s.cfg.Runner.ExecStream(readCtx, run.SandboxRef, runner.ExecSpec{Argv: paneSnapshotArgv})
	if sess != nil && sess.Close != nil {
		defer func() { _ = sess.Close() }() // tears down only this exec, never the sandbox
	}
	switch {
	case errors.Is(err, runner.ErrSandboxGone):
		return nil, "sandbox_gone"
	case err != nil, sess == nil, sess.Stdout == nil:
		return nil, "exec_failed"
	}
	if sess.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	}
	type exit struct {
		out  []byte
		code int
		err  error
	}
	done := make(chan exit, 1)
	go func() {
		out, err := io.ReadAll(io.LimitReader(sess.Stdout, signInPaneMaxBytes+1))
		if err != nil || len(out) > signInPaneMaxBytes || sess.Wait == nil {
			done <- exit{out: out, err: err}
			return
		}
		code, werr := sess.Wait()
		done <- exit{out, code, werr}
	}()
	select {
	case x := <-done:
		switch {
		case x.err != nil:
			return nil, "exec_failed"
		case len(x.out) > signInPaneMaxBytes:
			return nil, "overflow"
		case x.code != 0:
			return nil, "exit_nonzero"
		}
		return x.out, ""
	case <-readCtx.Done():
		if errors.Is(readCtx.Err(), context.Canceled) {
			return nil, "cancelled"
		}
		return nil, "timeout"
	}
}

var (
	// The hosts and the URL character class are extractDeviceVerificationUrl's
	// (login-pty-extract.ts): the IAM Identity Center device endpoint and an
	// access portal, and no whitespace, quote, angle bracket or control byte.
	signInURLRe = regexp.MustCompile(`(?i)https://(?:device\.sso\.[a-z0-9-]+\.amazonaws\.com|[a-z0-9-]+\.awsapps\.com)/[^\s'"<>\x00-\x1f\x7f]*`)
	// signInCodeRe is deviceCodeOf's (signin-progress.tsx); signInCodeShape is
	// the strict half: groups of letters and digits joined by dashes.
	signInCodeRe    = regexp.MustCompile(`[?&]user_code=([^&#\s]+)`)
	signInCodeShape = regexp.MustCompile(`^[A-Za-z0-9]{1,16}(?:-[A-Za-z0-9]{1,16}){0,3}$`)
)

// signInEndLines end an attempt: a line after the attempt's code that names its
// completion or failure means nobody is waiting on that code. The AWS CLI's own
// success line, wardyn-aws-sso's success and failure markers, and
// signin-pane.sh's DONE and FAILED lines (login-hint.sh); each prefix is pinned
// against its source by TestSignInPane_EndLinesMatchTheirSources.
var signInEndLines = []string{
	"Successfully logged into Start URL",
	"wardyn: aws sso credential captured",
	"wardyn: aws sso credential rejected:",
	"wardyn: sign-in command finished",
	"wardyn: sign-in did not complete",
}

// signInURLMaxLen bounds what a waiting answer may carry.
const signInURLMaxLen = 512

// parseSignInPane returns the latest attempt's pre-filled verification URL and
// its code, when nothing after that URL ends the attempt. The latest wins, so
// an earlier attempt's code is never returned, except over a truncated repaint
// of the same attempt (a strict prefix of the URL already seen), which a tmux
// redraw can leave behind.
func parseSignInPane(pane []byte) (link, code string, ok bool) {
	text := string(pane)
	best, bestEnd := "", -1
	for _, loc := range signInURLRe.FindAllStringIndex(text, -1) {
		if loc[1] >= len(text) {
			continue // still printing
		}
		u := strings.TrimRight(text[loc[0]:loc[1]], ".,)")
		if !strings.Contains(u, "user_code=") || (len(u) < len(best) && strings.HasPrefix(best, u)) {
			continue
		}
		best, bestEnd = u, loc[1]
	}
	if best == "" || len(best) > signInURLMaxLen {
		return "", "", false
	}
	after := text[bestEnd:]
	for _, end := range signInEndLines {
		if strings.Contains(after, end) {
			return "", "", false
		}
	}
	m := signInCodeRe.FindStringSubmatch(best)
	if m == nil || !signInCodeShape.MatchString(m[1]) {
		return "", "", false
	}
	if parsed, err := url.Parse(best); err != nil || parsed.Scheme != "https" {
		return "", "", false
	}
	return best, m[1], true
}
