// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/mount"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/vm/qemu"
)

// winVMImageCacheFormat names the Windows image cache under <StateDir>/images: one standalone
// disk.qcow2 per inspected image id, apart from the Linux VM cache.
const winVMImageCacheFormat = "win1"

func (c VMConfig) winImageDir(id string) string {
	return filepath.Join(c.StateDir, "images", winVMImageCacheFormat, id)
}

// winVMImage is the converted disk of the registered Windows image guestImage inspected, imageID, made
// on first use and cached by that id. The image is an OCI containerDisk holding exactly one
// /disk/*.qcow2: it is exported to a tar as a Linux guest image is, then wardyn-winvm-disk copies the
// qcow2 out and describes it (qemu-img info) in one prep step, the description is checked here, and a
// second prep step writes the standalone copy. A disk that names a backing file or an external data
// file is refused before anything is converted, so QEMU never opens a path the image chose.
func (d *Driver) winVMImage(ctx context.Context, spec runner.SandboxSpec, imageID string) (string, error) {
	vc := d.vmConfig()
	id := strings.TrimPrefix(imageID, "sha256:")
	if id == "" || strings.ContainsAny(id, "/.") {
		return "", fmt.Errorf("docker: the Windows image has no usable id %q", imageID)
	}
	dir := vc.winImageDir(id)
	if _, err := os.Stat(filepath.Join(dir, qemu.WinDisk)); err == nil {
		return dir, nil
	}
	spec.NotifyWaiting("image: converting for a Windows VM: " + spec.Image)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", fmt.Errorf("docker: VM image cache: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), id+".tmp-")
	if err != nil {
		return "", fmt.Errorf("docker: VM image cache: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := d.exportImage(ctx, spec, imageID, filepath.Join(tmp, "rootfs.tar")); err != nil {
		return "", err
	}
	if err := vc.chown(tmp, filepath.Join(tmp, "rootfs.tar")); err != nil {
		return "", err
	}
	out := []mount.Mount{{Type: mount.TypeBind, Source: tmp, Target: "/out"}}
	if err := d.runOneShot(ctx, spec.RunID, "image", []string{"wardyn-winvm-disk", "info"}, out); err != nil {
		return "", err
	}
	info, err := os.ReadFile(filepath.Join(tmp, "info.json"))
	if err != nil {
		return "", fmt.Errorf("docker: the Windows image's disk was not described: %w", err)
	}
	if _, err := qemu.ParseDiskInfo(info); err != nil {
		return "", fmt.Errorf("docker: the Windows image %s: %w", spec.Image, err)
	}
	if err := d.runOneShot(ctx, spec.RunID, "image", []string{"wardyn-winvm-disk", "convert"}, out); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(tmp, qemu.WinDisk)); err != nil {
		return "", fmt.Errorf("docker: the Windows image conversion made no %s", qemu.WinDisk)
	}
	// Atomic: a concurrent conversion of the same image that finished first wins, and ours is dropped.
	if err := os.Rename(tmp, dir); err != nil {
		if _, serr := os.Stat(filepath.Join(dir, qemu.WinDisk)); serr != nil {
			return "", fmt.Errorf("docker: VM image cache: %w", err)
		}
	}
	return dir, nil
}
