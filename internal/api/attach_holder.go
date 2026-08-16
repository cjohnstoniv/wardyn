// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Attach-session holder registry + audited take-over.
//
// THE PROBLEM THIS EXISTS TO FIX: attach is a SHARED tmux session. handleAttachWS
// (attach.go) opens a fresh Runner.Attach per client against the same persistent
// session, so opening the run page while a `wardyn attach` holds it from a CLI
// means two clients silently compete for one PTY — and neither can observe the
// other. The Redraw button in ui/src/app/components/attach-terminal.tsx exists
// only to clean up the tmux clamp that competition leaves behind.
//
// The honest version: name the holder, admit the second client READ-ONLY, and
// make displacing them an audited act.
package api

import "net/http"

// handleAttachHolder serves GET /api/v1/runs/{id}/attach-holder.
//
// handleAttachTakeover serves POST /api/v1/runs/{id}/attach/takeover.
//
// CONTRACT (lane A3 — fill this in):
//
//   - Gate BOTH: parseIDParam + s.getRunAuthorized (owner-or-admin; a foreign
//     run 404s). Follow handleAttachTicket (attach_ticket.go:113) exactly.
//
//   - Registry: a mutex-guarded map[uuid.UUID]*attachHolder on Server, holding
//     {principal, actorType, since, cols, rows, source, cancel}. Register right
//     after the SUCCESSFUL session.attach audit (attach.go:193); unregister on
//     the SAME defer path that emits session.detach, so a panicking pump can
//     never leave a phantom holder behind.
//
//   - Keep cols/rows CURRENT from the resize control frames the pump already
//     handles — the handshake value goes stale the first time the operator
//     resizes their window, and a stale geometry is worse than none.
//
//   - The SSH lane counts. sshgateway_channels.go:390 emits the same
//     session.attach/detach pair for an SSH-gateway PTY; it must register in
//     THIS registry with source="ssh", or a CLI holder over SSH stays invisible
//     and the browser confidently reports "nobody is attached" while someone is.
//
//   - Second attach while held: admit it READ-ONLY. Stream output, DROP input
//     frames server-side. Never rely on the client to refrain from sending —
//     the client is the thing you do not control.
//
//   - Take-over: audit session.takeover FIRST (actor + previous_holder), then
//     cancel() the previous holder's pump. Order matters: an audit written
//     after a cancel can be lost to the very teardown it describes (see the
//     finishCtx FINDING in attach.go's detach audit for the same trap).
//     Close the displaced socket with a reason the client can read, e.g.
//     "taken over by <principal>". The UI matches on that close reason and must
//     NOT fall into its bounded-reconnect path — two clients reconnecting at
//     each other is exactly the fight this endpoint exists to end.
//
//   - CEILING, document it at the type: the registry is IN-PROCESS. A
//     multi-replica k8s control plane sees only its own replica's holders. Do
//     not let the UI copy claim more than that.
//     ponytail: in-process holder registry, single-daemon truth. Upgrade path
//     is a store row keyed by run id if wardynd ever runs multi-replica.
func (s *Server) handleAttachHolder(w http.ResponseWriter, r *http.Request) {
	// ponytail: honest "nobody known to be holding" until lane A3 lands. The
	// UI treats held=false as today's behaviour (no holder chip), so an
	// unbuilt registry degrades to exactly the current experience.
	writeJSON(w, http.StatusOK, map[string]any{"held": false})
}

func (s *Server) handleAttachTakeover(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "attach take-over is not implemented on this build")
}
