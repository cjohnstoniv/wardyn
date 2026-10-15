// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/dockerutil"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// ErrImageArch is runner.ErrImageArch: the engine cannot run the image.
var ErrImageArch = runner.ErrImageArch

// imageArchError carries the refusal's own words, with no sentinel text in front, so what the
// person reads is the message alone; errors.Is still finds runner.ErrImageArch.
type imageArchError struct{ msg string }

func (e *imageArchError) Error() string        { return e.msg }
func (e *imageArchError) Is(target error) bool { return target == runner.ErrImageArch }

// engineArch is the engine's normalised architecture, cached after the first
// successful Info. "" means unknown.
func (d *Driver) engineArch(ctx context.Context) string {
	d.mu.Lock()
	cached := d.engArch
	d.mu.Unlock()
	if cached != "" {
		return cached
	}
	res, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return ""
	}
	a := dockerutil.NormalizeArch(res.Info.Architecture)
	d.mu.Lock()
	d.engArch = a
	d.mu.Unlock()
	return a
}

// checkImageArch refuses a present image the engine cannot run. An image of
// another architecture is not refused on that alone: engines emulate (Rosetta,
// qemu), so archRunnable probes it. An empty architecture on either side is
// unknown, not a mismatch.
func (d *Driver) checkImageArch(ctx context.Context, ref string) error {
	res, err := d.cli.ImageInspect(ctx, ref)
	if isNotFound(err) {
		return nil // gone since the presence check: nothing to compare
	}
	if err != nil {
		return fmt.Errorf("docker: image inspect %q: %w", ref, err)
	}
	img := dockerutil.NormalizeArch(res.Architecture)
	if img == "" {
		return nil
	}
	eng := d.engineArch(ctx)
	if eng == "" || eng == img {
		return nil
	}
	return d.archRunnable(ctx, ref, img, eng)
}

// archRunnable answers whether this engine can run an image of another architecture: it refuses
// only when a probe container proves it cannot (exec format error) and otherwise warns that the
// image runs under emulation. Emulation is a property of the engine, so the verdict is cached per
// foreign architecture and the first image of an architecture decides for the rest.
//
// Known false positive, rare: an emulated image whose entrypoint execs a binary of a third
// architecture logs exec format error and is refused although the image itself is emulable.
func (d *Driver) archRunnable(ctx context.Context, ref, img, eng string) error {
	unrunnable, cached := d.archVerdicts.Load(img)
	if !cached {
		unrunnable = d.probeExecFormat(ctx, ref)
		d.archVerdicts.Store(img, unrunnable)
		if !unrunnable.(bool) {
			slog.Warn(fmt.Sprintf("running %s (%s) under emulation on an %s engine; expect it to be slower", ref, img, eng))
		}
	}
	if !unrunnable.(bool) {
		return nil
	}
	return &imageArchError{msg: fmt.Sprintf(
		"this runner's engine is %s and cannot run %s, a single-architecture %s image (exec format error). Name an image for %s or publish a multi-arch one",
		eng, ref, img, eng)}
}
