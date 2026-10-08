// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "net/http"

// componentAttachRefusal is the who-may-attach question for one component on
// a run: orgRowID is an org row's id, or "" for a component the person defined
// (inline on the request, or their own saved row).
//
// Neither sentence names the component, its hosts, its secrets or its id: an
// org row is admin content, and a person who is not granted it must learn
// nothing about it from the refusal — not even that the id they sent is the
// one an admin wrote, so a refused id and a denied absent one answer the same
// bytes.
func (s *Server) componentAttachRefusal(r *http.Request, orgRowID string) *runRefusal {
	if orgRowID == "" {
		return s.runCapabilityRefusal(r, capFeature, featureCustomComponent, "runs.component",
			"Custom components aren't turned on for you. Ask your admin.")
	}
	return s.runCapabilityRefusal(r, capComponent, orgRowID, "runs.component",
		"This component isn't available to you. Ask your admin.")
}
