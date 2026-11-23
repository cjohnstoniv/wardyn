// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The live streams' gates against their twins. GET /api/v1/walls/stream is gated as GET /api/v1/governance is
// (requireUnitScope), and GET /api/v1/runs/{id}/walls/stream as GET /api/v1/runs/{id}/policy is, plus a unit admin
// of the run's unit (wallsRunReadable; walls_stream_authz_pg_test.go pins that arm). Each expected answer is read
// from a probe of the twin in the same test: a refusal there is the stream's, byte for byte, and a caller the twin
// lets in gets past the stream's gate. The assertions never name the stream's own answer, so they hold the same
// whatever serves past the gate.

// wallsStreamArm is one caller, on the server that knows them, and the run they probe.
type wallsStreamArm struct {
	who    string
	srv    *Server
	run    uuid.UUID
	cookie *http.Cookie
	bearer string
}

// TestWallsStreamGatesFollowTheirTwins: on both live streams, a super admin, a security admin, the run's owner, a
// member, a portal's delegated token and a service identity's token each get their twin's refusal, or pass the gate
// where the twin lets them in.
func TestWallsStreamGatesFollowTheirTwins(t *testing.T) {
	const owner = "sub-walls-stream-owner"
	srv, ast, _, _ := newAuthzMatrixServer(t)
	run := wallsRoutesSeedRun(ast, owner)
	delegated, _ := seedDelegation(t, ast.fakeDelegateStore, owner)
	svc, svcAST, svcToken := wallsRoutesServiceServer(t)
	svcRun := wallsRoutesSeedRun(svcAST, types.ServicePrincipalPrefix+wallsRoutesServiceName)
	arms := []wallsStreamArm{
		{"super admin", srv, run, ssoSession(t, "sub-walls-stream-admin", "walls-stream-admin@corp.example", oidc.RoleAdmin), ""},
		{"security admin", srv, run, ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin), ""},
		{"owner", srv, run, ssoSession(t, owner, "walls-stream-owner@corp.example", oidc.RoleUser), ""},
		{"member", srv, run, ssoSession(t, "sub-walls-stream-member", "walls-stream-member@corp.example", oidc.RoleUser), ""},
		{"portal token", srv, run, nil, delegated},
		{"service token", svc, svcRun, nil, svcToken},
	}
	for _, arm := range arms {
		t.Run(arm.who, func(t *testing.T) {
			h := arm.srv.Handler()
			runPath := "/api/v1/runs/" + arm.run.String()
			wallsStreamFollowsTwin(t, "the run's live stream",
				wallsRoutesServe(t, h, runPath+"/policy", arm.cookie, arm.bearer),
				wallsRoutesServe(t, h, runPath+"/walls/stream", arm.cookie, arm.bearer))
			wallsStreamFollowsTwin(t, "the topology's live stream",
				wallsRoutesServe(t, h, "/api/v1/governance", arm.cookie, arm.bearer),
				wallsRoutesServe(t, h, "/api/v1/walls/stream", arm.cookie, arm.bearer))
		})
	}
}

// wallsStreamFollowsTwin fails t unless stream answers as twin does at the gate: a twin's refusal (401, 403 or 404)
// is the stream's, status and body byte for byte, and a caller the twin lets in is not refused by the stream's gate.
func wallsStreamFollowsTwin(t *testing.T, which string, twin, stream *httptest.ResponseRecorder) {
	t.Helper()
	if wallsStreamRefused(twin.Code) {
		wallsStreamSameAnswer(t, which, stream, twin)
		return
	}
	if wallsStreamRefused(stream.Code) {
		t.Errorf("%s: %d %s, but its twin lets this caller in (%d)", which, stream.Code, stream.Body.String(), twin.Code)
	}
}

// wallsStreamRefused is whether code is a gate's refusal.
func wallsStreamRefused(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusNotFound
}

// wallsStreamSameAnswer fails t unless got carries want's status, Content-Type and body byte for byte: a refusal
// that differs from the one it stands for by a byte tells the caller something the other does not.
func wallsStreamSameAnswer(t *testing.T, who string, got, want *httptest.ResponseRecorder) {
	t.Helper()
	if got.Code != want.Code || got.Header().Get("Content-Type") != want.Header().Get("Content-Type") || got.Body.String() != want.Body.String() {
		t.Errorf("%s: answered %d %q %q, want %d %q %q byte for byte", who,
			got.Code, got.Header().Get("Content-Type"), got.Body.String(), want.Code, want.Header().Get("Content-Type"), want.Body.String())
	}
}

// wallsStreamAuthzRecorder ends the request once the handler has begun a 2xx answer, so a live stream returns at its
// first write or flush instead of waiting for events; a refusal ends on its own, and its audit row is written under
// the request's live context. It answers the deadline calls a stream makes through http.ResponseController.
type wallsStreamAuthzRecorder struct {
	*httptest.ResponseRecorder
	answered context.CancelFunc
}

func (w wallsStreamAuthzRecorder) Write(b []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(b)
	w.endIfStreaming()
	return n, err
}

func (w wallsStreamAuthzRecorder) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func (w wallsStreamAuthzRecorder) Flush() {
	w.ResponseRecorder.Flush()
	w.endIfStreaming()
}

func (w wallsStreamAuthzRecorder) endIfStreaming() {
	if w.Code >= 200 && w.Code < 300 {
		w.answered()
	}
}

func (wallsStreamAuthzRecorder) SetReadDeadline(time.Time) error  { return nil }
func (wallsStreamAuthzRecorder) SetWriteDeadline(time.Time) error { return nil }
