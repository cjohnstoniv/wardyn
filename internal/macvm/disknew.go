// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// The guest agent formats a disk drive whose first 4 KiB are all zero (guestagent's MacDisks.Attach): the
// state of an image MakeDiskImage has just made. An existing drive whose first 4 KiB were zeroed looks the
// same, so the runner lets a guest see such an image only while the mark MakeDiskImage left beside it vouches
// for that very file. The mark is used up once a guest has the drive mounted.

// diskHeadBytes is how much of a disk drive the guest agent reads to find it blank.
const diskHeadBytes = 4096

// newDiskMark is the mark beside the disk image at path. It holds the image's device and inode, so it
// vouches for that one file: an image put at the path later is not new by it.
func newDiskMark(path string) string { return path + ".new" }

// diskFileID is fi's device and inode as a mark holds them, "" when the system gives neither.
func diskFileID(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d %d\n", st.Dev, st.Ino)
}

// markNewDisk writes the mark of the image f, just made at path.
func markNewDisk(path string, f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	id := diskFileID(fi)
	if id == "" {
		return errors.New("no device and inode to mark it by")
	}
	return os.WriteFile(newDiskMark(path), []byte(id), 0o600)
}

// CheckDiskImage refuses the disk image at path, before any guest sees it, when the guest agent would
// format it but the runner did not make it new: its first 4 KiB are zero and no mark of this file is
// beside it, or it is shorter than 4 KiB. An image that does not start with 4 KiB of zeros has been
// formatted, so a mark still beside it is used up here. No image is no refusal: MakeDiskImage makes a new
// one, with its mark. The caller holds the image's lock.
func CheckDiskImage(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("macvm: disk image: %w", err)
	}
	defer f.Close()
	head := make([]byte, diskHeadBytes)
	switch n, err := io.ReadFull(f, head); {
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return diskNotFormatted(fmt.Sprintf("is %d bytes long, shorter than the 4 KiB a formatted drive starts with", n))
	case err != nil:
		return fmt.Errorf("macvm: disk image: %w", err)
	case bytes.Count(head, []byte{0}) != diskHeadBytes:
		return UseUpNewDisk(path)
	}
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("macvm: disk image: %w", err)
	}
	mark, err := os.ReadFile(newDiskMark(path))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("macvm: disk image mark: %w", err)
	}
	if id := diskFileID(fi); err == nil && id != "" && string(mark) == id {
		return nil
	}
	return diskNotFormatted("starts with 4 KiB of zeros, where a formatted drive keeps its partition map, and this Mac has no record that the runner made it new")
}

// UseUpNewDisk removes the mark beside the disk image at path: the drive has been formatted (a guest has it
// mounted, or it no longer starts with 4 KiB of zeros), so a blank image there later is damaged, not new.
// No mark is no error.
func UseUpNewDisk(path string) error {
	if err := os.Remove(newDiskMark(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("macvm: disk image mark: %w", err)
	}
	return nil
}

// diskNotFormatted is CheckDiskImage's refusal; why says what is wrong with the image.
func diskNotFormatted(why string) error {
	return errors.New("Wardyn did not format this disk drive, so any files it holds are still in its image. The image on this Mac " + why +
		". To start again with an empty drive, delete this drive and create a new one; deleting it asks this Mac to delete the image.")
}
