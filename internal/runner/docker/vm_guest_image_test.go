// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/vm/qemu"
)

// A Linux VM run's guest image is checked by its metadata and never started on the host: a foreign
// architecture is refused before any container is made from it, where the helper images' probe would
// have started it. An image of the host's own platform boots as before.
func TestVMGuestImageIsNeverStartedOnTheHost(t *testing.T) {
	r := newVMRig(t)
	r.f.info.Architecture = "x86_64"
	r.f.archProbeExit, r.f.archProbeLog = 255, execFormatLog
	spec := vmSpec()
	// riscv64 is one of the architectures dockerutil.NormalizeArch does not name.
	for _, arch := range []string{"arm64", "riscv64"} {
		r.f.mu.Lock()
		r.f.imageArch[vmTestImage] = arch
		r.f.mu.Unlock()
		spec.RunID = uuid.New()
		_, err := r.d.CreateSandbox(context.Background(), spec)
		if !errors.Is(err, runner.ErrImageArch) || !strings.Contains(err.Error(), "linux/amd64") {
			t.Fatalf("a %s guest image: err = %v, want an image-architecture refusal naming linux/amd64", arch, err)
		}
	}
	r.f.mu.Lock()
	for name, c := range r.f.containers {
		if c.cfg != nil && c.cfg.Image == vmTestImage {
			t.Errorf("container %s was made from the guest image on the host", name)
		}
	}
	exports := r.f.exports
	r.f.imageArch[vmTestImage] = "amd64"
	r.f.mu.Unlock()
	if exports != 0 {
		t.Errorf("%d exports: a refused guest image was converted", exports)
	}

	spec.RunID = uuid.New()
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("a linux/amd64 guest image on an x86-64 host: %v", err)
	}
	t.Cleanup(func() { _ = r.d.KillSandbox(context.Background(), sb.Ref) })
}

// The conversion exports the image the run inspected, by its id, even when its tag moves to another image
// before the export, and the cache made before exports were bound to the id is never read.
func TestVMImageExportsTheImageItInspected(t *testing.T) {
	r := newVMRig(t)
	hexA := strings.Repeat("a", 64)
	idA, idB := "sha256:"+hexA, "sha256:"+strings.Repeat("b", 64)
	r.f.imageIDs = map[string]string{vmTestImage: idA}
	r.f.images[idA], r.f.images[idB] = true, true
	old := filepath.Join(r.vc.StateDir, "images", hexA)
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, qemu.RootFS), []byte("another image's files"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := vmSpec()
	spec.RunID = uuid.New()
	export := vmPrepName(spec.RunID, "export")
	r.f.onCreate = func(name string) {
		if name == export {
			r.f.mu.Lock()
			r.f.imageIDs[vmTestImage] = idB // a refresh moves the tag between the inspect and the export
			r.f.mu.Unlock()
		}
	}
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.d.KillSandbox(context.Background(), sb.Ref) })
	c := r.container(t, export)
	r.f.mu.Lock()
	got := r.f.imageIDFor(c.cfg.Image)
	exports := r.f.exports
	r.f.mu.Unlock()
	if got != idA {
		t.Fatalf("the export was made from %q, image %s; want the inspected %s", c.cfg.Image, got, idA)
	}
	if exports != 1 {
		t.Fatalf("%d exports: the cache entry made before exports were bound to the id was read", exports)
	}
	if _, err := os.Stat(filepath.Join(r.vc.imageDir(hexA), qemu.RootFS)); err != nil {
		t.Fatalf("no converted base cached under the inspected id: %v", err)
	}
}
