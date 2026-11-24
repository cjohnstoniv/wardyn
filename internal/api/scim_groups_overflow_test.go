// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/scim"
)

// Different external group ids may share one display name. A filter by that name answers the whole set when it
// fits the cap, and tooMany, never a set cut at the cap that claims to be complete, when it does not.
func TestSCIMGroupsSameNameFilter_CapAndOverflow(t *testing.T) {
	for _, c := range []struct {
		n       int
		tooMany bool
	}{{scimListLimit, false}, {scimListLimit + 1, true}} {
		t.Run(fmt.Sprintf("%d groups", c.n), func(t *testing.T) {
			st := newSCIMCovStore()
			for i := range c.n {
				st.addGroup(fmt.Sprintf("ext-%03d", i), "Engineering")
			}
			e := newSCIMCovEnv(t, st)
			w := e.scim(http.MethodGet, "/scim/v2/Groups?filter="+url.QueryEscape(`displayName eq "engineering"`), "")
			if c.tooMany {
				scimCovWantError(t, w, http.StatusBadRequest, scim.TypeTooMany)
				return
			}
			list := scimCovDecode[struct {
				TotalResults int                `json:"totalResults"`
				Resources    []scimCovGroupBody `json:"Resources"`
			}](t, w)
			if w.Code != http.StatusOK || list.TotalResults != c.n || len(list.Resources) != c.n {
				t.Errorf("%d groups: %d, totalResults %d, %d resources; want 200 with all of them", c.n, w.Code, list.TotalResults, len(list.Resources))
			}
		})
	}
}
