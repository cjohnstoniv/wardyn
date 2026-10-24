// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package admission

import (
	"bytes"
	"context"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// SystemHost reads this Mac with sysctl, statfs and vm_stat (no cgo).
func SystemHost() Host { return darwinHost{} }

type darwinHost struct{}

func (darwinHost) BootTime() (int64, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return 0, err
	}
	return tv.Sec*1e6 + int64(tv.Usec), nil
}

func (darwinHost) MemoryBytes() (uint64, error) { return unix.SysctlUint64("hw.memsize") }

func (darwinHost) AvailableMemoryBytes() (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/vm_stat").Output()
	if err != nil {
		return 0, err
	}
	return parseVMStat(string(out))
}

func (darwinHost) FreeDiskBytes(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	// Bavail (not Bfree): what a non-root user can actually write.
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func (darwinHost) Processes() ([]Proc, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]Proc, 0, len(kps))
	for i := range kps {
		p := &kps[i].Proc
		name := p.P_comm[:]
		if j := bytes.IndexByte(name, 0); j >= 0 {
			name = name[:j]
		}
		out = append(out, Proc{
			PID:   int(p.P_pid),
			UID:   int(kps[i].Eproc.Ucred.Uid),
			Start: p.P_starttime.Sec*1e6 + int64(p.P_starttime.Usec),
			Name:  string(name),
		})
	}
	return out, nil
}
