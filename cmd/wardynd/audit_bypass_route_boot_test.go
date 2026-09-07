// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// PIN for the operator-facing residue of the finding that the audit-bypass
// enumeration was incomplete.
//
// The code half landed: db.AuditDDLProtected counts four routes, and the fourth
// follows role membership like the other three. The BOOT LOG did not follow. It
// took a bare bool and then asserted a cause of its own — the app role "still
// owns audit_events or is a superuser" — and prescribed "connect wardynd as a
// distinct non-owner role". That is the wrong remedy for two of the four routes
// and actively misleading for the fourth: a role that owns nothing, is nobody's
// superuser and holds no TRIGGER privilege, but was GRANTed SET on the
// session_replication_role parameter, silences every simply-enabled trigger with
// no DDL at all and was handed a warning naming two things it is not. The remedy
// there is a REVOKE on a PARAMETER, which no amount of role-swapping reaches.
//
// The decision recorded on this finding was to REPORT the route rather than arm
// ENABLE ALWAYS, so this pins that the daemon reports it.

import (
	"os"
	"strings"
	"testing"
)

func TestBootNamesTheAuditBypassRouteRatherThanGuessingIt(t *testing.T) {
	b, err := os.ReadFile("boot_deps.go")
	if err != nil {
		t.Fatalf("read boot_deps.go: %v", err)
	}
	boot := string(b)

	// 1. It asks for the ROUTES, not for a bool it then has to explain.
	if !strings.Contains(boot, "db.AuditDDLBypassRoutes(") {
		t.Errorf("boot_deps.go does not call db.AuditDDLBypassRoutes; a bare bool cannot tell an operator WHICH of the " +
			"four routes to close, and the remedies differ (a different role, a REVOKE on the table, a REVOKE on a PARAMETER)")
	}
	if !strings.Contains(boot, `slog.Any("bypass_routes", routes)`) {
		t.Errorf("the DDL-protection warning does not log the routes it found, so the operator is told a posture and " +
			"not the thing to fix")
	}

	// 2. And it no longer ASSERTS a cause the check did not establish. This is
	//    the sentence the finding names: it is false for the TRIGGER-privilege
	//    route and for the session_replication_role route, which are two of the
	//    four the same check counts.
	for _, guess := range []string{
		"still owns audit_events or is a superuser",
		"connect wardynd as a distinct non-owner role",
	} {
		if strings.Contains(boot, guess) {
			t.Errorf("the DDL-protection warning still says %q. AuditDDLProtected counts FOUR routes; that sentence "+
				"names two of them as the cause and prescribes a remedy that closes only those two", guess)
		}
	}

	// 3. SCOPED: the two lines docs/OPERATIONS.md quotes as the confirmation to
	//    look for must survive any rewording (TestMigrateDSNAdoptionIsDocumented
	//    pins them from the other side; asserted here too so a change to this
	//    warning is checked against them in the same test that changes it).
	for _, anchor := range []string{
		"the append-only guard is DDL-protected",
		"DDL protection is NOT in effect",
	} {
		if !strings.Contains(boot, anchor) {
			t.Errorf("boot_deps.go no longer logs %q — the runbook quotes it verbatim as the line an operator greps for", anchor)
		}
	}
}
