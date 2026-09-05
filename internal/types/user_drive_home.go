// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// THE HOME-SEGMENT RULES: what a single home name may look like, per substrate,
// and how each substrate's rule is said to an admin and to a member.
//
// Split out of user_drive.go along the seam the file's own "─── derivation ───"
// divider already named. Everything here is about ONE string — the per-person
// directory segment — and about the two alphabets that string has to satisfy: a
// Docker volume-name component and a Kubernetes DNS-1123 subdomain. The
// derivation that PRODUCES that string, and the object naming that consumes it,
// stay next to the rows they read.
package types

import (
	"fmt"
	"regexp"
)

// driveHomeSegmentRe is the shape a home name may take on a DOCKER backend: a
// single path segment that is also a legal Docker volume-name component, so
// one string can be both a subdirectory of a share and the suffix of a named
// volume.
//
// It is NOT a DNS-1123 name and never was — `_` is not legal in one, and a
// trailing `-` or `.` is not either. That claim used to sit on this comment
// and was the bug driveHomeSegmentK8sRe below exists to close: a k8s drive
// whose home came through here would validate and then be rejected by the
// apiserver at bind time, on somebody's run.
//
// The leading character is [a-z0-9] specifically to exclude a LEADING DOT: a
// dotfile home would put a drive inside the credential deny class the member
// mount rules already refuse by segment, and "..", the traversal, is excluded
// by the same clause.
var driveHomeSegmentRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// driveHomeSegmentK8sRe is the same segment on a KUBERNETES backend, where it
// is concatenated into a PVC NAME (DriveObjectName) and must therefore be a
// DNS-1123 subdomain: lowercase alphanumerics, with `-` and `.` in the middle
// only.
//
// ANCHORED PER LABEL, which is what a subdomain rule IS and what the single
// anchored run this used to be could not say. `^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`
// pinned only the first and last characters and let `.` and `-` sit in any
// order between them, so `a.-b`, `us-.bob`, `ab.-cd` and `x.-y.z` all matched
// HERE and were then refused by the apiserver — inside somebody's run, which is
// the one place this regex exists to move the refusal away from. Each
// dot-separated label is now independently anchored, character-for-character
// the rule k8s.io/apimachinery/pkg/util/validation enforces and prints:
// `[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*`. An EMPTY
// label is impossible by construction, so the ".." traversal falls out of the
// same clause rather than needing its own.
//
// THE 63 IS A SEPARATE CLAUSE NOW (driveHomeSegmentOK), and it has to be: a
// per-label form cannot also carry a whole-string length, and RE2 has no
// lookahead to bolt one on. It is not a check something can forget — both the
// predicate and the rule an admin reads (driveHomeSegmentRule) compose the two
// halves in one place each.
//
// THE MOTIVATING CASE IS NOT HYPOTHETICAL: an Entra `sub` is base64url and
// routinely carries `_`, so a `k8s_pvc` drive templated on `sub` passes the
// Docker rule, is stored, and then fails at bind time for every member it
// allocates. Refusing it in DriveHomeName makes that a resolve-time
// REFUSED_HOME_INVALID naming the claim an admin has to override, at the
// moment the admin previews the allocation, instead of a cluster error inside
// somebody's run.
var driveHomeSegmentK8sRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// maxDriveHomeLen is the 63 both home rules carry: the Docker rule spells it
// inline (`{0,62}` after one anchored character) and the k8s rule cannot, so
// the constant is what the two agree through. 63 is the DNS-1123 LABEL limit,
// kept for a subdomain too because DriveObjectName concatenates the home into
// a name whose own labels must each fit.
const maxDriveHomeLen = 63

// driveHomeSegmentOK reports whether seg is a legal home name for backend b,
// picking the rule from the substrate that has to hold the name.
//
// The LENGTH clause is the one thing the k8s regex above cannot say: it is
// anchored per dot-separated label, so nothing in it bounds the whole string.
// The ".." traversal needs no clause of its own any more — an empty label does
// not match a per-label rule — which is the same fact stated once instead of
// twice.
func driveHomeSegmentOK(b DriveBackend, seg string) bool {
	if b.RunnerTarget() != "k8s" {
		return driveHomeSegmentRe.MatchString(seg)
	}
	return len(seg) <= maxDriveHomeLen && driveHomeSegmentK8sRe.MatchString(seg)
}

// driveHomeSegmentRule renders b's rule for the error message that refuses a
// name — the pattern itself, so an admin reading a refusal sees the shape they
// have to satisfy rather than a prose paraphrase of it that can drift.
func driveHomeSegmentRule(b DriveBackend) string {
	if b.RunnerTarget() != "k8s" {
		return driveHomeSegmentRe.String()
	}
	return fmt.Sprintf("%s, at most %d characters (a DNS-1123 subdomain: it becomes part of a PVC name)",
		driveHomeSegmentK8sRe, maxDriveHomeLen)
}

// DriveHomeStricterRuleClause is the same difference said to a MEMBER: the extra
// sentence a backend's home rule needs beyond the frozen refusal, or "" when the
// frozen sentence is already the whole rule.
//
// The frozen sentence (DRIVE_MEMBER.REFUSED_HOME_INVALID) describes
// driveHomeSegmentRe — "lowercase letters and digits, then . _ -, up to 63
// characters" — which is the DOCKER rule. On a Kubernetes backend
// driveHomeSegmentK8sRe is strictly narrower, and the gap is exactly the
// motivating case: an Entra `sub` is base64url and routinely carries `_`, so the
// member whose k8s drive is templated on `sub` was told the character that
// refused them was allowed, and asked their admin for nothing.
//
// A SERVER-COMPOSED SUFFIX rather than a reworded canon (§7.1's "composed by
// the server — rendered verbatim, never keyed" class): the frozen table is one
// sentence per door and it is frozen, so the substrate's own extra clause is
// appended AFTER it. The canon sentence still ships byte-for-byte on every
// deployment, and a k8s deployment adds the clause its regex actually enforces.
//
// It lives HERE, beside the two regexes, because it is prose about them: a copy
// in the API layer would be a third statement of a rule that already has two.
func DriveHomeStricterRuleClause(b DriveBackend) string {
	if b.RunnerTarget() != "k8s" {
		return ""
	}
	return "(on a Kubernetes deployment the rule is stricter: no _, and it may not end in - or .)"
}
