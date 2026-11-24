// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ImageReuse keys a per-run build to the creator who asked for it, and carries the launch's hold and its
// "building" notice to the builder.
type ImageReuse struct {
	// Scope is the run's creator: only a later run of the same creator reuses the build.
	Scope string
	// Hold holds the output tag for this launch (runner.HoldImage) before the builder looks it up.
	Hold func(ref string)
	// Announce marks the run as building. The builder calls it only when it builds.
	Announce func()
}

// ReusingImageBuilder is an ImageBuilder that can return an earlier per-run build made from the same inputs. It is
// optional: a builder without it builds every run's image under the run's own tag.
type ReusingImageBuilder interface {
	// FinalizeBaseReused wraps baseRef as FinalizeBase does, keyed to the base image it inspected, the wrap and
	// the runner tools. reused is true when an earlier wrap for the same key and scope was returned.
	FinalizeBaseReused(ctx context.Context, baseRef string, reuse ImageReuse, logSink io.Writer) (built BuiltImage, reused bool, err error)
}

// runImageReuse is the builder that may reuse run's build and the reuse it is given. ok is false when the builder
// cannot reuse or the run has no creator. ctx is the launch's: it carries the hold scope and the notice's store.
func (s *Server) runImageReuse(ctx context.Context, run types.AgentRun) (ReusingImageBuilder, ImageReuse, bool) {
	rb, ok := s.cfg.ImageBuilder.(ReusingImageBuilder)
	if !ok || run.CreatedBy == "" {
		return nil, ImageReuse{}, false
	}
	return rb, ImageReuse{
		Scope:    run.CreatedBy,
		Hold:     func(ref string) { runner.HoldImage(ctx, ref) },
		Announce: s.announceImageBuild(ctx, run.ID),
	}, true
}

// wrapRunImage wraps base for run: a reusable wrap when the builder and the run allow one, else a fresh wrap under
// outTag. buildCtx is the build's context (trackRunBuild); ctx is the launch's.
func (s *Server) wrapRunImage(ctx, buildCtx context.Context, run types.AgentRun, base, outTag string) (BuiltImage, bool, error) {
	if rb, reuse, ok := s.runImageReuse(ctx, run); ok {
		return rb.FinalizeBaseReused(buildCtx, base, reuse, nil)
	}
	s.announceImageBuild(ctx, run.ID)()
	built, err := s.cfg.ImageBuilder.FinalizeBase(buildCtx, base, outTag, nil, false)
	return built, false, err
}

// reusedFields marks a run.build success row whose image was reused.
func reusedFields(data map[string]any, reused bool) map[string]any {
	if reused {
		data["cache_hit"] = true
	}
	return data
}
