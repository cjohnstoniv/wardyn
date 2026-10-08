// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
)

// componentUnavailableRefusal is the one sentence an org component refusal
// carries, whatever the reason.
const componentUnavailableRefusal = "This component isn't available to you. Ask your admin."

// componentAttachRefusal is the who-may-attach question for one component on
// a run: orgRowID is an org row's id, or "" for a component the person defined
// (inline on the request, or their own saved row).
//
// Neither sentence names the component, its hosts, its secrets or its id: an
// org row is admin content, and a person who is not granted it must learn
// nothing about it from the refusal — not even that the id they sent is the
// one an admin wrote, so a refused id and a denied absent one answer the same
// bytes.
//
// The id is canonicalized here, not by the caller: a restriction row is stored
// canonical, so an uppercase or braced spelling of a restricted id would miss
// it and fall through to an unenforced kind's allow, while the store still
// resolves that spelling to the same row. A value that is not a uuid names no
// row and is refused with the same bytes.
func (s *Server) componentAttachRefusal(r *http.Request, orgRowID string) *runRefusal {
	if orgRowID == "" {
		return s.runCapabilityRefusal(r, capFeature, featureCustomComponent, "runs.component",
			"Custom components aren't turned on for you. Ask your admin.")
	}
	id, err := uuid.Parse(orgRowID)
	if err != nil {
		return runDenied(authz.Deny(authz.ReasonCapabilityComponent, "runs.component", componentUnavailableRefusal))
	}
	return s.runCapabilityRefusal(r, capComponent, id.String(), "runs.component", componentUnavailableRefusal)
}
