// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gitPushWithheld is which forges this run may not push to: its effective capabilities for
// GitHub, or for Azure DevOps, leave out code_write.
type gitPushWithheld struct{ github, ado bool }

// gitPushWithheldFor reads the decision at create and at the queue's start (wireRunGrants runs at
// both, so the walls read here are the run owner's current ones, and the wall preset the run's frozen
// copy). Walls that cannot be read refuse the run, as the capability meet's do.
func (s *Server) gitPushWithheldFor(ctx context.Context, run types.AgentRun, spec types.RunPolicySpec) (gitPushWithheld, *runRefusal) {
	walls, err := s.personalWalls(ctx, run.CreatedBy)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: read the run owner's walls for its git pushes; the run is refused", slog.Any("err", err))
		return gitPushWithheld{}, runError(http.StatusServiceUnavailable, reasonGrantWithheldUnread,
			"Your walls could not be read, so whether this run may push cannot be decided. Walls that cannot be read refuse the run; try again.")
	}
	w := gitPushWithheldBy(spec, walls, run.WallPreset)
	// The boundary's and the run's restrictions' caps: a cap that leaves out code_write keeps pushes from
	// the forge it names, whatever the policy or the walls list.
	layers := s.outerLayers(ctx, run)
	w.github = w.github || capLacks(githubCapOf(layers), ghscope.CapCodeWrite)
	w.ado = w.ado || capLacks(adoCapOf(layers), adoscope.CapCodeWrite)
	return w, nil
}

// gitPushWithheldBy is the decision on spec, the person's walls and the run's wall preset (nil for
// none). Effective is the policy's list where it names one, else each wall's own list: the git lanes
// have no provider default the walls could meet instead. Where several name a list the push needs
// code_write in each, which at create is the meet the walls' steps already made and at the queue's
// start catches walls saved since.
func gitPushWithheldBy(spec types.RunPolicySpec, walls types.PersonalWallsSpec, preset *types.WallPresetOnRun) gitPushWithheld {
	w := gitPushWithheld{
		github: pushWithheldBy(spec.GitHubCapabilities, walls.GitHubCapabilities, ghscope.CapCodeWrite),
		ado:    pushWithheldBy(spec.AzureDevOpsCapabilities, walls.AzureDevOpsCapabilities, adoscope.CapCodeWrite),
	}
	if preset != nil {
		w.github = w.github || pushWithheldBy(nil, preset.Spec.GitHubCapabilities, ghscope.CapCodeWrite)
		w.ado = w.ado || pushWithheldBy(nil, preset.Spec.AzureDevOpsCapabilities, adoscope.CapCodeWrite)
	}
	return w
}

func pushWithheldBy[C ~string](policy []C, wall *[]C, write C) bool {
	if len(policy) > 0 && !slices.Contains(policy, write) {
		return true
	}
	return wall != nil && !slices.Contains(*wall, write)
}

// hosts is the proxy's GitPushWithheld: github.com for the GitHub App lane, and each Azure DevOps
// git_pat host the run was wired. nil when no push is withheld.
func (g gitPushWithheld) hosts(adoGitHosts []string) map[string]string {
	out := map[string]string{}
	if g.github {
		out["github.com"] = "GitHub"
	}
	for _, h := range adoGitHosts {
		if g.ado {
			out[h] = "Azure DevOps"
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// readOnlyGitHubGrants lowers each github_token grant's contents permission to read, so a token
// minted from it cannot push either: the broker clamps to what the scope asks (mintGitHub). The
// GitHub CLI's grants carry capabilities, not permissions, and are left as they are.
func readOnlyGitHubGrants(gs []types.GrantSpec) []types.GrantSpec {
	out := slices.Clone(gs)
	for i, g := range out {
		var sc map[string]any
		if g.Kind != types.GrantGitHubToken || json.Unmarshal(g.Scope, &sc) != nil {
			continue
		}
		perms, _ := sc["permissions"].(map[string]any)
		if lvl, ok := perms["contents"]; !ok || lvl == "read" {
			continue
		}
		perms["contents"] = "read"
		out[i].Scope = mustJSON(sc)
	}
	return out
}
