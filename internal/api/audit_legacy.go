// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// legacyAuditActions is the SOLE named table in which a pre-0.8 audit action
// name may appear outside docs/AUDIT-ACTIONS.md's "Renamed in 0.8" appendix,
// the CHANGELOG, migrations and tests (owner ruling, 2026-09-25, #1062): it is
// exempt from the #905 re-sweep and from any widened #1020 guard.
//
// READ-SIDE ONLY. Audit rows are hashed and append-only — a row written under
// the old name is never rewritten — so a reader of PERSISTED history has to
// keep recognising both spellings forever. NEVER pass one of these old names
// to an emitter; canonicalAction only widens what a reader accepts, it never
// changes what gets written.
var legacyAuditActions = map[string]string{
	"run.policy.effective":  "run.policy.resolve",
	"harness.login.started": "harness.login.start",
	"session.recording":     "session.recording.write",
}

// canonicalAction returns the 0.8 name a reader should compare against for a.
// An action already on the 0.8 grammar, or one this table does not know,
// passes through unchanged.
func canonicalAction(a string) string {
	if n, ok := legacyAuditActions[a]; ok {
		return n
	}
	return a
}
