// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// workspace_build_refresh.go — what POST /workspaces/{id}/build adds for {"refresh": true}
// (design env-build-record §7): the body, the failure reasons, and the workspace.image.refresh
// audit row.

// The `reason` of a failed refresh's workspace.image.refresh row.
const (
	refreshFailureSuperseded = "superseded"
	refreshFailureBuild      = "build_failed"
)

// readBuildRefresh reads POST /build's optional body. An empty or whitespace-only body is the
// plain build; otherwise it is {"refresh": bool} and nothing else.
func readBuildRefresh(w http.ResponseWriter, r *http.Request) (refresh, ok bool) {
	raw, ok := readCappedBody(w, r, maxJSONBody, "request body")
	if !ok {
		return false, false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return false, true
	}
	var body struct {
		Refresh bool `json:"refresh"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "invalid JSON body: "+err.Error())
		return false, false
	}
	return body.Refresh, true
}

// supersededDetail is the failed state a build that lost the swap leaves for GET /build.
func supersededDetail(refresh bool) string {
	what := "build"
	if refresh {
		what = "refresh"
	}
	return "superseded: another build replaced this workspace's image while the " + what + " ran"
}

// buildFailureDetail reads the REAL reason back from the audit row resolveWorkspaceImage wrote
// for buildID, so the wizard shows it rather than a pointer at the audit trail.
func (s *Server) buildFailureDetail(ctx context.Context, buildID uuid.UUID) string {
	detail := "build failed — see the run.build audit trail for the reason"
	evs, err := s.cfg.Store.QueryAuditEvents(ctx, buildID, 5)
	if err != nil {
		return detail
	}
	for _, ev := range evs {
		if ev.Action != "run.build" || ev.Outcome != "failure" {
			continue
		}
		var d struct {
			Error      string            `json:"error"`
			Difference []buildDifference `json:"difference"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.Error != "" {
			detail = d.Error
			if len(d.Difference) > 0 {
				detail += describeDifference(d.Difference)
			}
		}
		break
	}
	return detail
}

// refreshAuditData is the data of a workspace.image.refresh row. prev is the record that was
// current when the refresh was asked for, nil when none was. failure is "" for a refresh that
// swapped its image in.
func refreshAuditData(principal string, ws types.Workspace, buildID uuid.UUID, res imageResolveResult, prev *types.ImageBuild, failure string) json.RawMessage {
	data := map[string]any{
		"workspace_id": ws.ID.String(), "build_id": buildID.String(), "lane": workspaceBuildLane(ws),
		"reason": types.ImageBuildReasonRefresh, "changed": []string{},
	}
	if failure != "" {
		data["reason"] = failure
	}
	if prev != nil {
		if prev.Image.ID != "" {
			data["previous_image_id"] = prev.Image.ID
		}
		if sum, err := prev.SHA256(); err == nil {
			data["previous_record_sha256"] = sum
		}
	}
	if failure == "" && res.Record != nil {
		if res.Record.Image.ID != "" {
			data["image_id"] = res.Record.Image.ID
		}
		if sum, err := res.Record.SHA256(); err == nil {
			data["record_sha256"] = sum
		}
		if prev != nil {
			data["changed"] = changedInputs(prev.Inputs, res.Record.Inputs)
		}
	}
	return auditWorkspaceDataFor(principal, ws.OwnedBy, data)
}

// changedInputs names, sorted, every input whose digest or version differs between two
// records: `base`, `feature:<ref>` and `installer:<tool>`. An input only one record has counts
// as moved.
func changedInputs(old, cur types.ImageBuildInputs) []string {
	changed := []string{}
	if old.Base.Digest != cur.Base.Digest {
		changed = append(changed, "base")
	}
	features := map[string][2]string{}
	for _, f := range old.Features {
		features[f.Ref] = [2]string{f.Digest, ""}
	}
	for _, f := range cur.Features {
		v := features[f.Ref]
		v[1] = f.Digest
		features[f.Ref] = v
	}
	for ref, v := range features {
		if v[0] != v[1] {
			changed = append(changed, "feature:"+ref)
		}
	}
	type installer struct{ version, sha string }
	installers := map[string][2]installer{}
	for _, i := range old.Installers {
		installers[i.Tool] = [2]installer{{i.Version, i.SHA256}, {}}
	}
	for _, i := range cur.Installers {
		v := installers[i.Tool]
		v[1] = installer{i.Version, i.SHA256}
		installers[i.Tool] = v
	}
	for tool, v := range installers {
		if v[0] != v[1] {
			changed = append(changed, "installer:"+tool)
		}
	}
	sort.Strings(changed)
	return changed
}

// workspaceBuildLane is the lane resolveWorkspaceImage builds ws on: an explicit base image,
// a repo's own devcontainer, else the generated one. It mirrors workspaceBuiltImageKey's
// lane selection.
func workspaceBuildLane(ws types.Workspace) string {
	if b := ws.BaseImage; b != nil && b.Kind != "recommended" && strings.TrimSpace(b.Image) != "" {
		return types.ImageBuildLaneBYOI
	}
	if p, ok := workspaceProfile(ws); ok && repoOwnDevcontainerURL(ws, p) != "" {
		return types.ImageBuildLaneRepo
	}
	return types.ImageBuildLaneGenerated
}
