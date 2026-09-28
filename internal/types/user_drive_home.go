// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Home-segment rules: what a per-person home name may look like per substrate (a Docker
// volume-name component; a Kubernetes DNS-1123 subdomain), and how each is worded to users.
package types

import (
	"fmt"
	"regexp"
)

// driveHomeSegmentRe is a home name's shape on Docker: one path segment that's also a
// legal volume-name component; the leading [a-z0-9] excludes a leading dot and "..".
var driveHomeSegmentRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// driveHomeSegmentK8sRe is the same segment on Kubernetes, concatenated into a PVC name so
// it must be a DNS-1123 subdomain, each label independently anchored — a single
// first/last-char anchor once let strings like "a.-b" through, later refused by the
// apiserver mid-run. Length is separate (driveHomeSegmentOK) since RE2 can't add it to a
// per-label regex. Motivating case: an Entra sub is base64url with "_", so this catches at
// resolve time what the Docker rule missed until bind time.
var driveHomeSegmentK8sRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// maxDriveHomeLen is the 63-char DNS-1123 label limit both home rules carry (Docker inline
// via `{0,62}`, k8s via this constant, since its regex can't spell a length).
const maxDriveHomeLen = 63

// driveHomeSegmentOK reports whether seg is legal for backend b; length is the one thing
// the per-label k8s regex above can't say on its own.
func driveHomeSegmentOK(b DriveBackend, seg string) bool {
	if b.RunnerTarget() != "k8s" {
		return driveHomeSegmentRe.MatchString(seg)
	}
	return len(seg) <= maxDriveHomeLen && driveHomeSegmentK8sRe.MatchString(seg)
}

// driveHomeSegmentRule renders b's rule as the pattern itself, so a refusal shows the
// exact shape, not a paraphrase that can drift.
func driveHomeSegmentRule(b DriveBackend) string {
	if b.RunnerTarget() != "k8s" {
		return driveHomeSegmentRe.String()
	}
	return fmt.Sprintf("%s, at most %d characters (a DNS-1123 subdomain: it becomes part of a PVC name)",
		driveHomeSegmentK8sRe, maxDriveHomeLen)
}

// DriveHomeStricterRuleClause is the extra sentence a backend's home rule needs beyond the
// frozen DOCKER-rule refusal, said to a MEMBER (or "" when that already covers it) — a
// server-composed SUFFIX appended after the frozen sentence, never a reworded canon.
func DriveHomeStricterRuleClause(b DriveBackend) string {
	if b.RunnerTarget() != "k8s" {
		return ""
	}
	return "(on a Kubernetes deployment the rule is stricter: no _, and it may not end in - or .)"
}
