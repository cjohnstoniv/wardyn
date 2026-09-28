// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// THE HOME-SEGMENT RULES: what a single home name may look like, per
// substrate, and how each substrate's rule is said to an admin and to a
// member. Split out of user_drive.go: everything here is about ONE string
// (the per-person directory segment) and the two alphabets it must satisfy —
// a Docker volume-name component, and a Kubernetes DNS-1123 subdomain.
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
// NOT a DNS-1123 name (`_` and a trailing `-`/`.` are legal here but not
// there) — driveHomeSegmentK8sRe below exists for that case, since a k8s
// drive validated by this rule would be rejected by the apiserver at bind
// time. The leading [a-z0-9] excludes a leading dot (the credential deny
// class member mount rules already refuse) and the ".." traversal.
var driveHomeSegmentRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// driveHomeSegmentK8sRe is the same segment on a KUBERNETES backend, where it
// is concatenated into a PVC NAME (DriveObjectName) and must be a DNS-1123
// subdomain. ANCHORED PER LABEL — a single anchored expression pinning only
// the first/last characters (e.g. `^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)
// let `.`/`-` sit in any order between them, so strings like `a.-b` matched
// here and were then refused by the apiserver mid-run. Each dot-separated
// label is now independently anchored to match
// k8s.io/apimachinery/pkg/util/validation's own rule exactly; an empty label
// is impossible by construction, which also rules out "..".
//
// The 63-char length is a SEPARATE clause (driveHomeSegmentOK): a per-label
// regex can't also carry a whole-string length, and RE2 has no lookahead to
// add one.
//
// Motivating case: an Entra `sub` is base64url and routinely carries `_`, so
// a `k8s_pvc` drive templated on `sub` passed the Docker rule and then
// failed at bind time for every member it allocated. This rule turns that
// into a resolve-time REFUSED_HOME_INVALID an admin sees when previewing the
// allocation, instead of a cluster error inside somebody's run.
var driveHomeSegmentK8sRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// maxDriveHomeLen is the 63 both home rules carry (Docker inline via
// `{0,62}`, k8s via this constant, since its regex can't spell a length).
// 63 is the DNS-1123 label limit; DriveObjectName concatenates the home into
// a name whose own labels must each fit.
const maxDriveHomeLen = 63

// driveHomeSegmentOK reports whether seg is a legal home name for backend b,
// picking the rule from the substrate that has to hold the name. The length
// clause is the one thing the per-label k8s regex above can't say.
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

// DriveHomeStricterRuleClause is the same difference said to a MEMBER: the
// extra sentence a backend's home rule needs beyond the frozen refusal
// (DRIVE_MEMBER.REFUSED_HOME_INVALID, which describes the DOCKER rule), or ""
// when the frozen sentence is already the whole rule. On k8s the rule is
// strictly narrower, and the gap is the same motivating case as above: a
// member told `_` was allowed then hit a refusal with nothing to fix.
//
// A server-composed SUFFIX, not a reworded canon: the frozen sentence still
// ships byte-for-byte, with this clause appended after it.
func DriveHomeStricterRuleClause(b DriveBackend) string {
	if b.RunnerTarget() != "k8s" {
		return ""
	}
	return "(on a Kubernetes deployment the rule is stricter: no _, and it may not end in - or .)"
}
