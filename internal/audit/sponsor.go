// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
)

// A service identity (#1508) is its own audit actor, and every row its request writes names the person
// who sponsors it as data.sponsor. The marker lives here, beside the delegation one, because rows are
// written by more than the API and DelegationRecorder stamps them all.

type sponsorKey struct{}

// WithSponsor marks ctx as a request made by a service identity sponsored by sponsor.
func WithSponsor(ctx context.Context, sponsor string) context.Context {
	return context.WithValue(ctx, sponsorKey{}, sponsor)
}

// SponsorFrom reports the sponsor of the service identity ctx's request was made by, when it was.
func SponsorFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(sponsorKey{}).(string)
	return v, ok && v != ""
}

// StampSponsor returns data with the sponsor added under "sponsor" when ctx is a service request, and data
// unchanged otherwise. Data that is not a
// JSON object is kept whole under "data", so no service row can lack the stamp.
func StampSponsor(ctx context.Context, data json.RawMessage) json.RawMessage {
	sponsor, ok := SponsorFrom(ctx)
	if !ok {
		return data
	}
	m := map[string]any{}
	if len(data) > 0 && string(data) != "null" {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(data, &obj); err == nil {
			for k, v := range obj {
				m[k] = v
			}
		} else {
			m["data"] = data
		}
	}
	m["sponsor"] = sponsor
	// A map of RawMessage and a string cannot fail to marshal.
	out, _ := json.Marshal(m)
	return out
}
