// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit_test

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/releasesig/releasesigtest"
)

// An identity_regexp must match the whole certificate identity. Alternation binds looser than the ^ and $
// the author wrote, so each alternative has to be grouped before it is matched.
func TestIdentityRegexpMatchesTheWholeIdentity(t *testing.T) {
	const wf = "https://github\\.com/corp/images/\\.github/workflows/build\\.yml@refs/heads/"
	const wfURL = "https://github.com/corp/images/.github/workflows/build.yml@refs/heads/"
	tests := []struct {
		name, pattern, identity string
		admitted                bool
	}{
		{"main alternative", "^" + wf + "main|" + wfURL + "release$", wfURL + "main", true},
		{"release alternative", "^" + wf + "main|" + wfURL + "release$", wfURL + "release", true},
		{"main prefix is not main", "^" + wf + "main|" + wfURL + "release$", wfURL + "main-untrusted", false},
		{"release suffix is not release", "^" + wf + "main|" + wfURL + "release$", wfURL + "not-release", false},
		{"first alternative prefix", "^a|b$", "ab-extra", false},
		{"second alternative suffix", "^a|b$", "xb", false},
		{"first alternative whole", "^a|b$", "a", true},
		{"second alternative whole", "^a|b$", "b", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := releasesigtest.New(t)
			spec := corpCI()
			spec.Keyless.Identity, spec.Keyless.IdentityRegexp = "", tc.pattern
			o := ciOpts()
			o.Identity = tc.identity
			v := check(t, compile(t, s, spec), nil, s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), o))
			if tc.admitted {
				wantReason(t, v, "")
			} else {
				wantReason(t, v, imageadmit.SignatureInvalid)
			}
		})
	}
}
