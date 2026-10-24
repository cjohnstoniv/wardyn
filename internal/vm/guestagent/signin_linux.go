// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// CheckNoSwap refuses a guest with any swap on (a swap partition, a swap file or zram): every line
// of the kernel's swap list after its header is one. The sign-in store is a tmpfs, which the kernel
// pages out only to swap.
func CheckNoSwap(swaps string) error {
	f, err := os.Open(swaps)
	if err != nil {
		return fmt.Errorf("read the guest's swap list: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var on []string
	for n := 0; sc.Scan(); n++ {
		if line := strings.TrimSpace(sc.Text()); n > 0 && line != "" {
			on = append(on, strings.Fields(line)[0])
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read the guest's swap list: %w", err)
	}
	if len(on) > 0 {
		return fmt.Errorf("the guest has swap on (%s): the image contract allows no swap and no zram", strings.Join(on, ", "))
	}
	return nil
}

// MountSignInStore mounts an empty tmpfs, owned by the agent user and mode 0700, over each of
// SignInPaths under home, read-only when off. A missing path is made first, its new directories
// the agent's; a path that is a symbolic link, or under one, is refused.
func MountSignInStore(home string, off bool) error {
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV)
	if off {
		flags |= unix.MS_RDONLY
	}
	opts := fmt.Sprintf("mode=0700,uid=%d,gid=%d", AgentUID, AgentGID)
	for _, rel := range SignInPaths {
		p := filepath.Join(home, rel)
		if err := mkdirAgent(home, rel); err != nil {
			return err
		}
		if real, err := filepath.EvalSymlinks(p); err != nil || real != p {
			return fmt.Errorf("%s is a symbolic link or under one", p)
		}
		if err := unix.Mount("tmpfs", p, "tmpfs", flags, opts); err != nil {
			return fmt.Errorf("mount a tmpfs at %s: %w", p, err)
		}
	}
	return nil
}

// mkdirAgent makes home/rel, each directory it adds owned by the agent user.
func mkdirAgent(home, rel string) error {
	p := home
	for _, part := range strings.Split(rel, "/") {
		p = filepath.Join(p, part)
		err := os.Mkdir(p, 0o700)
		switch {
		case errors.Is(err, os.ErrExist):
			continue
		case err != nil:
			return err
		}
		if err := os.Lchown(p, AgentUID, AgentGID); err != nil {
			return err
		}
	}
	return nil
}
