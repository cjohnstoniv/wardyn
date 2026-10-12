// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
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
// repoCloneURL passes through for an https source and which is a credential.
func repoRecordURL(cloneURL string) string {
	u, err := url.Parse(cloneURL)
	if err != nil || u.User == nil {
		return cloneURL
	}
	u.User = nil
	return u.String()
}

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
	id, found, err := ider.LocalImage(ctx, ws.ImageRef)
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
