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

	"github.com/cjohnstoniv/wardyn/internal/runner"
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
	var describe func(ctx context.Context, ref string) (string, error)
	if d, ok := s.cfg.Runner.(runner.ImageDescriber); ok {
		describe = d.DescribeImage
	}
	return runnerArchImagesCheck(counts, runtime.GOARCH, s.cfg.AgentImages, s.cfg.AgentImageVariants, func(ref string) string {
		return describeLocalImage(ctx, describe, ref)
	})
}

// describeLocalImage says what wardynd's own image store knows about ref: its registry digest, platform
// and whether the digest is a multi-arch index. The store is wardynd's, not the registered runner's, and
// it is read without a registry call. When there is no store, no such image or the store cannot say,
// the text says it cannot tell.
func describeLocalImage(ctx context.Context, describe func(context.Context, string) (string, error), ref string) string {
	if describe != nil {
		if d, err := describe(ctx, ref); err == nil {
			return d
		}
	}
	return "cannot tell from wardynd's local image store (no such image there, or no store to read); check with docker buildx imagetools inspect " + ref
}

// runnerArchImagesCheck lists, for each architecture other than plainArch (the one the plain refs are taken
// to serve: wardynd's own) that registered runners report, the operator's image keys with no `<key>@<arch>`
// entry. It is info and never warn: wardynd cannot tell a multi-arch image from a single-arch one, so it
// cannot know the plain ref fails, and a warn would redirect the first-run gate (agentImageCheck). It
// returns no row when no such runner exists or every key has an entry. Writing the same ref as the
// variant is how an operator states that the ref is multi-arch.
//
// describe says what is known of a plain ref's resolved digest, platform and index-ness (describeLocalImage);
// the detail repeats it per key so the operator sees which image is in question. Blame on a mirror or a
// platform pin stays a hypothesis: the row never says which it is.
func runnerArchImagesCheck(counts map[string]int, plainArch string, images map[string]string, variants map[string]map[string]string, describe func(ref string) string) (SetupCheck, bool) {
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
		var sel []string
		allIndex := true
		for _, key := range missing {
			d := describe(images[key])
			allIndex = allIndex && strings.Contains(d, "a multi-arch index")
			sel = append(sel, fmt.Sprintf("%s = %s: %s", key, images[key], d))
		}
		// A mirror that copied one platform is no suspect when every selected image is a multi-arch index.
		blame := " If the ref is a mirror copy, the mirror may have copied one platform only, or a platform pin may be at fault; neither is known from here."
		if allIndex {
			blame = ""
		}
		detail = append(detail, fmt.Sprintf("%d %s %s. These WARDYN_AGENT_IMAGES keys name no %s image: %s. "+
			"A run on an %s runner uses the plain ref, which works only if it is a multi-arch image; a single-arch image runs under emulation, or is refused when the engine cannot emulate it. Selected images: %s.%s",
			n, arch, noun, arch, strings.Join(missing, ", "), arch, strings.Join(sel, "; "), blame))
		if fix == "" {
			fix = fmt.Sprintf("Add %q: \"<%s ref>\" (the same ref if it is multi-arch; used by runs on a registered %s runner), then restart wardynd. ", missing[0]+"@"+arch, arch, arch) +
				"If you built the image, build it multi-arch (docker buildx build --platform linux/amd64,linux/arm64); " +
				"if it is a published Wardyn image, check your mirror copied all platforms (docker buildx imagetools inspect <ref>)."
		}
	}
	if len(detail) == 0 {
		return SetupCheck{}, false
	}
	return SetupCheck{ID: "runner_arch_images", Label: "Runner architecture images", Status: "info", Detail: strings.Join(detail, " "), Fix: fix}, true
}
