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
// the symbols an owner edits. Enforced says whether a door calls the predicate
// yet, and TestBackgroundDoorsAreHonest holds that column to the code, so the
// table cannot claim a denial that is not there.

// backgroundDoor is one way a person or tool enters a running sandbox.
type backgroundDoor struct {
	// Door is the surface as the refusal names it: "no {Door}".
	Door string
	// Owner is the lane that implements the denial at this door.
	Owner string
	// Symbols are the functions the owner edits. A door with no surface yet has none.
	Symbols []string
	// Enforced is true once the door asks backgroundOnlyRefusal.
	Enforced bool
}

// backgroundDoors lists every interactive door. `wardyn run attach` and
// `wardyn ssh`, the SDK and the console reach the same routes and the same SSH
// gateway, so a direct API or CLI call needs no door of its own: denying here
// denies there.
var backgroundDoors = []backgroundDoor{
	{Door: "terminal", Owner: "H4 (attach relay), H5", Symbols: []string{"handleAttachTicket", "handleAttachWS", "handleAttachTakeover"}},
	{Door: "interactive exec", Owner: "H4", Symbols: []string{"establishExec"}},
	{Door: "SSH access", Owner: "SSH gateway owner", Symbols: []string{"sshAuth", "handleSSHSessionChannel", "handleSSHDirectTCPIP"}},
	{Door: "web application gateway", Owner: "UI gateway owner", Symbols: []string{"handleUIEnter", "handleUIRelay", "handleUIBind"}},
	{Door: "desktop", Owner: "D-117 (no desktop surface exists yet; its first owner adds the symbols here)"},
	{Door: "interactive session", Owner: "H3 (restored and reconnected sessions)", Symbols: []string{"handleReviveRun", "reviveRunProxy"}},
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
