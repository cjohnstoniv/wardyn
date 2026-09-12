// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// govCeilingOnlyBody is the smallest profile body validatePolicySpec accepts: a
// name and a ceiling with its required confinement class, and no limits block at
// all. govLimitsBody adds one.
const govCeilingOnlyBody = `{"name":"p","ceiling":{"min_confinement_class":"CC2"}}`

func govLimitsBody(limits string) string {
	return `{"name":"p","ceiling":{"min_confinement_class":"CC2"},"limits":{` + limits + `}}`
}

// TestGovernanceLimitsRefuseNegatives is the write boundary GovernanceLimits did
// not have. The sibling ORG block refuses the identical shape by name
// (validateStorageProviders → providers400Negative, plus a default-above-max
// arm); the profile's three numbers decoded straight into the stored row.
//
// NOTHING DOWNSTREAM MIS-ENFORCES a negative — composer.CapDiskMiB needs a
// positive ceiling, driveSizeCeiling.bound treats <= 0 as unlimited, and
// denyMemberRunQuota returns on limit <= 0 — which is why this is a write-boundary
// fix and not a runtime one: a stored -5 renders in the profile editor as a cap
// that binds nothing, and the API is the door the console's own nonNegativeInt
// cannot cover (a hand-made PUT never passes through it).
//
// 0 STAYS UNLIMITED on all three. That is the documented zero value the two
// booleans beside them share, and refusing it would break every profile that
// authors a ceiling without a quota.
func TestGovernanceLimitsRefuseNegatives(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "a negative run quota",
			body: govLimitsBody(`"max_concurrent_runs":-1`),
			want: "limits.max_concurrent_runs",
		},
		{
			name: "a negative ephemeral cap",
			body: govLimitsBody(`"max_ephemeral_disk_mib":-4096`),
			want: "limits.max_ephemeral_disk_mib",
		},
		{
			name: "a negative drive cap",
			body: govLimitsBody(`"max_drive_size_mib":-1`),
			want: "limits.max_drive_size_mib",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/governance/profiles", strings.NewReader(tc.body))
			_, msg := decodeGovernanceProfileRequest(httptest.NewRecorder(), r)
			if !strings.Contains(msg, tc.want) {
				t.Errorf("msg = %q, want it to NAME the field (%s) — an admin told only \"invalid\" has three "+
					"numbers to guess between", msg, tc.want)
			}
		})
	}

	// THE CONTROL, and it is the half that matters as much: 0 is unlimited/unset
	// on every one of the three, so a profile that authors none of them is
	// writable exactly as before.
	for _, body := range []string{
		govCeilingOnlyBody,
		govLimitsBody(``),
		govLimitsBody(`"max_concurrent_runs":0,"max_ephemeral_disk_mib":0,"max_drive_size_mib":0`),
		govLimitsBody(`"max_concurrent_runs":3,"max_ephemeral_disk_mib":4096,"max_drive_size_mib":2048`),
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/governance/profiles", strings.NewReader(body))
		if _, msg := decodeGovernanceProfileRequest(httptest.NewRecorder(), r); msg != "" {
			t.Errorf("body %s refused with %q; 0 is unlimited and a positive number is a cap", body, msg)
		}
	}
}
