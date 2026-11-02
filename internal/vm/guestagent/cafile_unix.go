// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package guestagent

import (
	"bytes"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// writeCAFull writes dst as the file at roots followed by ca, 0644 and root's: every exec reads it.
func writeCAFull(roots string, ca []byte, dst string) error {
	sys, err := os.ReadFile(roots)
	if err != nil {
		return fmt.Errorf("the system roots: %w", err)
	}
	var b bytes.Buffer
	b.Write(sys)
	if !bytes.HasSuffix(sys, []byte("\n")) {
		b.WriteByte('\n')
	}
	b.Write(ca)
	// Created fresh and never followed, as a managed file is (writeFile): a link left at the temp name
	// by the agent user cannot redirect root's write.
	tmp := dst + ".wardyn-tmp"
	_ = os.Remove(tmp)
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	_, err = fh.Write(b.Bytes())
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
