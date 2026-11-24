// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The purposes a ticket may be minted for, and the door each opens. A purpose is stored with the ticket and checked
// at redemption, so a ticket spent at any other door is refused after it is burned.
const (
	ticketPurposeSSHExec       = "ssh-exec"
	ticketPurposeSSHForward    = "ssh-forward"
	ticketPurposeSSHEditor     = "ssh-editor"
	ticketPurposeAttachForward = "attach-forward"
)

// sshTicketKinds is the channel kinds each SSH purpose serves on GET /runs/{id}/ssh.
var sshTicketKinds = map[string][]sshChannelKind{
	ticketPurposeSSHExec:    {sshKindExec},
	ticketPurposeSSHForward: {sshKindForward},
	ticketPurposeSSHEditor:  {sshKindShell, sshKindExec, sshKindSFTP, sshKindForward},
}

// ticketPurposeWords names a purpose in the sentence a refused channel gets.
var ticketPurposeWords = map[string]string{
	ticketPurposeSSHExec: "one command", ticketPurposeSSHForward: "port forwarding", ticketPurposeSSHEditor: "an editor",
}

// ticketScope is what a mint's purpose adds to the ticket: the purpose and its ports.
type ticketScope struct {
	purpose string
	ports   []int
}

// sshTicketPurpose reports whether p is one of the purposes GET /runs/{id}/ssh redeems.
func sshTicketPurpose(p string) bool { _, ok := sshTicketKinds[p]; return ok }

// parseTicketPorts reads a mint's ports query: a comma list of 1 to 16 distinct ports, each decimal digits only
// from 1 to 65535 (attachForwardPort's rule). Anything else is false.
func parseTicketPorts(q string) ([]int, bool) {
	parts := strings.Split(q, ",")
	if q == "" || len(parts) > 16 {
		return nil, false
	}
	out := make([]int, 0, len(parts))
	for _, x := range parts {
		n, ok := attachForwardPort(x)
		if !ok || slices.Contains(out, n) {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// mintScope is the ticket mint's purpose check. It runs before the support arm and any run read, so every answer is
// deployment-, request- or person-shaped and none is a run oracle. A purpose that is neither an SSH purpose nor
// attach-forward keeps today's meaning ("" and "ui" included) and takes no ports. ok=false means the refusal is
// written.
func (s *Server) mintScope(w http.ResponseWriter, r *http.Request, purpose string) (ticketScope, bool) {
	ports := r.URL.Query().Get("ports")
	switch {
	case sshTicketPurpose(purpose):
		return s.sshTicketScope(w, r, purpose, ports)
	case purpose == ticketPurposeAttachForward:
		return s.attachForwardScope(w, r, ports)
	case strings.HasPrefix(purpose, "ssh-"):
		writeErrorReason(w, http.StatusBadRequest, reasonAttachTicketPurposeInvalid, "Purpose must be ssh-exec, ssh-forward, ssh-editor or attach-forward.")
		return ticketScope{}, false
	case ports != "":
		refuseTicketPorts(w)
		return ticketScope{}, false
	}
	return ticketScope{}, true
}

func refuseTicketPorts(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusBadRequest, reasonAttachTicketPortsInvalid, "Ports must be 1 to 16 distinct ports from 1 to 65535, and only with purpose ssh-forward or attach-forward.")
}

// sshTicketScope is mintScope for an SSH purpose: the ports, then a person signed in directly, the gateway, the
// switch, each kind the purpose serves, and the ssh_key capability a key's registration asks.
func (s *Server) sshTicketScope(w http.ResponseWriter, r *http.Request, purpose, q string) (ticketScope, bool) {
	var ports []int
	if purpose == ticketPurposeSSHForward {
		var ok bool
		if ports, ok = parseTicketPorts(q); !ok {
			refuseTicketPorts(w)
			return ticketScope{}, false
		}
	} else if q != "" {
		refuseTicketPorts(w)
		return ticketScope{}, false
	}
	_, delegated := audit.DelegationFrom(r.Context())
	if at, _ := actorFromRequest(r); delegated || at != types.ActorHuman {
		writeErrorReason(w, http.StatusForbidden, reasonSSHTicketUnsupportedCaller, "SSH tickets are for a person signed in directly, not a portal, device or service.")
		return ticketScope{}, false
	}
	if s.sshGatewayHealthz() == nil {
		writeErrorReason(w, http.StatusConflict, reasonSSHGatewayOff, "The SSH gateway is off on this deployment (WARDYN_SSH_LISTEN).")
		return ticketScope{}, false
	}
	if !s.cfg.SSHTickets {
		writeErrorReason(w, http.StatusConflict, reasonSSHTicketsOff, "SSH tickets are off on this deployment (WARDYN_SSH_TICKETS).")
		return ticketScope{}, false
	}
	for _, k := range sshTicketKinds[purpose] {
		if !s.sshChannelOn(k) {
			writeErrorReason(w, http.StatusConflict, reasonSSHTicketChannelOff, "This deployment's SSH gateway does not serve "+sshKindWord(k)+" (WARDYN_SSH_CHANNELS).")
			return ticketScope{}, false
		}
	}
	// Fail closed, as the key door is: a person an ssh_key deny covers gets no SSH ticket either.
	if s.denyUserCapability(w, r, capFeature, featureSSHKey, "runs.ssh_ticket", sshTicketFeatureRefusal) {
		return ticketScope{}, false
	}
	return ticketScope{purpose: purpose, ports: ports}, true
}

// attachForwardScope is mintScope for attach-forward: the ports, the forward door's switch and WARDYN_SSH_CHANNELS,
// and no portal (the forward door refuses a portal's ticket, so none is minted).
func (s *Server) attachForwardScope(w http.ResponseWriter, r *http.Request, q string) (ticketScope, bool) {
	ports, ok := parseTicketPorts(q)
	if !ok {
		refuseTicketPorts(w)
		return ticketScope{}, false
	}
	if !s.attachForwardOn(r.Context()) {
		writeErrorReason(w, http.StatusConflict, reasonRunForwardOff, attachForwardOffMsg)
		return ticketScope{}, false
	}
	if !s.sshChannelOn(sshKindForward) {
		writeErrorReason(w, http.StatusConflict, reasonRunForwardOff, attachForwardChannelOffMsg)
		return ticketScope{}, false
	}
	if _, delegated := audit.DelegationFrom(r.Context()); delegated {
		writeErrorReason(w, http.StatusForbidden, reasonAttachTicketNotYourRun, attachForwardTicketMsg)
		return ticketScope{}, false
	}
	return ticketScope{purpose: ticketPurposeAttachForward, ports: ports}, true
}
