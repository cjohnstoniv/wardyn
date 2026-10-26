// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package admission

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"time"
	"unsafe"

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

var _ bootSessioner = darwinHost{}

func (darwinHost) BootSession() (string, error) { return unix.Sysctl("kern.bootsessionuuid") }

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
	euid := os.Geteuid()
	out := make([]Proc, 0, len(kps))
	for i := range kps {
		p := &kps[i].Proc
		name := p.P_comm[:]
		if j := bytes.IndexByte(name, 0); j >= 0 {
			name = name[:j]
		}
		pr := Proc{
			PID:   int(p.P_pid),
			UID:   int(kps[i].Eproc.Ucred.Uid),
			Start: p.P_starttime.Sec*1e6 + int64(p.P_starttime.Usec),
			Name:  string(name),
		}
		// Only this user's: the ledger orders no other user's processes,
		// and the kernel refuses them to a non-root caller anyway.
		if pr.UID == euid {
			pr.MonoStart = monoStart(pr.PID)
		}
		out = append(out, pr)
	}
	return out, nil
}

// From XNU: PROC_INFO_CALL_PIDRUSAGE (bsd/sys/proc_info_private.h) and
// RUSAGE_INFO_V1 (bsd/sys/resource.h). struct rusage_info_v1 there is
// uint8_t ri_uuid[16] followed by sixteen uint64_t fields, 144 bytes; read as
// 8-byte words, ri_uuid is words 0 and 1 and ri_proc_start_abstime, the ninth
// uint64_t, is word 10 (byte offset 80).
const (
	procInfoCallPIDRusage = 9
	rusageInfoV1          = 1
	rusageInfoV1Words     = 18
	riProcStartAbstime    = 10
)

// monoStart is pid's ri_proc_start_abstime, read as libproc's
// proc_pid_rusage(pid, RUSAGE_INFO_V1, &ri) does it:
// __proc_info(PROC_INFO_CALL_PIDRUSAGE, pid, flavor, 0, &ri, 0), a plain
// system call, so no cgo. XNU stamps it at fork together with the calendar
// start (kern_fork.c, microtime_with_abstime) and keeps both across exec. 0
// when it cannot be read: another user's process (EPERM), or a pid gone since
// the snapshot. A pid reused since the snapshot reads its new process's later
// start, which only keeps a slot longer.
func monoStart(pid int) uint64 {
	var ri [rusageInfoV1Words]uint64
	_, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDRusage, uintptr(pid), rusageInfoV1, 0,
		uintptr(unsafe.Pointer(&ri[0])), 0)
	if errno != 0 {
		return 0
	}
	return ri[riProcStartAbstime]
}
