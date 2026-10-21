// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The background denial contract. A run launched as a Background task has no
// interactive terminal, SSH, interactive exec or attach, web application gateway
// or graphical desktop, whatever the caller sends later and whoever the caller is
// (a super admin included: this is the run's shape, not a permission). Logs,
// audit, status and the authorised lifecycle controls (kill, stop, approve) stay.
//
// The stored field is agent_runs.experience (types.AgentRun.Experience), written
// once at create from the request. The single predicate is
// types.AgentRun.BackgroundOnly, and every door asks it through
// backgroundOnlyRefusal. A door that reads the request, or the legacy Interactive
// flag, instead is the bug this contract exists to prevent: Interactive is an
// execution detail that a no-task request even coerces to true.
//
// A run with no stored experience (an older client, or before 0.9) is not
// background-only: it keeps the behaviour every door has today. That is a
// compatibility choice made in exactly one place, BackgroundOnly.
//
// Each door is implemented by its owning lane; backgroundDoors is the list, with
// the symbols an owner edits and the /runs/{id} routes that lead to it. Enforced
// says whether the door asks the predicate yet, and TestBackgroundDoorsAreHonest
// holds that column to the code: a door is Enforced only when EVERY listed symbol
// calls backgroundOnlyRefusal or refuseBackgroundOnly, so a half-wired door cannot
// claim a denial that is not there. TestRunRoutesAreClassified fails when a new
// /runs/{id} route is registered that is neither a door's route nor listed in
// passiveRunRoutes, so a new interactive route cannot slip in unlisted.
//
// Revive is not a door. It gives a lost run a new proxy and restarts a rebooted
// run's agent, which is lifecycle recovery, and a background task keeps its
// authorised lifecycle controls. A revived Background run still refuses every
// door below, because the stored experience survives the revive.

// backgroundDoor is one way a person or tool enters a running sandbox.
type backgroundDoor struct {
	// Door is the surface as the refusal names it: "no {Door}".
	Door string
	// Owner is the lane that implements the denial at this door.
	Owner string
	// Symbols are the functions the owner edits. A door with no surface yet has none.
	Symbols []string
	// Routes are the /runs/{id} routes that open this door, as "METHOD /path".
	// SSH and the UI gateway are not under /runs/{id}; their Symbols are the guard.
	Routes []string
	// Enforced is true once every symbol asks backgroundOnlyRefusal.
	Enforced bool
}

// backgroundDoors lists every interactive door. `wardyn attach` and
// `wardyn ssh`, the SDK and the console reach the same routes and the same SSH
// gateway, so a direct API or CLI call needs no door of its own: denying here
// denies there.
var backgroundDoors = []backgroundDoor{
	// admitRunEntry, not handleAttachTicket: the ticket mint and a "Needs you" takeover's three
	// verbs (loadTakeover, under /approvals/{id}) enter only through it, and it asks the predicate.
	{Door: "terminal", Owner: "H4 (attach relay), H5", Symbols: []string{"admitRunEntry", "handleAttachWS", "handleAttachTakeover"},
		Routes: []string{"POST /runs/{id}/attach-ticket", "POST /runs/{id}/attach/ticket", "GET /runs/{id}/attach", "POST /runs/{id}/attach/takeover"}, Enforced: true},
	{Door: "interactive exec", Owner: "C-mode-doors", Symbols: []string{"establishExec"}, Enforced: true},
	// sshFreshRun, not the channel handlers: those take a run id and hold no run,
	// while sshFreshRun is the one fresh read every channel bridge (shell, exec,
	// sftp, wardyn-sync, direct-tcpip) makes before it execs.
	{Door: "SSH access", Owner: "C-mode-doors", Symbols: []string{"sshAuth", "sshFreshRun"}, Enforced: true},
	// handleUIEnter only parses and delegates to uiEnterCommon, and handleUIBind
	// sets a binding cookie without reading any run, so neither can ask honestly.
	{Door: "web application gateway", Owner: "C-mode-doors", Symbols: []string{"uiEnterCommon", "uiSessionStillAuthorized"}, Enforced: true},
	// The Files panel moves files in and out of the graphical desktop, so it is the desktop door.
	// The desktop app itself is reached through the web application gateway, which refuses above.
	{Door: "desktop", Owner: "CTL-A (desktop Files panel)", Symbols: []string{"desktopRun"},
		Routes: []string{"GET /runs/{id}/desktop/files", "PUT /runs/{id}/desktop/files/{name}", "GET /runs/{id}/desktop/files/{name}"}, Enforced: true},
}

// passiveRunRoutes are the /runs/{id} routes that are not interactive doors: they
// read, or change the run's lifecycle, and stay available to a background run.
var passiveRunRoutes = map[string]string{
	"GET /runs/{id}":                     "read the run",
	"GET /runs/{id}/ado-tokens":          "read",
	"GET /runs/{id}/attach-holder":       "read who holds the terminal; opens nothing",
	"GET /runs/{id}/attach/holder":       "read who holds the terminal; opens nothing",
	"GET /runs/{id}/containment":         "read the containment hold; opens nothing",
	"GET /runs/{id}/events":              "read",
	"GET /runs/{id}/launch-intent":       "read the setup the run was launched with; opens nothing",
	"GET /runs/{id}/files":               "read",
	"GET /runs/{id}/grants":              "read",
	"GET /runs/{id}/output":              "logs and output stay available",
	"GET /runs/{id}/policy":              "read",
	"GET /runs/{id}/resources":           "read",
	"GET /runs/{id}/sign-in":             "read",
	"MOUNT /runs/{id}/recording":         "replay of a finished recording",
	"PATCH /runs/{id}":                   "lifecycle: end and wait",
	"PATCH /runs/{id}/title":             "lifecycle: title",
	"POST /runs/{id}/kill":               "lifecycle: kill",
	"POST /runs/{id}/node-record":        "a security operator records why a held run's node is gone; opens nothing",
	"POST /runs/{id}/profile":            "synthesise a profile from audit",
	"POST /runs/{id}/profile/synthesize": "synthesise a profile from audit",
	"POST /runs/{id}/resume":             "lifecycle: resume a paused run",
	"POST /runs/{id}/revive":             "lifecycle recovery; the revived run keeps its stored experience, so every door above still refuses it",
}

// backgroundOnlyRefusal is the one call every interactive door makes: nil when
// the run may be entered, the 409 to answer with when it is background-only. A
// door audits its own refusal under the action it already uses.
func backgroundOnlyRefusal(run types.AgentRun, door string) *runRefusal {
	if !run.BackgroundOnly() {
		return nil
	}
	return runError(http.StatusConflict, reasonRunBackgroundOnly, runBackgroundOnlyMsg(door))
}

// refuseBackgroundOnly is backgroundOnlyRefusal for an HTTP door: true when it
// has answered.
func (s *Server) refuseBackgroundOnly(w http.ResponseWriter, r *http.Request, run types.AgentRun, door string) bool {
	return backgroundOnlyRefusal(run, door).write(s, w, r)
}
