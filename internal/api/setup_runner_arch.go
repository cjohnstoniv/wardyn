// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// runnerArchCounter counts the registered, non-revoked runners per reported CPU architecture.
type runnerArchCounter interface {
	RunnerArchCounts(ctx context.Context) (map[string]int, error)
}

var _ runnerArchCounter = store.PG{}

// runnerArchImagesRow is the runner_arch_images row of setup status: the WARDYN_AGENT_IMAGES keys that name
// no image for an architecture some registered runner reports. Operator callers only (it reads Postgres).
func (s *Server) runnerArchImagesRow(ctx context.Context) (SetupCheck, bool) {
	rc, ok := s.cfg.Store.(runnerArchCounter)
	if !ok || len(s.cfg.AgentImages) == 0 {
		return SetupCheck{}, false
	}
	counts, err := rc.RunnerArchCounts(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: setup status: count runner architectures", slog.Any("err", err))
		return SetupCheck{}, false
	}
	return runnerArchImagesCheck(counts, runtime.GOARCH, s.cfg.AgentImages, s.cfg.AgentImageVariants)
}

// runnerArchImagesCheck lists, for each architecture other than plainArch (the one the plain refs are taken
// to serve: wardynd's own) that registered runners report, the operator's image keys with no `<key>@<arch>`
// entry. It is info and never warn: wardynd cannot tell a multi-arch image from a single-arch one, so it
// cannot know the plain ref fails, and a warn would redirect the first-run gate (agentImageCheck). It
// returns no row when no such runner exists or every key has an entry. Writing the same ref as the
// variant is how an operator states that the ref is multi-arch.
func runnerArchImagesCheck(counts map[string]int, plainArch string, images map[string]string, variants map[string]map[string]string) (SetupCheck, bool) {
	var detail []string
	var fix string
	for _, arch := range []string{"amd64", "arm64"} {
		n := counts[arch]
		if n == 0 || arch == plainArch {
			continue
		}
		var missing []string
		for key := range images {
			if _, has := variants[arch][key]; !has {
				missing = append(missing, key)
			}
		}
		if len(missing) == 0 {
			continue
		}
		slices.Sort(missing)
		noun := "runners"
		if n == 1 {
			noun = "runner"
		}
		detail = append(detail, fmt.Sprintf("%d %s %s. These WARDYN_AGENT_IMAGES keys name no %s image: %s. A run on an %s runner uses the plain ref, which works only if it is a multi-arch image; a single-arch image runs under emulation, or is refused when the engine cannot emulate it.",
			n, arch, noun, arch, strings.Join(missing, ", "), arch))
		if fix == "" {
			fix = fmt.Sprintf("Add %q: \"<%s ref>\" (the same ref if it is multi-arch), then restart wardynd. If you built the image, build it multi-arch (docker buildx build --platform linux/amd64,linux/arm64); if it is a published Wardyn image, check your mirror copied all platforms (docker buildx imagetools inspect <ref>).", missing[0]+"@"+arch, arch)
		}
	}
	if len(detail) == 0 {
		return SetupCheck{}, false
	}
	return SetupCheck{ID: "runner_arch_images", Label: "Runner architecture images", Status: "info", Detail: strings.Join(detail, " "), Fix: fix}, true
}
