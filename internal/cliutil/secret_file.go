// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package cliutil

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// ReadSecretFile reads the secret file path that setting names and returns
// its raw bytes; each caller trims them its own way. The mode is checked on
// the OPENED descriptor (CheckSecretFileMode), so the file checked is the
// file read, however often the caller re-reads a rotated file. Every error
// names setting and path, never the content.
func ReadSecretFile(setting, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s=%q is unreadable: %w", setting, path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%s=%q is unreadable: %w", setting, path, err)
	}
	owner := -1
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		owner = int(st.Uid)
	}
	if err := CheckSecretFileMode(setting, path, fi.Mode().Perm(), owner, os.Geteuid()); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("%s=%q is unreadable: %w", setting, path, err)
	}
	return raw, nil
}

// CheckSecretFileMode is the mode rule, split out so every delivery shape is
// testable without chown. owner is the file's uid (-1 when unknown).
//
// Group- or world-WRITABLE is always refused: anyone in that set could swap the
// secret before the next read, and no supported delivery produces it.
//
// Other-READABLE is refused only on a file the process's own non-root uid owns —
// the hand-made host file, which `chmod 640` fixes. It stays allowed everywhere a
// supported mechanism produces it, which is why this differs from
// WARDYN_DAEMON_PROXY_SECRET's 0600 rule: a Secret volume is root-owned 0440
// (group-read added by the kubelet under the chart's fsGroup), a Secrets Store
// CSI file is root-owned 0644 and reachable by a non-root reader only through
// the other-read bit, and Vault Agent writes 0644 as its own uid by default.
func CheckSecretFileMode(setting, path string, perm os.FileMode, owner, euid int) error {
	if perm&0o022 != 0 {
		return fmt.Errorf("%s=%q is mode %04o — a group- or world-writable secret file lets someone else replace it; remove the write bits (chmod 640)", setting, path, perm)
	}
	if perm&0o004 != 0 && euid != 0 && owner == euid {
		return fmt.Errorf("%s=%q is mode %04o and owned by wardynd's own uid %d — any local user can read it; chmod 640 it (under Vault Agent, set vault.hashicorp.com/agent-inject-perms-<name>: \"0440\")", setting, path, perm, euid)
	}
	return nil
}
