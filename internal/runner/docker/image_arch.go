// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

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
// unknown, not a mismatch. runtime is the runtime ref will run under ("" is the engine default).
func (d *Driver) checkImageArch(ctx context.Context, ref, runtime string) error {
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
	return d.archRunnable(ctx, ref, img, eng, runtime)
}

// archRunnable answers whether this engine can run an image of another architecture: it refuses
// only when a probe container proves it cannot (exec format error) and otherwise warns that the
// image runs under emulation, probing under runtime, the runtime the image will run under. Emulation
// is a property of the engine and runtime, so a runnable verdict is cached per foreign architecture
// and runtime. A refusal can rest on the image's own output, so it is never cached and the next image
// of that architecture is probed afresh, as after an inconclusive probe (see probeExecFormat).
//
// Known false positive, rare: an emulated image whose entrypoint execs a binary of a third
// architecture logs exec format error and is refused although the image itself is emulable.
func (d *Driver) archRunnable(ctx context.Context, ref, img, eng, runtime string) error {
	kind, cached := archInconclusive, false
	key := archVerdictKey{img, runtime}
	if v, ok := d.archVerdicts.Load(key); ok {
		kind, cached = v.(archVerdictKind), true
	} else {
		kind = d.probeExecFormat(ctx, ref, runtime)
		if kind == archRunnable {
			d.archVerdicts.Store(key, kind)
		}
	}
	if kind != archUnrunnable {
		if !cached {
			// This runner never learns which setting supplied ref, so the text says how to find it.
			source := fmt.Sprintf("If this is an agent image, it comes from the WARDYN_AGENT_IMAGES entry whose value is %s, or is the published default if no entry has it; add a <key>@%s entry (used by runs on a registered %s runner) or build it multi-arch (docker buildx build --platform linux/amd64,linux/arm64)", ref, eng, eng)
			if ref == d.cfg.ProxyImage {
				source = fmt.Sprintf("This is the proxy image, set by WARDYN_PROXY_IMAGE or, for a registered %s runner, WARDYN_PROXY_IMAGE_%s; point one of them at a %s image or build it multi-arch (docker buildx build --platform linux/amd64,linux/arm64)", eng, strings.ToUpper(eng), eng)
			}
			slog.Warn(fmt.Sprintf("running %s (%s image) under emulation on an %s engine; expect it to be slower. %s. If it is a published Wardyn image, check your mirror copied all platforms (docker buildx imagetools inspect %s)",
				ref, img, eng, source, ref))
		}
		return nil
	}
	return &imageArchError{msg: fmt.Sprintf(
		"this runner's engine is %s and cannot run %s, a single-architecture %s image (exec format error). Name an image for %s or publish a multi-arch one",
		eng, ref, img, eng)}
}
