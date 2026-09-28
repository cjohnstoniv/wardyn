// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package egress

import "context"

// HostVerdict is an Evaluator's policy-only decision for a host, BEFORE the
// proxy's hardwired first-use approval and IP vetting run.
type HostVerdict string

const (
	VerdictAllow   HostVerdict = "allow"
	VerdictDeny    HostVerdict = "deny"
	VerdictUnknown HostVerdict = "unknown" // not denied, not allowed -> approval candidate
)

// Evaluator is the pluggable egress policy-verdict seam. It decides ONLY the
// host allow/deny/unknown verdict and whether a method passes; the proxy
// keeps the security non-negotiables HARDWIRED around it (private-IP guard,
// first-use approval FSM, post-verdict IP vetting, credential injection) so a
// pluggable engine can never weaken them. An EvaluateHost error MUST be
// treated as deny (fail closed).
//
// An alternate to the builtin evaluator must pass
// internal/egress/evaluatortest.RunConformance.
type Evaluator interface {
	Name() string
	EvaluateHost(ctx context.Context, req Request) (HostVerdict, error)
	MethodAllowed(method string) bool
}
