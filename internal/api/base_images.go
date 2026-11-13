// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/distribution/reference"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// base_images.go — the tier-2 BASE-IMAGE CATALOG endpoints: shared, reusable
// images (registry ref / custom recipe / BYO) referenced by many workspaces.
// "Recommended" is deliberately absent: that build is DERIVED from one
// workspace's merged source profiles, has no catalog identity, and the store's
// CHECK constraint refuses it structurally.

// validateBaseImageWrite mirrors the workspace's own base-image shape checks
// (validateWorkspaceBaseImage's rules, catalog-shaped): a known reusable kind and
// a non-empty image ref with no control characters.
//
// The step caps are gone: they justified themselves as "the same caps the
// wizard's build-steps editor enforces", and that editor does not exist — nor
// could the steps ever run. See types.BaseImageEntry.Steps.
func validateBaseImageWrite(b types.BaseImageEntry) string {
	switch b.Kind {
	case "registry", "byo", "custom", types.BaseImageKindPrewrapped:
	default:
		return `kind must be "registry", "custom", "byo" or "prewrapped" — "recommended" is derived per workspace, never a catalog entry`
	}
	if strings.TrimSpace(b.Name) == "" {
		return "name is required"
	}
	img := strings.TrimSpace(b.Image)
	if img == "" {
		return "image is required"
	}
	if !repoFieldSafe(img) {
		return "image must not contain whitespace or control characters"
	}
	if !imageRefPathSafe(img) {
		return fmt.Sprintf(image400DotSegment, "image")
	}
	return ""
}

// image400DotSegment — %s is the field name.
const image400DotSegment = `%s is not an image reference — an image reference has no backslash and no "", "." or ".." path segment`

// imageRefPathSafe reports whether an image ref has no backslash and no
// empty, "." or ".." segment. No registry names an image that way, and an
// image ref is also a capability value the console builds a control path from
// (the Images tab's Available to), so a member-typed `../policy/<uuid>` must
// never be stored.
// Not repoLocatorPathSafe: that reads "host:5000/..." and "img:tag" as
// scp-form and checks only what follows the colon.
func imageRefPathSafe(ref string) bool {
	if strings.Contains(ref, `\`) {
		return false
	}
	for _, seg := range strings.Split(strings.TrimSpace(ref), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// image400Prefix — %s is the quoted value. A `*` in an image grant is only ever the registry-prefix form.
const image400Prefix = `%s is not an image prefix — a prefix grant is "<registry host>/<path>/*": a lowercase registry host (a first segment holding a "." or ":", or "localhost"), plain path segments, and one "*" as the whole last segment, with no tag, digest, whitespace, "." or ".." segment`

// imagePrefixSuffix ends an image capability value that grants a registry
// prefix instead of one exact reference.
const imagePrefixSuffix = "/*"

// imagePrefixGrant reports whether an image capability value is a well-formed
// prefix grant ("registry.example.com/team/*") and returns the part before
// "/*". Any other value holding a "*" besides the bare wildcard is not a
// grant of any kind: capValueMatches gives it no match at all (imageStarRow),
// so a row stored before this form existed cannot start matching by accident.
func imagePrefixGrant(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasSuffix(v, imagePrefixSuffix) {
		return "", false
	}
	prefix := strings.TrimSuffix(v, imagePrefixSuffix)
	return prefix, validImagePrefix(prefix)
}

// isImagePrefixGrant: v is a well-formed image prefix grant.
func isImagePrefixGrant(v string) bool {
	_, ok := imagePrefixGrant(v)
	return ok
}

// imageStarRow: an image value with a "*" that is neither the bare wildcard nor
// a well-formed prefix. The write boundary refuses it; a row stored earlier
// matches nothing.
func imageStarRow(v string) bool {
	v = strings.TrimSpace(v)
	if v == capWildcard || !strings.Contains(v, "*") {
		return false
	}
	_, ok := imagePrefixGrant(v)
	return !ok
}

// validImagePrefix: a path-safe "host/seg/..." with no tag, digest, whitespace
// or "*", whose first segment is a registry host by the same test
// github.com/distribution/reference uses for a domain (a "." or ":" in it, or
// exactly "localhost"). Without that, "team/*" would name a Docker Hub
// namespace by accident. The host and every path segment are ASCII
// [a-z0-9._-] (the host may also hold one ":port", 1..65535, and may not be
// only dots). Nothing is normalised: the prefix is compared as typed.
func validImagePrefix(prefix string) bool {
	if !imageRefPathSafe(prefix) {
		return false
	}
	segs := strings.Split(prefix, "/")
	host := segs[0]
	if !imageIsRegistryHost(host) || strings.Count(host, ":") > 1 {
		return false
	}
	name, port, hasPort := strings.Cut(host, ":")
	if hasPort {
		n, err := strconv.Atoi(port)
		if !allASCIIDigits(port) || err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	if strings.Trim(name, ".") == "" || !imageNameChars(name) {
		return false
	}
	for _, seg := range segs[1:] {
		if !imageNameChars(seg) {
			return false
		}
	}
	return true
}

// imageNameChars: a non-empty run of ASCII [a-z0-9._-].
func imageNameChars(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// allASCIIDigits: a registry port is one or more ASCII digits.
func allASCIIDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// imageRepoAsTyped is a reference with its @digest and :tag removed, cut on
// the string as typed (no normalisation: a bare "nginx" stays "nginx", a host
// keeps its case). ok is false for a reference that cannot be an image: any
// whitespace, a "*", a "." or ".." or empty segment, a digest holding a "/", or
// a ":" outside the host and the last segment.
func imageRepoAsTyped(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if strings.ContainsFunc(ref, unicode.IsSpace) || strings.Contains(ref, "*") || !imageRefPathSafe(ref) {
		return "", false
	}
	if at := strings.Index(ref, "@"); at >= 0 {
		if strings.Contains(ref[at:], "/") {
			return "", false
		}
		ref = ref[:at]
	}
	segs := strings.Split(ref, "/")
	last := len(segs) - 1
	for i := 1; i < last; i++ {
		if strings.Contains(segs[i], ":") {
			return "", false
		}
	}
	if last > 0 {
		segs[last], _, _ = strings.Cut(segs[last], ":")
	}
	return strings.Join(segs, "/"), true
}

// imagePrefixCovers: a prefix grant matches want only when want's repository,
// as typed, starts with prefix + "/" and has at least one more segment. The
// tag and digest play no part; a want that is not a plausible reference
// matches nothing.
func imagePrefixCovers(prefix, want string) bool {
	repo, ok := imageRepoAsTyped(want)
	return ok && len(repo) > len(prefix)+1 && strings.HasPrefix(repo, prefix+"/")
}

// imagePrefixOverlaps is the prefix half of capValueOverlaps, asked only after
// capValueMatches said no: two prefixes overlap when one is a segment-boundary
// prefix of the other (or they are equal), and an exact value overlaps a
// prefix that would match it. A prefix grant against an exact want is
// capValueMatches' own question, so it is not repeated here.
func imagePrefixOverlaps(grantValue, want string) bool {
	wp, wantIsPrefix := imagePrefixGrant(want)
	if !wantIsPrefix {
		return false
	}
	gp, grantIsPrefix := imagePrefixGrant(grantValue)
	if !grantIsPrefix {
		return imagePrefixCovers(wp, grantValue)
	}
	return gp == wp || strings.HasPrefix(gp, wp+"/") || strings.HasPrefix(wp, gp+"/")
}

// imageSpellings lists the spellings of one image reference that name the same
// registry and image, for the DENY side only (an allow is compared as typed):
// the reference as typed; with its registry host lowercased, ":443" stripped and
// the Docker Hub registry aliases read as "docker.io"; for a reference with no
// registry host or a Docker Hub one, its fully qualified Docker Hub form
// ("nginx:1" is "docker.io/library/nginx:1"), tag and digest kept; and the two
// same-image forms of imageSameImageForms (digest alone, implicit ":latest").
// Without it a deny on "registry.example.com/team/*" is walked around by
// "REGISTRY.example.com/team/x" or "registry.example.com:443/team/x".
func imageSpellings(ref string) []string {
	ref = strings.TrimSpace(ref)
	out := []string{ref}
	norm := imageNormalHost(ref)
	if norm != ref {
		out = append(out, norm)
	}
	if hub, ok := imageDockerHubForm(norm); ok && hub != norm && hub != ref {
		out = append(out, hub)
	}
	return imageAppendForms(out, out[len(out)-1])
}

// imageDenyForm is a deny row's value read the same way imageSpellings reads a
// want: host lowercased, ":443" stripped, Docker Hub made fully qualified. A
// value that cannot be read that way is returned as stored.
func imageDenyForm(v string) string {
	v = imageNormalHost(strings.TrimSpace(v))
	if hub, ok := imageDockerHubForm(v); ok {
		return hub
	}
	return v
}

// imageDenyForms is every form a deny row (or a restriction) value answers to:
// as stored, imageDenyForm, and imageSameImageForms of that. Only an exact
// reference gains the extra forms; a prefix pattern never parses.
func imageDenyForms(v string) []string {
	out := []string{v}
	if d := imageDenyForm(v); d != v {
		out = append(out, d)
	}
	return imageAppendForms(out, out[len(out)-1])
}

// imageAppendForms adds imageSameImageForms(qualified) to out, skipping repeats.
func imageAppendForms(out []string, qualified string) []string {
	for _, f := range imageSameImageForms(qualified) {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// imageSameImageForms are the other ways one exact reference names the same
// image: with a digest, the digest alone (a runtime pulls by digest, so
// "app:1@sha256:X" is "app@sha256:X"); with neither tag nor digest, the
// implicit ":latest". Any registry. Nothing for a value that is not a
// reference.
func imageSameImageForms(ref string) []string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return nil
	}
	if d, ok := named.(reference.Digested); ok {
		return []string{reference.TrimNamed(named).String() + "@" + d.Digest().String()}
	}
	if _, ok := named.(reference.Tagged); !ok {
		return []string{reference.TagNameOnly(named).String()}
	}
	return nil
}

// imageNormalHost lowercases the registry host of ref (its first segment, when
// that reads as a host), drops a ":443" port and maps the Docker Hub registry
// aliases (index.docker.io, registry-1.docker.io, registry.hub.docker.com) to
// docker.io. The path, tag and digest are left as typed.
func imageNormalHost(ref string) string {
	host, rest, hasRest := strings.Cut(ref, "/")
	if !hasRest || !imageIsRegistryHost(host) {
		return ref
	}
	host = strings.TrimSuffix(strings.ToLower(host), ":443")
	switch host {
	case "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		host = "docker.io"
	}
	return host + "/" + rest
}

// imageIsRegistryHost is the test github.com/distribution/reference applies to
// a first segment: a "." or ":" in it, or exactly "localhost".
func imageIsRegistryHost(seg string) bool {
	return strings.ContainsAny(seg, ".:") || seg == "localhost"
}

// imageDockerHubForm returns the fully qualified Docker Hub spelling of ref
// ("docker.io/library/nginx:1") when ref names a Docker Hub image, either with
// no registry host or with docker.io. ok is false for any other registry and
// for a reference the parser refuses.
func imageDockerHubForm(ref string) (string, bool) {
	if host, _, hasRest := strings.Cut(ref, "/"); hasRest && imageIsRegistryHost(host) && host != "docker.io" {
		return "", false
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil || reference.Domain(named) != "docker.io" {
		return "", false
	}
	out := named.Name()
	if t, ok := named.(reference.Tagged); ok {
		out += ":" + t.Tag()
	}
	if d, ok := named.(reference.Digested); ok {
		out += "@" + d.Digest().String()
	}
	return out, true
}

// handleListBaseImages returns the catalog. A security admin sees the prewrapped rows only: the
// other kinds name the operator's registry coordinate (mayWriteBaseImageKind). image_builder says
// whether this control plane wraps the other kinds at all, and prewrapped_check_unavailable why it
// cannot check a prewrapped one (empty when it can); the console's Add image reads both.
//
//	GET /api/v1/base-images
func (s *Server) handleListBaseImages(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Store.ListBaseImages(r.Context())
	if err != nil {
		writeServerError(w, r, "list base images", err)
		return
	}
	now := s.cfg.Now()
	out := make([]types.BaseImageEntry, 0, len(list))
	for _, b := range list {
		if s.mayWriteBaseImageKind(r.Context(), b.Kind) {
			out = append(out, prewrappedView(b, now))
		}
	}
	_, why := s.prewrappedChecker()
	writeJSON(w, http.StatusOK, map[string]any{"base_images": out, "image_builder": s.cfg.ImageBuilder != nil,
		"prewrapped_check_unavailable": why})
}

// mayWriteBaseImageKind: an admin writes every kind; a security admin writes prewrapped only (design
// final §3), and the route group has already refused anyone else.
func (s *Server) mayWriteBaseImageKind(ctx context.Context, kind string) bool {
	return s.isOperator(ctx) || kind == types.BaseImageKindPrewrapped
}

// handleCreateBaseImage upserts a catalog image by identity (kind, image,
// steps) — saving the same recipe twice is one entry, which is the reuse the
// catalog exists for. 200 on an identity hit, 201 on a new row.
//
//	POST /api/v1/base-images
func (s *Server) handleCreateBaseImage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind  string   `json:"kind"`
		Name  string   `json:"name"`
		Image string   `json:"image"`
		Steps []string `json:"steps,omitempty"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	explicitName := strings.TrimSpace(req.Name)
	entry := types.BaseImageEntry{
		ID: uuid.New(), Kind: req.Kind, Name: explicitName,
		Image: strings.TrimSpace(req.Image), Steps: req.Steps,
		CreatedAt: s.cfg.Now().UTC(), UpdatedAt: s.cfg.Now().UTC(),
	}
	if entry.Name == "" {
		entry.Name = lastPathSegment(entry.Image, nil)
	}
	if !s.mayWriteBaseImageKind(r.Context(), entry.Kind) {
		s.refuse(w, r, authz.Deny(authz.ReasonAdminSurface, r.URL.Path, ""))
		return
	}
	if msg := validateBaseImageWrite(entry); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonBaseImageWriteInvalid, msg)
		return
	}
	var checker runner.PrewrappedImageChecker
	if entry.Kind == types.BaseImageKindPrewrapped {
		if len(entry.Steps) > 0 { // runs as is, and its admission lookup is by the ref alone
			writeErrorReason(w, http.StatusBadRequest, reasonBaseImageWriteInvalid, "a prewrapped image runs as is: it takes no steps")
			return
		}
		if !prewrappedPinned(entry.Image) {
			writeErrorReason(w, http.StatusBadRequest, reasonPrewrappedImageNotPinned,
				"a prewrapped image is registered by digest: <repository>[:tag]@sha256:<64 hex>")
			return
		}
		var why string
		if checker, why = s.prewrappedChecker(); checker == nil {
			writeErrorReason(w, http.StatusUnprocessableEntity, reasonPrewrappedCheckUnavailable,
				"a prewrapped image is checked on this control plane's own Kubernetes cluster, and it cannot check one: "+why)
			return
		}
		entry.Check = &types.BaseImageCheck{Attempt: uuid.New()}
	}
	created, err := s.cfg.Store.UpsertBaseImage(r.Context(), entry)
	if err != nil {
		writeServerError(w, r, "upsert base image", err)
		return
	}
	if created.ID == entry.ID && checker != nil {
		s.startImageCheck(r, checker, created)
	}
	status := http.StatusCreated
	if created.ID != entry.ID {
		status = http.StatusOK
		// An identity hit is the Add dialog's ONLY rename route (no
		// separate edit UI/API/CLI/SDK) — apply the operator's explicitly
		// typed name here, never inside UpsertBaseImage's own conflict clause
		// (see its doc comment: that upsert is also a passthrough path with
		// no rename intent at all, which must never clobber a custom name).
		if explicitName != "" {
			updated, uerr := s.cfg.Store.UpdateBaseImageName(r.Context(), created.ID, explicitName)
			if uerr != nil {
				writeServerError(w, r, "apply name to existing base image", uerr)
				return
			}
			created = updated
		}
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"base_image.write", created.ID.String(), "success", mustJSON(map[string]any{
			"kind": created.Kind, "image": created.Image, "steps": len(created.Steps),
		})))
	writeJSON(w, status, prewrappedView(created, s.cfg.Now()))
}

// handleDeleteBaseImage removes a catalog row. In use → 409 naming the
// workspaces; ?force=1 detaches them, which is HONEST here rather than
// destructive: a detached workspace's base_image_id goes NULL, and NULL means
// the derived recommended build — a working state, stated as such. The in-use
// gate and the delete are ONE atomic statement in the store (DeleteBaseImage):
// WorkspacesUsingBaseImage here only names who's using it for the 409 body,
// it does not decide the outcome, so a workspace attaching between this call
// and the delete can never slip through.
//
//	DELETE /api/v1/base-images/{id}[?force=1]
func (s *Server) handleDeleteBaseImage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "base image")
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	// The stored kind decides who may delete, before any referencing workspace is named. Detaching
	// workspaces stays an admin's.
	if !s.isOperator(r.Context()) {
		b, err := s.cfg.Store.GetBaseImage(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeErrorReason(w, http.StatusNotFound, reasonBaseImageNotFound, "no such base image")
			return
		}
		if err != nil {
			writeServerError(w, r, "read base image", err)
			return
		}
		if force || !s.mayWriteBaseImageKind(r.Context(), b.Kind) {
			s.refuse(w, r, authz.Deny(authz.ReasonAdminSurface, r.URL.Path, ""))
			return
		}
	}
	names, err := s.cfg.Store.WorkspacesUsingBaseImage(r.Context(), id)
	if err != nil {
		writeServerError(w, r, "check base image use", err)
		return
	}
	// The audit row names what the store's detach changed, not this listing: a workspace that
	// pointed at the image after it is detached too. The listing names who uses it for the 409.
	detached, err := s.cfg.Store.DeleteBaseImage(r.Context(), id, force)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			if force {
				// A workspace pointed at the image while the forced delete ran: the store rolled its
				// detach back. The listing above predates that workspace and force is already set.
				writeErrorReason(w, http.StatusConflict, reasonBaseImageInUse,
					"A workspace started using this base image during the delete, so it was not deleted. Try again.")
				return
			}
			writeErrorReason(w, http.StatusConflict, reasonBaseImageInUse, fmt.Sprintf(
				"base image is used by %d workspace(s): %s — repoint them first, or pass ?force=1 (they fall back to the derived recommended build)",
				len(names), strings.Join(names, ", ")))
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeErrorReason(w, http.StatusNotFound, reasonBaseImageNotFound, "no such base image")
			return
		}
		writeServerError(w, r, "delete base image", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"base_image.delete", id.String(), "success", mustJSON(map[string]any{
			"detached_from": detached, "forced": force,
		})))
	w.WriteHeader(http.StatusNoContent)
}
