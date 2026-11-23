// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"fmt"
	"net/http"

	"github.com/distribution/reference"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A component may name the image a run must use to attach it (#1972,
// ComponentDefinition.Image). The gate only checks: a run whose image does not
// meet every attached component's requirement is refused at every run door,
// and nothing is pulled or chosen for the run.

func componentImageUnmetMsg(name, want, have string) string {
	return fmt.Sprintf("%s needs the image %s, and this run uses %s. Use that image for the run, or remove the component.", name, want, have)
}

func componentImageUnnamedMsg(name, want string) string {
	return fmt.Sprintf("%s needs the image %s, and this run uses the default image or builds one when it starts. Use that image for the run, or remove the component.", name, want)
}

// componentRunImage is the image the run starts from, as far as it is known
// before the start: the request's, which the workspace seed also sets from an
// attached workspace's base image; else a policy-carried workspace's base
// image; else, for a run-mode run whose workspace builds no image, its harness
// set's image, as resolveCreateRunImage picks it. A dev-container build, a
// workspace image built at launch and an older client's convention image give
// "".
func (s *Server) componentRunImage(req createRunRequest, wsRefs []types.Workspace) string {
	switch {
	case req.Image != "":
		return req.Image
	case req.DevcontainerRepo != "":
		return ""
	}
	for _, ws := range wsRefs {
		if b := workspaceBaseImage(ws); b != "" {
			return b
		}
	}
	// req.AllowedImage is refused before this gate at this version (unappliedFields); when the server
	// applies it, it is the case to add here, beside req.Image.
	// Launch may swap this image for the other architecture's variant (archImage), which the gate cannot see.
	if req.UsesRunMode() && !s.workspaceBuildsImage(wsRefs) {
		fact, _ := harnessSetImage(harnessSet(req), s.cfg.AgentImages, customImageRequested(req, wsRefs))
		return fact.Ref
	}
	return ""
}

// componentImageMet reports whether have meets the requirement want: the same
// repository once both are normalised ("nginx" is docker.io/library/nginx),
// and the same digest or tag when want names one. A side that does not parse
// meets nothing.
func componentImageMet(want, have string) bool {
	w, err := reference.ParseNormalizedNamed(want)
	if err != nil {
		return false
	}
	h, err := reference.ParseNormalizedNamed(have)
	if err != nil || h.Name() != w.Name() {
		return false
	}
	if wd, ok := w.(reference.Digested); ok {
		hd, ok := h.(reference.Digested)
		return ok && hd.Digest() == wd.Digest()
	}
	if wt, ok := w.(reference.Tagged); ok {
		// A bare repository runs as :latest, so "nginx" meets "nginx:latest".
		ht, ok := reference.TagNameOnly(h).(reference.Tagged)
		return ok && ht.Tag() == wt.Tag()
	}
	return true
}

// componentImageRefusal is the first attached component, by ordinal, whose
// image requirement have does not meet. The sentence names the component and
// both images, an organisation's component too: the requirement is on its
// card, and choosing the run's image is the person's own fix.
func componentImageRefusal(comps runComponents, have string) *componentRefusal {
	for i, c := range comps.attached {
		want := c.snapshot.Definition.Image
		if want == "" || componentImageMet(want, have) {
			continue
		}
		name := cmp.Or(c.snapshot.Name, "This component")
		msg := componentImageUnnamedMsg(name, want)
		if have != "" {
			msg = componentImageUnmetMsg(name, want, have)
		}
		return &componentRefusal{
			refusal: runError(http.StatusUnprocessableEntity, reasonComponentImageUnmet, fmt.Sprintf("components[%d]: %s", i, msg)),
			reason:  reasonComponentImageUnmet, ordinal: i, source: c.source, id: c.snapshot.ComponentID,
		}
	}
	return nil
}
