// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// failingDevcontainerBuilder fails the repo's own devcontainer build.
type failingDevcontainerBuilder struct{ fakeImageBuilder }

func (failingDevcontainerBuilder) BuildDevcontainer(context.Context, string, string, string, io.Writer) (BuiltImage, error) {
	return BuiltImage{}, errors.New("build failed")
}

// A repo-own-devcontainer workspace whose image was not built (no builder
// configured, or the build failed) runs the convention image: the caveat says
// so, and does not claim the image comes from the repo's devcontainer (#2286).
func TestLaunchRecordRun_RepoDevcontainerFallbackCaveat(t *testing.T) {
	for name, builder := range map[string]ImageBuilder{
		"no builder":   nil,
		"build failed": failingDevcontainerBuilder{},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			profile := workspacescan.WorkspaceProfile{
				Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
				HasDevcontainer: true,
			}
			ws := types.Workspace{
				ID:      uuid.New(),
				Status:  types.WorkspaceScanned,
				Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/widgets", Ref: "main"}},
				Profile: mustJSON(profile),
			}
			fake := newRecordLLMModeStore(ws)
			cfg := baseTestConfig(h, fake)
			cfg.Runner = &fakeRunner{}
			cfg.Broker = h.broker
			cfg.ImageBuilder = builder
			srv := New(cfg)

			if _, _, err := srv.launchRecordRun(context.Background(), "alice@example.com", ws, "build", "build", false); err != nil {
				t.Fatalf("launchRecordRun: %v", err)
			}
			caveats := strings.Join(fake.lastRecord().Caveats, "\n")
			if !strings.Contains(caveats, "was not built") || strings.Contains(caveats, "is built from the repo's OWN devcontainer") {
				t.Fatalf("session caveats = %q, want the not-built fallback note", caveats)
			}
		})
	}
}
