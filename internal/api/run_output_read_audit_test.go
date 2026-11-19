// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An admin's read of another person's terminal text through GET /runs/{id}/output (a pane snapshot, or output
// recovered from the recording) is audited to that run as a replay of its recording is; the owner's own read,
// and the command output of the same run, write none.
func TestRunOutput_OperatorReadOfAnotherPersonsTerminalTextIsAudited(t *testing.T) {
	const person = "sub-person"
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	own := ssoSession(t, person, "person@corp.example", oidc.RoleUser)
	reads := func(srv *Server) []types.AuditEvent {
		var rows []types.AuditEvent
		for _, ev := range srv.cfg.Audit.(*recRecorder).snapshot() {
			if ev.Action == auditRunOutputRead {
				rows = append(rows, ev)
			}
		}
		return rows
	}
	for _, source := range []string{paneSnapshotSource, recordingOutputSource} {
		t.Run(source, func(t *testing.T) {
			srv, ast, mem, _, _ := newDelegatedOutputServer(t, person)
			id := seedOutputRun(ast, mem, person, source, "terminal text\n")
			path := "/api/v1/runs/" + id.String() + "/output"

			if w := doSSO(t, srv, http.MethodGet, path, own, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "terminal text") {
				t.Fatalf("the owner's read: %d %s, want 200 with the text", w.Code, w.Body)
			}
			if rows := reads(srv); len(rows) != 0 {
				t.Fatalf("the owner's own read wrote %+v", rows)
			}

			if w := doSSO(t, srv, http.MethodGet, path, admin, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "terminal text") {
				t.Fatalf("the admin's read: %d %s, want 200 with the text", w.Code, w.Body)
			}
			rows := reads(srv)
			if len(rows) != 1 || rows[0].RunID == nil || *rows[0].RunID != id || rows[0].Actor != "sub-admin" ||
				rows[0].Target != id.String() || rows[0].Outcome != "success" ||
				!strings.Contains(string(rows[0].Data), `"source":"`+source+`"`) || strings.Contains(string(rows[0].Data), "terminal text") {
				t.Fatalf("run.output.read rows = %+v, want one success row by sub-admin on the run naming the source and none of the text", rows)
			}
		})
	}
	t.Run("command output of another person's run writes none", func(t *testing.T) {
		srv, ast, mem, _, _ := newDelegatedOutputServer(t, person)
		id := seedOutputRun(ast, mem, person, "stdout", "command text\n")
		if w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/output", admin, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "command text") {
			t.Fatalf("the admin's read: %d %s, want 200 with the text", w.Code, w.Body)
		}
		if rows := reads(srv); len(rows) != 0 {
			t.Fatalf("a command-output read wrote %+v", rows)
		}
	})
}
