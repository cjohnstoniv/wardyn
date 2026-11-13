// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/dockerutil"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// vmImageCacheFormat names the VM image cache's layout under <StateDir>/images. Entries made before the
// export was bound to the inspected image id sit directly under images/ and may hold another image's
// files under this one's id, so they are never read.
const vmImageCacheFormat = "v2"

// vmImages makes a Linux VM run's images present: the QEMU and proxy images, each checked as any helper image
// is (ensureImage), then the guest image (guestImage). It returns the guest image's inspected id.
func (d *Driver) vmImages(ctx context.Context, spec runner.SandboxSpec, qemuImage string) (string, error) {
	for _, img := range []string{qemuImage, d.proxyImage()} {
		if err := d.ensureImage(ctx, img, "", func() { spec.NotifyWaiting("image: Pulling: " + img) }); err != nil {
			return "", err
		}
	}
	return d.guestImage(ctx, spec)
}

// guestImage makes a Linux VM run's guest image present, pulling it when absent, and checks from its
// metadata alone that it is a linux/amd64 image, the only kind the VM boots. It never starts the image,
// as the architecture probe of the helper images (ensureImage) may: the guest's code runs only inside
// the VM. It returns the inspected image's id, which the conversion exports and caches by.
func (d *Driver) guestImage(ctx context.Context, spec runner.SandboxSpec) (string, error) {
	present, err := d.imagePresent(ctx, spec.Image)
	if err != nil {
		return "", err
	}
	if !present {
		spec.NotifyWaiting("image: Pulling: " + spec.Image)
		if err := dockerutil.PullImage(ctx, d.cli, spec.Image, "docker"); err != nil {
			return "", pullFailure(spec.Image, err)
		}
	}
	insp, err := d.cli.ImageInspect(ctx, spec.Image)
	if err != nil {
		return "", fmt.Errorf("docker: inspect the VM image: %w", err)
	}
	// Metadata the image does not carry is no refusal, as for any image (checkImageArch): the VM then fails
	// to boot it. An architecture it does carry must be amd64. NormalizeArch knows amd64 and arm64 only and
	// answers "" for every other one, so "" from it is not "unknown" here.
	if goos, arch := insp.Os, insp.Architecture; (goos != "" && goos != "linux") || (arch != "" && dockerutil.NormalizeArch(arch) != "amd64") {
		return "", &imageArchError{msg: fmt.Sprintf("docker: the VM image %s is %s/%s, but a Linux VM sandbox boots only linux/amd64 images: "+
			"use an x86-64 Linux image for this run", spec.Image, insp.Os, insp.Architecture)}
	}
	return insp.ID, nil
}
