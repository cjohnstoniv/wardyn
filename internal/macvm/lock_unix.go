// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package macvm

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// lockFile takes a flock on path, creating it 0600. The file must be this
// user's and not a symlink: a slot lock in a directory someone else can write
// would let them stall the helper. It waits until ctx ends.
func lockFile(ctx context.Context, path string, exclusive bool) (unlock func(), err error) {
	_, unlock, err = lockFileHeld(ctx, path, exclusive)
	return unlock, err
}

// lockFileHeld is lockFile that also returns the open lock file, so a caller can tell whether the
// path still names it (a lock file that is removed leaves its holders on an unlinked inode).
func lockFileHeld(ctx context.Context, path string, exclusive bool) (f *os.File, unlock func(), err error) {
	f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("macvm: slot lock: %w", err)
	}
	fi, err := f.Stat()
	if err == nil {
		err = checkOwner(fi)
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("macvm: slot lock %s: %w", path, err)
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	for {
		err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
		if err == nil {
			return f, func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close()
			return nil, nil, fmt.Errorf("macvm: slot lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func checkOwner(fi os.FileInfo) error {
	if st, ok := fi.Sys().(*unix.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return errors.New("not owned by this user")
	}
	return nil
}

// A person can mount a disk drive on the Mac itself (`wardyn-runnerd drive open --direct`). The command
// holds the disk's lock while it does, but a flock dies with its holder and a mount doesn't. So the
// command writes <image>.mounted under the lock before hdiutil starts and removes it only once it has
// seen the image detached. Any marker at all refuses LockDisk: the attach may even still be finishing,
// and only a person's recovery, which proves otherwise, clears it.
const diskMountSuffix = ".mounted"

// The phases a DiskMount records.
const (
	DiskMountAttaching = "attaching" // written before hdiutil starts: the attach may be pending
	DiskMountAttached  = "attached"  // hdiutil reported the mount
)

// DiskMount is what a mount marker holds: how far the mount got, and which image it is of.
type DiskMount struct {
	Phase string `json:"phase"`
	Name  string `json:"name"`  // the drive's name, for the sentence
	Image string `json:"image"` // the image's path, which hdiutil and ps name it by
}

// DiskMountedError is the refusal of a disk whose mount marker is there.
type DiskMountedError struct{ Name string }

func (e *DiskMountedError) Error() string {
	return fmt.Sprintf("The drive %q may still be mounted on this Mac, so Wardyn won't start a run on it. Run drive open --direct --recover %s to check and clear it.", e.Name, e.Name)
}

// LockDiskForMount is LockDisk for the mount command and its recovery, which must get past a marker
// to read it. It takes the lock in one try: ErrDiskInUse when another holds it.
func LockDiskForMount(path string) (unlock func(), err error) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // lockDiskFile tries once before it waits on ctx
	unlock, err = lockDiskFile(ctx, path)
	if errors.Is(err, context.Canceled) {
		return nil, ErrDiskInUse
	}
	return unlock, err
}

// ReadDiskMount reads the image's marker. found is false only when there is certainly none: a marker
// that can't be read or parsed is found, with err saying why.
func ReadDiskMount(path string) (m DiskMount, found bool, err error) {
	data, err := os.ReadFile(path + diskMountSuffix)
	if errors.Is(err, os.ErrNotExist) {
		if _, serr := os.Lstat(path + diskMountSuffix); errors.Is(serr, os.ErrNotExist) {
			return DiskMount{}, false, nil
		}
	}
	if err != nil {
		return DiskMount{}, true, fmt.Errorf("macvm: disk mount marker: %w", err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return DiskMount{}, true, fmt.Errorf("macvm: disk mount marker: %w", err)
	}
	return m, true, nil
}

// WriteDiskMount replaces the image's marker whole and durably. The caller holds the disk's lock.
func WriteDiskMount(path string, m DiskMount) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+diskMountSuffix+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path+diskMountSuffix)
	}
	if err == nil {
		err = syncDir(filepath.Dir(path))
	}
	return err
}

// ClearDiskMount removes the image's marker. The caller holds the disk's lock and has seen the image detached.
func ClearDiskMount(path string) error {
	if err := os.Remove(path + diskMountSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// refuseMountedDisk is the refusal LockDisk gives while the image's marker is there. The sentence names
// the drive as the marker does, or by its object name when the marker can't say.
func refuseMountedDisk(path string) error {
	m, found, _ := ReadDiskMount(path)
	if !found {
		return nil
	}
	return &DiskMountedError{Name: cmp.Or(m.Name, strings.TrimSuffix(filepath.Base(path), ".img"))}
}
