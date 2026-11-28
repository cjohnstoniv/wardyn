// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/mount"
)

// makeShareMountpoints makes, in the host folder of each writable folder share, the folder that every other share
// whose target is inside it mounts on, as a container's runtime makes it. It stays inside that folder (os.Root),
// whatever links the folder holds. A read-only share or a volume gets none: the guest needs the folder there already,
// as it always has.
func makeShareMountpoints(shares []mount.Mount) error {
	for _, outer := range shares {
		if outer.Type != mount.TypeBind || outer.ReadOnly {
			continue
		}
		for _, inner := range shares {
			rel, err := filepath.Rel(outer.Target, inner.Target)
			if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
				continue
			}
			root, err := os.OpenRoot(outer.Source)
			if err != nil {
				return fmt.Errorf("docker: the mountpoint of %s: %w", inner.Target, err)
			}
			err = root.MkdirAll(rel, 0o755)
			_ = root.Close()
			if err != nil {
				return fmt.Errorf("docker: the mountpoint of %s inside %s: %w", inner.Target, outer.Target, err)
			}
		}
	}
	return nil
}
