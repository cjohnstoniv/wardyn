// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"encoding/json"
	"fmt"
)

// DiskInfo is what `qemu-img info --output=json` says about a Windows image's disk.
type DiskInfo struct {
	Format              string `json:"format"`
	VirtualSize         int64  `json:"virtual-size"`
	BackingFilename     string `json:"backing-filename"`
	FullBackingFilename string `json:"full-backing-filename"`
	FormatSpecific      struct {
		Type string `json:"type"`
		Data struct {
			DataFile string `json:"data-file"`
		} `json:"data"`
	} `json:"format-specific"`
}

// ParseDiskInfo reads qemu-img's answer and refuses a disk that is not a standalone qcow2: one that
// names a backing file or an external data file would have QEMU open a path the image chose.
func ParseDiskInfo(b []byte) (DiskInfo, error) {
	var di DiskInfo
	if err := json.Unmarshal(b, &di); err != nil {
		return di, fmt.Errorf("qemu: read the disk's qemu-img info: %w", err)
	}
	switch {
	case di.Format != "qcow2":
		return di, fmt.Errorf("qemu: the image's disk is %q, not qcow2", di.Format)
	case di.BackingFilename != "" || di.FullBackingFilename != "":
		return di, fmt.Errorf("qemu: the image's disk names a backing file (%q); register a standalone qcow2", di.BackingFilename)
	case di.FormatSpecific.Data.DataFile != "":
		return di, fmt.Errorf("qemu: the image's disk names an external data file (%q); register a standalone qcow2", di.FormatSpecific.Data.DataFile)
	case di.VirtualSize <= 0:
		return di, fmt.Errorf("qemu: the image's disk has no size")
	}
	return di, nil
}
