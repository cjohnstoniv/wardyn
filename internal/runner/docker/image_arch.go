// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/moby/moby/client"
)

// ErrImageArch is the refusal of an image whose architecture differs from the
// engine's: a single-arch image of the other architecture would otherwise
// pull fine and die with an exec-format error at its first run.
var ErrImageArch = errors.New("image architecture does not match the runner")

// normalizeArch maps the spellings Docker uses for the engine (uname style)
// and for images (GOARCH style) onto one. Unknown names pass through.
func normalizeArch(a string) string {
	a = strings.ToLower(strings.TrimSpace(a))
	switch a {
	case "x86_64", "x86-64":
		return "amd64"
	case "aarch64":
		return "arm64"
	case "armv7l", "armv6l", "armhf":
		return "arm"
	}
	return a
}

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
	a := normalizeArch(res.Info.Architecture)
	d.mu.Lock()
	d.engArch = a
	d.mu.Unlock()
	return a
}

// checkImageArch refuses a present image whose architecture differs from the
// engine's. An empty architecture on either side is unknown, not a mismatch.
func (d *Driver) checkImageArch(ctx context.Context, ref string) error {
	res, err := d.cli.ImageInspect(ctx, ref)
	if isNotFound(err) {
		return nil // gone since the presence check: nothing to compare
	}
	if err != nil {
		return fmt.Errorf("docker: image inspect %q: %w", ref, err)
	}
	img := normalizeArch(res.Architecture)
	if img == "" {
		return nil
	}
	eng := d.engineArch(ctx)
	if eng == "" || eng == img {
		return nil
	}
	return fmt.Errorf("%w: this runner is %s and %s is a single-architecture %s image; publish a multi-arch image or set a %s variant for it",
		ErrImageArch, eng, ref, img, eng)
}
