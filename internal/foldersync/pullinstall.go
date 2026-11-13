// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// errSavedLocally is a pull's install or delete that found a laptop save, or delete, made after its
// last look at the laptop file (unchangedLocally): the laptop's file stays where it is.
var errSavedLocally = errors.New("the local file changed after its check")

// errNotPutBack is a laptop file a pull moved out of the way and could not put back; the error
// names where it is.
var errNotPutBack = errors.New("the local file could not be put back where it was")

// keptAt is errNotPutBack for the file now at name.
func keptAt(name string) error {
	return &InstallError{Step: "put-back", Err: fmt.Errorf("%w; it is kept as %s", errNotPutBack, name)}
}

// installPulled installs a pull's synced temp tmpName (tmp, beside dst) at dst without dropping a
// laptop save made after the last look. A new file (walked nil) is linked or moved in, never over one
// made since (InstallNew). A file the walk listed is swapped with the temp in one step
// (exchangeFiles), so whatever was at dst is still named, at tmpName, and is looked at: the file the
// walk listed is left there for the caller to remove, as a rename would have dropped it; anything else
// is a save, swapped back, and errSavedLocally. A save still at tmpName after that is moved to a name
// of its own and named in the error: when the swap back failed, dst holds the sandbox's version; when
// another save came in between the two swaps, dst holds the earlier save and the later one is the one
// moved. Where the swap is not offered (Windows, and filesystems without it), the temp is renamed over
// dst as before. ours is the temp's localSide. The caller removes tmpName.
func (sy *Syncer) installPulled(tmp *os.File, tmpName, dst string, walked *syncFile, ours Side) error {
	if walked == nil {
		_, err := installSynced(sy.Root, sy.dirs, tmp, tmpName, InstallNew, dst)
		if errors.Is(err, errNamesTaken) {
			return errSavedLocally
		}
		return err
	}
	switch err := exchangeFiles(sy.Root, tmpName, dst); {
	case errors.Is(err, fs.ErrNotExist):
		return errSavedLocally // dst was deleted or moved after the look
	case err != nil:
		_, err = installSynced(sy.Root, sy.dirs, tmp, tmpName, InstallReplace, dst)
		return err
	}
	if sy.unchangedLocally(tmpName, walked) {
		if err := syncDirsUp(sy.Root, sy.dirs, path.Dir(dst)); err != nil {
			return &InstallError{Step: "sync-dir", Installed: true, Err: err}
		}
		return nil
	}
	if err := exchangeFiles(sy.Root, tmpName, dst); err != nil || !sy.unchangedLocally(tmpName, &syncFile{side: ours}) {
		keep, f, err := createTemp(sy.Root, path.Dir(dst), ".wardyn-sync-save-", "")
		if err != nil {
			return err
		}
		_ = f.Close()
		if err := replaceFile(sy.Root, tmpName, keep); err != nil {
			return err
		}
		return keptAt(keep)
	}
	if err := syncDirsUp(sy.Root, sy.dirs, path.Dir(dst)); err != nil {
		return &InstallError{Step: "sync-dir", Err: err}
	}
	return errSavedLocally
}

// deleteLooked runs in moveToTrash after its look at the laptop file and its copy into Trash, and
// before its move; tests save there.
var deleteLooked = func() {}

// moveToTrash copies the laptop file rel into Trash and removes it, only while it is still the file
// the walk listed (walked). The copy is made from the file in place, so a pass killed during it
// leaves the file where it was. rel is then moved aside in one step, onto an empty temp of its own,
// and what was moved is looked at: a save made after the first look is moved back, never over a file
// made since, its Trash copy is dropped, and it is errSavedLocally. Only the file the walk listed is
// removed.
func (sy *Syncer) moveToTrash(rel string, walked *syncFile) error {
	if !sy.unchangedLocally(rel, walked) {
		return errSavedLocally
	}
	trash := filepath.Join(sy.Opts.Trash, filepath.FromSlash(rel))
	if err := copyToTrash(sy.Root, rel, trash); err != nil {
		return err
	}
	deleteLooked()
	aside, f, err := createTemp(sy.Root, path.Dir(rel), ".wardyn-sync-", "")
	if err != nil {
		_ = os.Remove(trash)
		return err
	}
	_ = f.Close()
	if err := replaceFile(sy.Root, rel, aside); err != nil {
		_ = sy.Root.Remove(aside)
		_ = os.Remove(trash)
		return err
	}
	if !sy.unchangedLocally(aside, walked) {
		_ = os.Remove(trash)
		return sy.putBack(aside, rel, errSavedLocally)
	}
	return sy.Root.Remove(aside)
}

// putBack moves aside back to rel, never over a file made since (placeNew), and returns err. One it
// cannot move back stays at aside, which the error names.
func (sy *Syncer) putBack(aside, rel string, err error) error {
	if placeNew(sy.Root, aside, rel) != nil {
		return keptAt(aside)
	}
	_ = sy.Root.Remove(aside) // placeNew linked, copied or moved it
	return err
}

// copyToTrash copies the laptop file name into dst, a new file outside the root.
func copyToTrash(root *os.Root, name, dst string) error {
	src, err := openFileIn(root, name, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
