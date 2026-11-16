// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Workload names the workload a run was launched for: its owner and its name, the key the workloads
// table holds unique. The zero value is no workload, an ad-hoc run.
type Workload struct {
	// Owner is a person, or service:<name> for a service identity. It may hold any byte, "/" among them.
	Owner string
	// Name is a workload name (workloadName), which never holds "/".
	Name string
}

// String is w as "<owner>/<name>", the gateway token's wardyn_workload claim, and "" for no workload.
// The last "/" splits it, since a name holds none.
func (w Workload) String() string {
	if w == (Workload{}) {
		return ""
	}
	return w.Owner + "/" + w.Name
}

// WorkloadRunMinter is a Provider that also mints for a run a workload launched. It stands apart from
// Provider so the many callers and test doubles that mint ad-hoc runs keep the narrow contract;
// RunConformance holds every provider to it.
type WorkloadRunMinter interface {
	// MintWorkloadRunIdentity is MintRunIdentity for a run w launched: the token's SPIFFE ID has RunPath's
	// workload form, and the ad-hoc form when w is zero.
	MintWorkloadRunIdentity(ctx context.Context, runID uuid.UUID, w Workload, humanSub, sponsor, audience string, operatorOwned bool) (RunIdentity, error)
}

// workloadName is the workload name rule, the workloads table's CHECK on its name column.
var workloadName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// RunPath is the path of runID's SPIFFE ID: /workload/<owner>/<name>/run/<run id> for a run w launched,
// the owner written by escapeOwner, and /agent-run/<run id> for any other run. A relying party admits
// every run of one workload by the prefix up to and including "/run/"; the last segment keeps each run's
// ID its own, so revocation and audit stay per run.
func RunPath(w Workload, runID uuid.UUID) (string, error) {
	if w == (Workload{}) {
		return "/agent-run/" + runID.String(), nil
	}
	if w.Owner == "" || !workloadName.MatchString(w.Name) {
		return "", fmt.Errorf("identity: workload %q needs an owner and a workload name", w.String())
	}
	return "/workload/" + escapeOwner(w.Owner) + "/" + w.Name + "/run/" + runID.String(), nil
}

// ParseRunPath is RunPath's inverse: the workload, zero for an ad-hoc run, and the run of a run's SPIFFE
// ID path. It refuses any other path, and any owner, name or run id not in the one form RunPath writes,
// so a path it accepts is exactly RunPath of what it returns.
func ParseRunPath(path string) (Workload, uuid.UUID, error) {
	seg := strings.Split(path, "/")
	var w Workload
	var run string
	switch {
	case len(seg) == 3 && seg[0] == "" && seg[1] == "agent-run":
		run = seg[2]
	case len(seg) == 6 && seg[0] == "" && seg[1] == "workload" && seg[4] == "run":
		owner, ok := unescapeOwner(seg[2])
		if !ok || !workloadName.MatchString(seg[3]) {
			return Workload{}, uuid.Nil, fmt.Errorf("identity: path %q names no workload in its written form", path)
		}
		w, run = Workload{Owner: owner, Name: seg[3]}, seg[5]
	default:
		return Workload{}, uuid.Nil, fmt.Errorf("identity: path %q is not a run's", path)
	}
	runID, err := uuid.Parse(run)
	if err != nil || runID.String() != run {
		return Workload{}, uuid.Nil, fmt.Errorf("identity: path %q has no run id in its written form", path)
	}
	return w, runID, nil
}

// escapeOwner writes an owner as one SPIFFE path segment. ASCII letters, digits and "-" stand for
// themselves; every other byte, "_" and "." among them, is "_" and the byte's two uppercase hex digits,
// so "service:ci-bot" is "service_3Aci-bot". Each owner has one form, no two owners share one, and the
// form never holds "/" and is never "." or "..".
func escapeOwner(owner string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(owner); i++ {
		c := owner[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('_')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// unescapeOwner is escapeOwner's inverse. It refuses a segment escapeOwner never writes: an empty one, a
// cut-off or non-hex escape, lowercase hex, an escaped byte that stands for itself, and an unescaped
// byte that must not.
func unescapeOwner(seg string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		if seg[i] != '_' {
			b.WriteByte(seg[i])
			continue
		}
		if i+2 >= len(seg) {
			return "", false
		}
		v, err := strconv.ParseUint(seg[i+1:i+3], 16, 8)
		if err != nil {
			return "", false
		}
		b.WriteByte(byte(v))
		i += 2
	}
	owner := b.String()
	return owner, owner != "" && escapeOwner(owner) == seg
}
