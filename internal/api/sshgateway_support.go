// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// SSH for a support helper (support_entry.go). A helper's key is an admin key on a person's run, admitted by
// sshAuth's support arm when the run's owner ticked an SSH door. The connection then carries the grant and the
// ticked doors in its permissions; each channel kind is judged against its own door, and sshCurrentKey asks
// supportMayEnter again on every tick, so a revoke, an expiry or a failed read ends the connection.

const (
	sshExtSupportGrant = "support_grant" // the grant id; present only on a support connection
	sshExtSupportDoors = "support_doors" // the doors the owner ticked, comma-joined
	sshExtOverride     = "override"      // "true" on an admin key's entry to an operator-owned run
)

// The reasons a support connection's re-check gives: an ended grant closes it at the next tick; a grant that could
// not be read rides the session budget first (sshRecheckUnread).
const (
	sshSupportEnded      = "Support grant ended"
	sshSupportUnverified = "Support grant could not be verified"
)

// sshSupportDoorOfKind is the door the owner must tick for a channel kind. A kind with no door answers "", which
// no grant holds, so a kind added later is refused to a helper until it is mapped.
func sshSupportDoorOfKind(k sshChannelKind) string {
	switch k {
	case sshKindShell:
		return string(supportDoorTerminal)
	case sshKindExec:
		return string(supportDoorCommands)
	case sshKindSFTP, sshKindSync:
		return string(supportDoorFiles)
	case sshKindForward:
		return string(supportDoorForward)
	}
	return ""
}

// sshSupportEntry is what a support connection carries from its auth to every channel.
type sshSupportEntry struct {
	grant uuid.UUID
	doors []string
}

type sshSupportCtxKey struct{}

// sshMarkEntry records on a connection's permissions how it got in: an admin key's override of an operator-owned
// run (grant is uuid.Nil), or a support grant and the doors it ticks.
func sshMarkEntry(ext map[string]string, override bool, grant uuid.UUID, doors []string) {
	if grant != uuid.Nil {
		ext[sshExtSupportGrant] = grant.String()
		ext[sshExtSupportDoors] = strings.Join(doors, ",")
		return
	}
	if override {
		ext[sshExtOverride] = "true"
	}
}

// sshSupportGrant is the grant a connection's permissions name, uuid.Nil for an owner or an override.
func sshSupportGrant(perms *ssh.Permissions) uuid.UUID {
	id, err := uuid.Parse(perms.Extensions[sshExtSupportGrant])
	if err != nil {
		return uuid.Nil
	}
	return id
}

// sshSupportGrantUnreadable reports a support-grant extension that is present and not a grant id, the nil id
// included. Such a connection is a helper's, never an owner's: sshSupportGrant reads it as no grant, so a caller
// that did not ask here would take it for the run's owner.
func sshSupportGrantUnreadable(perms *ssh.Permissions) bool {
	raw, ok := perms.Extensions[sshExtSupportGrant]
	if !ok {
		return false
	}
	id, err := uuid.Parse(raw)
	return err != nil || id == uuid.Nil
}

// sshAuthData is the ssh.authenticate success row's data: override for an admin key's operator-owned entry,
// support_grant and entry for a helper's, nothing for the owner.
func sshAuthData(ext map[string]string) json.RawMessage {
	if ext[sshExtSupportGrant] != "" {
		return mustJSON(map[string]any{"support_grant": ext[sshExtSupportGrant], "entry": "support"})
	}
	if ext[sshExtOverride] == "true" {
		return mustJSON(map[string]any{"override": true})
	}
	return nil
}

// withSupportDoors puts a support connection's grant and doors on ctx for its channels. Any other
// connection's ctx comes back unchanged; one whose grant extension cannot be read gets no doors.
func withSupportDoors(ctx context.Context, perms *ssh.Permissions) context.Context {
	grant := sshSupportGrant(perms)
	if grant == uuid.Nil {
		if sshSupportGrantUnreadable(perms) {
			// No doors: every channel is refused until the re-check closes the connection.
			return context.WithValue(ctx, sshSupportCtxKey{}, sshSupportEntry{})
		}
		return ctx
	}
	var doors []string // an empty extension is no doors, not the one door named ""
	if joined := perms.Extensions[sshExtSupportDoors]; joined != "" {
		doors = strings.Split(joined, ",")
	}
	return context.WithValue(ctx, sshSupportCtxKey{}, sshSupportEntry{grant: grant, doors: doors})
}

// sshSupportOf is the support entry ctx carries, if its connection is a helper's.
func sshSupportOf(ctx context.Context) (sshSupportEntry, bool) {
	e, ok := ctx.Value(sshSupportCtxKey{}).(sshSupportEntry)
	return e, ok
}

// sshSupportDoorRefused reports whether ctx's connection is a helper's and its grant does not tick the door for k.
// A kind with no door is refused outright, whatever the grant holds.
func sshSupportDoorRefused(ctx context.Context, k sshChannelKind) bool {
	e, ok := sshSupportOf(ctx)
	if !ok {
		return false
	}
	door := sshSupportDoorOfKind(k)
	return door == "" || !slices.Contains(e.doors, door)
}

// sshSupportOffMsg is the one sentence a channel the owner did not allow gets.
func sshSupportOffMsg(k sshChannelKind) string {
	return "wardyn: the run's owner did not allow " + sshKindWord(k) + " in this support session"
}

// sshAuditSupportDoorOff writes the ssh.channel.reject row for a channel the owner's ticks left out, as
// sshAuditChannelOff does for the switch.
func (s *Server) sshAuditSupportDoorOff(ctx context.Context, runID uuid.UUID, principal, channelType string) {
	e, _ := sshSupportOf(ctx)
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.channel.reject",
		runID.String(), "failure", mustJSON(map[string]any{
			"channel_type": channelType, "reason": string(authz.ReasonSupportDoorNotGranted), "support_grant": e.grant, "entry": "support",
		})))
}

// sshRejectData is the data of a re-check refusal's ssh.channel.reject row: the channel and the reason, and on a
// helper's connection the grant and entry support, as the other helper rows carry them.
func sshRejectData(ctx context.Context, channelType, reason string) json.RawMessage {
	data := map[string]any{"channel_type": channelType, "reason": reason}
	if e, ok := sshSupportOf(ctx); ok {
		data["support_grant"], data["entry"] = e.grant, "support"
	}
	return mustJSON(data)
}

// sshRejectMessage is what a refused channel's client reads: the key sentence, or on a helper's connection whose
// grant ended or could not be read, the support sentence.
func sshRejectMessage(reason string) string {
	switch reason {
	case sshSupportEnded:
		return "the support session ended"
	case sshSupportUnverified:
		return "the support session could not be verified"
	}
	return "SSH key is no longer authorized"
}

// sshSupportReason is the connection's re-check reason for supportMayEnter's verdict: "" while the grant stands.
func sshSupportReason(v supportVerdict) string {
	switch v {
	case supportOK:
		return ""
	case supportUnavailable:
		return sshSupportUnverified
	}
	return sshSupportEnded
}

// sshRecheckUnread reports whether a connection's re-check reason is a read that failed, so the connection rides
// the session budget rather than closing at once: a key, run or cutoff lookup, or a helper's grant the store could
// not read. A grant id the connection's own permissions cannot parse is no grant, and closes it.
func sshRecheckUnread(perms *ssh.Permissions, reason string) bool {
	return sshReasonTransient(reason) || (reason == sshSupportUnverified && !sshSupportGrantUnreadable(perms))
}
