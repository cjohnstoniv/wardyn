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

// The reasons a support connection's re-check gives. Both are non-transient, so the probe closes the connection.
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
// connection's ctx comes back unchanged.
func withSupportDoors(ctx context.Context, perms *ssh.Permissions) context.Context {
	grant := sshSupportGrant(perms)
	if grant == uuid.Nil {
		return ctx
	}
	doors := strings.Split(perms.Extensions[sshExtSupportDoors], ",")
	return context.WithValue(ctx, sshSupportCtxKey{}, sshSupportEntry{grant: grant, doors: doors})
}

// sshSupportOf is the support entry ctx carries, if its connection is a helper's.
func sshSupportOf(ctx context.Context) (sshSupportEntry, bool) {
	e, ok := ctx.Value(sshSupportCtxKey{}).(sshSupportEntry)
	return e, ok
}

// sshSupportDoorRefused reports whether ctx's connection is a helper's and its grant does not tick the door for k.
func sshSupportDoorRefused(ctx context.Context, k sshChannelKind) bool {
	e, ok := sshSupportOf(ctx)
	return ok && !slices.Contains(e.doors, sshSupportDoorOfKind(k))
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
			"channel_type": channelType, "reason": string(authz.ReasonSupportDoorNotGranted), "support_grant": e.grant,
		})))
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

// sshSupportStrict makes a failed store read on a helper's connection a reason that closes it: an owner's idle
// connection survives a store blip (sshReasonTransient), a helper's does not, so a revoke or an expiry never
// outlives a blip by more than a tick.
func sshSupportStrict(perms *ssh.Permissions, reason string) string {
	if reason != "" && sshReasonTransient(reason) && sshSupportGrant(perms) != uuid.Nil {
		return sshSupportUnverified
	}
	return reason
}
