// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/envbuild"
)

// envBuilderAdapter adapts *envbuild.Builder to api.ImageBuilder. The envbuild
// package is build-tagged "docker" (it imports the docker client), so this seam
// keeps the control-plane default build free of target-specific code.
type envBuilderAdapter struct {
	b *envbuild.Builder
}

var (
	_ api.ImageBuilder      = envBuilderAdapter{}
	_ api.ImageBuildSweeper = envBuilderAdapter{}
)

func (e envBuilderAdapter) BuildDevcontainer(ctx context.Context, repoURL, ref, outputTag string, logSink io.Writer) (string, error) {
	return e.b.Build(ctx, envbuild.BuildSpec{
		RepoURL:        repoURL,
		Ref:            ref,
		OutputImageTag: outputTag,
		LogSink:        e.tee(logSink),
	})
}

// BuildFromDevcontainerFiles builds a per-workspace image from generated
// devcontainer files (workspacescan.GenerateDevcontainer's output) via the
// git-free local-context envbuilder path. Same hardened builder, no git URL.
func (e envBuilderAdapter) BuildFromDevcontainerFiles(ctx context.Context, files map[string]string, outputTag string, logSink io.Writer) (string, error) {
	return e.b.BuildFromDevcontainerFiles(ctx, files, outputTag, e.tee(logSink))
}

// FinalizeBase wraps a user-supplied base image (BYOI) with the runner tools +
// a cleared ENTRYPOINT via the trusted finalize stage.
func (e envBuilderAdapter) FinalizeBase(ctx context.Context, baseRef, outputTag string, logSink io.Writer) (string, error) {
	return e.b.FinalizeBase(ctx, baseRef, outputTag, e.tee(logSink))
}

// SweepOrphanedBuilds satisfies api.ImageBuildSweeper: force-removes build
// containers left behind by a crashed or restarted process. Wired into
// api.Server.ReconcileOnBoot via a type assertion — envbuild is docker-tagged
// and api must stay target-agnostic, so the capability is optional.
func (e envBuilderAdapter) SweepOrphanedBuilds(ctx context.Context) error {
	return e.b.SweepOrphanedBuilds(ctx)
}

// tee combines a caller-supplied per-call log sink (e.g. the wizard Build
// step's in-memory ring, threaded down from api.ImageBuilder) with the
// builder's own DefaultLogSink (wardynd's slog, set below) so a caller
// watching one build is ADDITIVE — operator logs must not regress just
// because the wizard started watching too. A nil logSink passes through
// unchanged: envbuild.Builder's own methods already fall back to
// DefaultLogSink on nil, and teeing nil with DefaultLogSink here would just
// duplicate that same fallback.
func (e envBuilderAdapter) tee(logSink io.Writer) io.Writer {
	if logSink == nil {
		return nil
	}
	if e.b.DefaultLogSink == nil {
		return logSink
	}
	return io.MultiWriter(logSink, e.b.DefaultLogSink)
}

// newEnvBuilder constructs the devcontainer image builder. Compiled only with
// `-tags docker`. The builder runs daemonless: envbuilder pushes to the registry
// (cacheRepo) and Wardyn finalizes the image locally, so a missing cacheRepo
// fails closed.
// slogLineWriter forwards build output lines to slog so a failed build's
// reason lands in wardynd's own logs instead of vanishing with the removed
// container (the old behavior left only "exit code 1").
type slogLineWriter struct{ buf []byte }

func (w *slogLineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(string(w.buf[:i])); line != "" {
			slog.Info("envbuild: " + line)
		}
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func newEnvBuilder(envbuilderImage, cacheRepo string) (api.ImageBuilder, error) {
	b, err := envbuild.New(envbuilderImage, cacheRepo)
	if err != nil {
		return nil, err
	}
	b.DefaultLogSink = &slogLineWriter{}
	return envBuilderAdapter{b: b}, nil
}
