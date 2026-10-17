// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// newImageBuild assembles the record of one successful build (design
// env-build-record §3.2). built.Ref and key are what the workspace row will
// name, so the record is current exactly while the row still does.
func newImageBuild(lane, key string, runID uuid.UUID, reason string, in types.ImageBuildInputs, built BuiltImage, now time.Time) *types.ImageBuild {
	return &types.ImageBuild{
		Schema:        types.ImageBuildSchema,
		Lane:          lane,
		CacheKey:      key,
		BuildID:       runID.String(),
		Reason:        reason,
		BuiltAt:       now.UTC(),
		WardynVersion: version.Version,
		Resolved:      imageBuildResolved(lane, in),
		Inputs:        in,
		Image:         types.ImageBuildImage{Ref: built.Ref, ID: built.ID},
	}
}

// imageBuildResolved is the design §3.2 lane table: whether the build's inputs
// are pinned tightly enough to rebuild the same image.
func imageBuildResolved(lane string, in types.ImageBuildInputs) bool {
	switch lane {
	case types.ImageBuildLaneBYOI:
		return in.Base.Digest != ""
	case types.ImageBuildLaneGenerated:
		if in.Base.Digest == "" {
			return false
		}
		for _, f := range in.Features {
			if f.Digest == "" {
				return false
			}
		}
		for _, i := range in.Installers {
			if i.Version == "" || i.SHA256 == "" {
				return false
			}
		}
		return true
	default: // the repository's files and their inputs are not pinned
		return false
	}
}

// imageBuildReason is why this build runs, decided before it does: the key
// matched but the image is gone, or there was nothing usable to reuse.
func imageBuildReason(primary types.Workspace, key string) string {
	if primary.ImageRef != "" && primary.BuiltProfileHash == key {
		return types.ImageBuildReasonImageMissing
	}
	return types.ImageBuildReasonCacheMiss
}

// repoRecordURL is the clone URL as the record keeps it: without userinfo, which
// repoCloneURL passes through for an https source and which is a credential. A URL
// that does not parse fails closed: everything up to the last '@' before the path
// goes, and a URL whose authority cannot be found is not recorded at all.
func repoRecordURL(cloneURL string) string {
	if u, err := url.Parse(cloneURL); err == nil {
		u.User = nil
		return u.String()
	}
	scheme, rest, ok := strings.Cut(cloneURL, "://")
	if !ok {
		if scpRepoURL.MatchString(cloneURL) {
			// user@host:path carries a user name, never a password; a user name that is a token's
			// shape is dropped, since ssh never needed it to authenticate.
			user, hostPath, _ := strings.Cut(cloneURL, "@")
			if _, secret := contentscan.MatchSecretFormat(user); secret {
				return hostPath
			}
			return cloneURL
		}
		return unparseableRepoURL
	}
	authority, path, hasPath := strings.Cut(rest, "/")
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return unparseableRepoURL
	}
	out := scheme + "://" + authority[at+1:]
	if hasPath {
		out += "/" + path
	}
	return out
}

var scpRepoURL = regexp.MustCompile(`^[\w.-]+@[\w.-]+:[^\s@]*$`)

const unparseableRepoURL = "<unparseable repository URL>"

// imageBuildView returns ws's build record while it is current (design §3.3):
// schema known, naming the row's image and cache key, and, when the builder can
// say, still the image its tag names. missing reports a current record whose
// image the builder no longer has. liveID is the builder's answer for the row's
// image, "" when it gave none, whether or not a record is current.
func (s *Server) imageBuildView(ctx context.Context, ws types.Workspace) (rec *types.ImageBuild, missing bool, liveID string) {
	rec = ws.ImageBuild
	current := rec != nil && rec.Schema == types.ImageBuildSchema && rec.Image.Ref == ws.ImageRef && rec.CacheKey == ws.BuiltProfileHash
	ider, ok := s.cfg.ImageBuilder.(ImageIdentifier)
	if !ok || ws.ImageRef == "" {
		if current {
			return rec, false, ""
		}
		return nil, false, ""
	}
	lctx, cancel := context.WithTimeout(ctx, localImageTimeout)
	defer cancel()
	id, found, err := ider.LocalImage(lctx, ws.ImageRef)
	switch {
	case err != nil:
		return nil, false, ""
	case !found:
		if current {
			return rec, true, ""
		}
		return nil, false, ""
	case !current || id != rec.Image.ID:
		return nil, false, id
	}
	return rec, false, id
}

// projectImageBuild is rec as the reader's tier may see it: below the full tier
// a byoi record's base is the operator's authored coordinate, blanked as
// redactWorkspaceForRead blanks base_image. The image stays, as image_ref does.
func projectImageBuild(rec *types.ImageBuild, tier workspaceReadTier) *types.ImageBuild {
	if rec == nil || tier == workspaceReadFull || rec.Lane != types.ImageBuildLaneBYOI {
		return rec
	}
	c := *rec
	c.Inputs.Base = types.ImageBuildRef{}
	return &c
}

// withBuildRecord adds the record to a done answer, only while it is current. record_sha256 is the hash of the FULL
// record, so it matches the build's audit row at every tier.
func (s *Server) withBuildRecord(ctx context.Context, resp buildResponse, ws types.Workspace, tier workspaceReadTier) buildResponse {
	if resp.State != "done" {
		return resp
	}
	rec, _, _ := s.imageBuildView(ctx, ws)
	if rec == nil {
		return resp
	}
	sum, err := rec.SHA256()
	if err != nil {
		return resp
	}
	resp.Record = projectImageBuild(rec, tier)
	resp.RecordSHA256 = sum
	resp.Reproducible = &rec.Resolved
	return resp
}

// addBuildRecordFields puts a fresh build's identity on its run.build success row. A record that cannot be
// marshalled is left off the row (mustJSON would reduce the whole row to {}).
func addBuildRecordFields(extra map[string]any, rec *types.ImageBuild) {
	extra["image"] = rec.Image.Ref
	extra["reason"] = rec.Reason
	extra["resolved"] = rec.Resolved
	if rec.Image.ID != "" {
		extra["image_id"] = rec.Image.ID
	}
	if sum, err := rec.SHA256(); err == nil {
		extra["record"] = rec
		extra["record_sha256"] = sum
	}
}

// addCacheHitFields puts the image's identity on a cache hit's run.build row: the
// record's while it is current, else the builder's own answer, else nothing.
func (s *Server) addCacheHitFields(ctx context.Context, primary types.Workspace, extra map[string]any) {
	rec, _, liveID := s.imageBuildView(ctx, primary)
	if rec != nil {
		if rec.Image.ID != "" {
			extra["image_id"] = rec.Image.ID
		}
		if sum, err := rec.SHA256(); err == nil {
			extra["record_sha256"] = sum
		}
		return
	}
	if liveID != "" {
		extra["image_id"] = liveID
	}
}

// buildSuccessData adds the built image's ID to a per-run build's run.build row.
func buildSuccessData(data map[string]any, imageID string) map[string]any {
	if imageID != "" {
		data["image_id"] = imageID
	}
	return data
}

// remoteDigestTimeout bounds each registry lookup a build makes before it starts.
const remoteDigestTimeout = 10 * time.Second

// localImageTimeout bounds the build host's answer for what a tag names, so a hung daemon cannot hold
// GET /build open; no answer reads as unrecorded.
const localImageTimeout = 10 * time.Second

const pinnedInputGoneError = "a pinned input is no longer available; refresh the image to resolve it again"

// remoteDigest is one bounded RemoteDigest lookup. A builder that cannot answer is an error,
// which every caller reads as "leave it unpinned" or "inconclusive".
func (s *Server) remoteDigest(ctx context.Context, ref string) (string, error) {
	ider, ok := s.cfg.ImageBuilder.(ImageIdentifier)
	if !ok {
		return "", errors.New("the image builder cannot resolve digests")
	}
	ctx, cancel := context.WithTimeout(ctx, remoteDigestTimeout)
	defer cancel()
	return ider.RemoteDigest(ctx, ref)
}

// resolveGeneratedPins pins the generated build's base and each feature to the digest the
// registry gives now (design env-build-record §6.1). A lookup that fails for any reason leaves
// that input unpinned: an unresolvable input is recorded as such, never a failed run.
func (s *Server) resolveGeneratedPins(ctx context.Context, unpinned types.ImageBuildInputs) workspacescan.Pins {
	pins := workspacescan.Pins{Features: map[string]string{}}
	refs := []string{unpinned.Base.Ref}
	for _, f := range unpinned.Features {
		refs = append(refs, f.Ref)
	}
	for i, ref := range refs {
		d, err := s.remoteDigest(ctx, ref)
		if err != nil {
			slog.DebugContext(ctx, "wardynd: image input left unpinned", slog.String("ref", ref), slog.Any("err", err))
			continue
		}
		if i == 0 {
			pins.BaseDigest = d
		} else {
			pins.Features[ref] = d
		}
	}
	return pins
}

// pinnedInputsGone is the rebuild pre-check (design §6.4): each pinned base and feature is
// asked of the registry as <repository>@<digest>. A manifest the registry reports missing is a
// difference; a lookup that could not be answered is not one, and the build goes ahead pinned.
func (s *Server) pinnedInputsGone(ctx context.Context, in types.ImageBuildInputs, pins workspacescan.Pins) []buildDifference {
	var gone []buildDifference
	check := func(kind, ref, digest string) {
		if digest == "" {
			return
		}
		named, err := reference.ParseNormalizedNamed(ref)
		if err != nil {
			return
		}
		if _, err := s.remoteDigest(ctx, reference.TrimNamed(named).String()+"@"+digest); errors.Is(err, ErrRemoteDigestNotFound) {
			gone = append(gone, buildDifference{Input: kind + ":" + ref, Want: digest, Got: "not_found"})
		}
	}
	check("base", in.Base.Ref, pins.BaseDigest)
	for _, f := range in.Features {
		check("feature", f.Ref, pins.Features[f.Ref])
	}
	return gone
}

// withObservedInstaller fills the recorded installer fields from the pin file the build wrote;
// a pin file that does not parse leaves them empty.
func withObservedInstaller(in types.ImageBuildInputs, pinFile []byte) types.ImageBuildInputs {
	pin, ok := workspacescan.ParseInstallerPin(pinFile)
	if !ok {
		return in
	}
	for i := range in.Installers {
		in.Installers[i].Version, in.Installers[i].Platform, in.Installers[i].SHA256 = pin.Version, pin.Platform, pin.SHA256
	}
	return in
}

// buildDifference is one entry of a failed rebuild's `difference`.
type buildDifference struct {
	Input string `json:"input"`
	Want  string `json:"want"`
	Got   string `json:"got"`
}

// describeDifference renders the entries for GET /build's detail: ": <input> (want <digest>, got <got>)[; ...]".
func describeDifference(diff []buildDifference) string {
	parts := make([]string, 0, len(diff))
	for _, d := range diff {
		parts = append(parts, fmt.Sprintf("%s (want %s, got %s)", d.Input, d.Want, d.Got))
	}
	return ": " + strings.Join(parts, "; ")
}
