// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package posture

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// mdmFile is where a device-management agent writes the posture it attests.
const mdmFile = "/etc/wardyn/posture.json"

// osVersion is /etc/os-release's VERSION_ID.
func osVersion() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "VERSION_ID="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

// diskEncrypted reports whether the root filesystem sits on dm-crypt, through
// any device-mapper layers (LVM on LUKS). A filesystem with no block device
// (btrfs subvolumes, overlay) reads as not encrypted.
func diskEncrypted() bool {
	var st syscall.Stat_t
	if err := syscall.Stat("/", &st); err != nil {
		return false
	}
	dev := uint64(st.Dev) //nolint:unconvert // Dev is uint64 on most arches, not all
	return cryptBacked(filepath.Join("/sys/dev/block", strconv.FormatUint(uint64(unix.Major(dev)), 10)+":"+strconv.FormatUint(uint64(unix.Minor(dev)), 10)), 0)
}

func cryptBacked(sysDev string, depth int) bool {
	if depth > 8 {
		return false
	}
	if uuid, err := os.ReadFile(filepath.Join(sysDev, "dm", "uuid")); err == nil && strings.HasPrefix(string(uuid), "CRYPT-") {
		return true
	}
	slaves, _ := filepath.Glob(filepath.Join(sysDev, "slaves", "*"))
	for _, s := range slaves {
		if cryptBacked(s, depth+1) {
			return true
		}
	}
	return false
}
