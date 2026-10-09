// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The fixed extension makes even empty, "." and ".." run prefixes ordinary
// filenames, without collisions or longer names than the casts they fence.
func fenceName(key string) string { return key + ".cast" }

func erasureDir(root *os.Root, create bool) (*os.File, error) {
	created := false
	var mode os.FileMode
	var gid int
	if create {
		info, err := root.Stat(".")
		if err != nil {
			return nil, err
		}
		// Traversal is still bounded by the recording root. Only actors who can
		// already write that root may write metadata; umask must not strand peers.
		mode = 0o555 | (info.Mode().Perm() & 0o222)
		gid = int(info.Sys().(*syscall.Stat_t).Gid)
		err = root.Mkdir(".erased", mode)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		created = err == nil
	}
	dir, err := root.OpenFile(".erased", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil || !created {
		return dir, err
	}
	info, err := dir.Stat()
	if err == nil {
		// A setgid root already supplies this group. Without setgid, preserve a
		// more privileged parent group when the creator is one of its members.
		if mode&0o070 != (mode&0o007)<<3 && int(info.Sys().(*syscall.Stat_t).Gid) != gid {
			err = dir.Chown(-1, gid)
		}
		if err == nil {
			err = dir.Chmod(mode | (info.Mode() & os.ModeSetgid))
		}
	}
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	return dir, nil
}

func controlFile(dir *os.File, name string) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_CREAT|unix.O_EXCL, 0o444)
	created := err == nil
	if errors.Is(err, os.ErrExist) {
		fd, err = unix.Openat(int(dir.Fd()), name, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.Join(err, errors.New("recording: invalid fence file"))
	}
	// Empty control files have no writable content. The parent directory gates
	// their names; read access lets a different recorder UID take the same flock.
	if created {
		if err := f.Chmod(0o444); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}

func lockFSRun(ctx context.Context, root *os.Root, key string) (*os.File, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	f, err := controlFile(dir, castRun(key)+".lock")
	if err != nil {
		return nil, err
	}
	// Never unlink a lock file: a waiter must lock the same inode as a new
	// caller. Closing the descriptor releases the lock on every exit path.
	for {
		if err = ctx.Err(); err != nil {
			break
		}
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func checkFSErased(root *os.Root, key string) error {
	dir, err := erasureDir(root, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	for _, prefix := range fenceKeys(key) {
		var stat unix.Stat_t
		err := unix.Fstatat(int(dir.Fd()), fenceName(prefix), &stat, unix.AT_SYMLINK_NOFOLLOW)
		if err == nil {
			return ErrErased
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func markFSErased(ctx context.Context, root *os.Root, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := erasureDir(root, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	marker, err := controlFile(dir, fenceName(key))
	if err != nil {
		return err
	}
	err = errors.Join(marker.Sync(), marker.Close(), dir.Sync())
	if err != nil {
		return err
	}
	// Sync the root too: .erased may have been created by this operation.
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
