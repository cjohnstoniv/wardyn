// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

// Reuse of a per-run build. A run that names an image to wrap has no workspace to keep its image on, so it built a
// fresh image every time. A reusable build is tagged by a key over everything the build reads, and a later build with
// the same key and scope returns that image while the server process that built it keeps running.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/containerd/errdefs"
)

const (
	// reuseKeyLabel holds the key a reusable image was built for. A tag is reused only when its image carries the
	// key asked for.
	reuseKeyLabel = "wardyn.envbuild.reuse-key"
	// byoiReuseRepo keeps the wardyn-byoi/ prefix, which arms dispatch's harness selftest for a wrapped image.
	byoiReuseRepo = "wardyn-byoi/reuse"
)

// Reuse is what a reusable build is keyed to, and what it tells its caller.
type Reuse struct {
	// Scope keys the build to one creator: only a later build with the same scope reuses it. Empty is refused.
	Scope string
	// Hold is called with the output tag before the tag is looked up. Nil does nothing.
	Hold func(ref string)
	// Announce is called once just before a build starts, never when a build is reused. Nil does nothing.
	Announce func()
}

// reuseKey is the hex SHA-256 of the length-prefixed parts, so no part can run into the next.
func reuseKey(parts ...string) string {
	h := sha256.New()
	var n [4]byte
	for _, p := range parts {
		binary.BigEndian.PutUint32(n[:], uint32(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// reusable reports the ID of the image tag names when this process built that image for key and the tag still
// names it. A tag this process did not build is never trusted, whatever its label says. An absent tag, another image under the tag, or one labelled for another key
// is not reusable. Any other inspect failure is an error, so a daemon fault fails the build rather than building
// over a tag it could not read.
func (b *Builder) reusable(ctx context.Context, tag, key string) (string, bool, error) {
	want, ok := b.reuseIDs.Load(key)
	if !ok {
		return "", false, nil
	}
	res, err := b.cli.ImageInspect(ctx, tag)
	if errdefs.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("envbuild: inspect reusable image %q: %w", tag, err)
	}
	if res.ID != want.(string) || res.Config == nil || res.Config.Labels[reuseKeyLabel] != key {
		return "", false, nil
	}
	return res.ID, true, nil
}

// reuseOrBuild holds tag, returns the image under it when it was built for key, and otherwise announces and builds.
func (b *Builder) reuseOrBuild(ctx context.Context, tag, key, baseDigest string, reuse Reuse, build func() (Built, error)) (Built, bool, error) {
	if reuse.Hold != nil {
		reuse.Hold(tag)
	}
	id, ok, err := b.reusable(ctx, tag, key)
	if err != nil {
		return Built{}, false, err
	}
	if ok {
		return Built{Ref: tag, ID: id, BaseDigest: baseDigest}, true, nil
	}
	if reuse.Announce != nil {
		reuse.Announce()
	}
	built, err := build()
	if err != nil {
		return built, false, err
	}
	if imageIDPattern.MatchString(built.ID) {
		b.reuseIDs.Store(key, built.ID)
	} else {
		b.reuseIDs.Delete(key) // the daemon named no image ID for this build: nothing to bind the tag to
	}
	return built, false, nil
}

// FinalizeBaseReused is FinalizeBase for one creator's run. The base is ensured and passes the wrap-safety preflight
// every time; the wrap is keyed to the exact build context, whose FROM line is the inspected image ID, and to
// reuse.Scope. An image already built for that key is returned with reused true.
func (b *Builder) FinalizeBaseReused(ctx context.Context, baseRef string, reuse Reuse, logSink io.Writer) (Built, bool, error) {
	if reuse.Scope == "" {
		return Built{}, false, errors.New("envbuild: a reusable build needs a scope")
	}
	toolsDir, err := b.validateToolsDir()
	if err != nil {
		return Built{}, false, err
	}
	timeout := b.BuildTimeout
	if timeout <= 0 {
		timeout = defaultBuildTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := b.ensureImage(ctx, baseRef); err != nil {
		return Built{}, false, baseUnavailable(baseRef, false, err)
	}
	if logSink == nil {
		logSink = b.DefaultLogSink
	}
	in, err := b.prepareFinalize(ctx, baseRef, toolsDir, false)
	if err != nil {
		return Built{}, false, err
	}
	sum := sha256.Sum256(in.context)
	key := reuseKey("byoi-wrap/v1", reuse.Scope, hex.EncodeToString(sum[:]))
	tag := byoiReuseRepo + ":" + key
	return b.reuseOrBuild(ctx, tag, key, in.baseDigest, reuse, func() (Built, error) {
		return b.runFinalize(ctx, in, tag, map[string]string{reuseKeyLabel: key}, logSink)
	})
}
