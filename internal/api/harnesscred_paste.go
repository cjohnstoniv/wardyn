// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"strings"
)

// maxHarnessPasteTokenLen bounds a PASTED harness credential. A real
// `claude setup-token` output is well under a kilobyte; this is generous enough
// that no genuine token is refused and tight enough that the value going into
// MaskRegistry.AddGlobal — which holds it for the daemon's whole life and scans
// every PTY, asciicast and decision-log byte against it — cannot be made large
// enough to matter.
const maxHarnessPasteTokenLen = 8 << 10

// DRAFT (M2 canon pending)
const (
	// harnessPasteViaHelperRefusal answers PUT /setup/harness-credential/{p} for
	// a provider whose credential is captured by a helper inside a login sandbox
	// (harnessLogin.captureViaHelper), not pasted. aws is the live case: its
	// reserved secret holds a structured SSO blob, and a pasted {"token":…} both
	// destroys that blob — Bedrock then silently falls through to ~/.aws or
	// static keys — and writes it to the unscoped operator-wide row while every
	// other aws path reads the caller's own namespace.
	harnessPasteViaHelperRefusal = "%s credentials are captured by the containerized login, not pasted — " +
		"start the login from Setup instead; pasting here would overwrite the captured session"
	// harnessPasteEmptyRefusal / harnessPasteTooLongRefusal are the two shape
	// guards the paste door never had. An empty token stored a blob that reads
	// as "connected" and resolves to nothing at inject time; an unbounded one
	// went straight into the process-global mask corpus.
	harnessPasteEmptyRefusal   = "the token is empty"
	harnessPasteTooLongRefusal = "the token is too long (%d bytes, max %d)"
)

// harnessPasteRefusal validates an operator-pasted harness credential and
// returns the 400 sentence, or "" when the paste may proceed.
//
// The first check is the point. `captureViaHelper` is a real field on the
// provider row (harnesscred.go): it says this credential is written to a file
// inside a login sandbox and uploaded by an in-sandbox helper, never printed to
// a PTY and scraped. aws is such a row, and it also has no tokenPrefix, so the
// existing format guard let a paste straight through to
// Secrets.Put(harness-credential-aws) — overwriting the reserved, structured SSO
// blob with {"token":…}. Bedrock then fell through to the ~/.aws mount or static
// keys with no refusal anywhere, and the arbitrary pasted string was added to
// the process-global mask corpus for the life of the daemon, which is the exact
// abuse ssotoken.go's upload path already defends itself against.
//
// The paste door is the only writer refused here. DISCONNECT is untouched: it
// deletes through the caller's own scope (handleHarnessDisconnect resolves the
// per_user namespace), so removing a helper-captured session is a legitimate,
// already-scoped operation — and with paste refused, the unscoped For("") write
// that made the two disagree can no longer happen at all.
func harnessPasteRefusal(hl harnessLogin, token string) string {
	if hl.captureViaHelper {
		return fmt.Sprintf(harnessPasteViaHelperRefusal, hl.provider)
	}
	if token == "" {
		return harnessPasteEmptyRefusal
	}
	if len(token) > maxHarnessPasteTokenLen {
		return fmt.Sprintf(harnessPasteTooLongRefusal, len(token), maxHarnessPasteTokenLen)
	}
	// Format guard (NOT authentication — the token is validated for real on
	// first use, when the proxy injects it and the provider accepts or rejects
	// it). Reject an obvious paste error early with an actionable message.
	// An empty tokenPrefix now means only "this provider has no fixed prefix to
	// guard on"; it is no longer also the aws lane's way in, because that row is
	// refused above.
	if hl.tokenPrefix != "" && !strings.HasPrefix(token, hl.tokenPrefix) {
		return "that does not look like a `claude setup-token` output (expected a token starting with " + hl.tokenPrefix + ")"
	}
	return ""
}
