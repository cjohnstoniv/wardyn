// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

// maxSyncEntries bounds one side's listing: the sandbox chooses what is in it.
const maxSyncEntries = 200_000

// syncReport is one pass's outcome. Every list is non-nil so --json prints [].
type syncReport struct {
	Pushed    []string      `json:"pushed"`
	Pulled    []string      `json:"pulled"`
	Conflicts []string      `json:"conflicts"`
	Refused   []syncRefusal `json:"refused"`
	BytesOut  int64         `json:"bytes_to_sandbox"`
	BytesIn   int64         `json:"bytes_from_sandbox"`
	seenSet   map[string]bool
}

type syncRefusal struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func newSyncReport() *syncReport {
	return &syncReport{Pushed: []string{}, Pulled: []string{}, Conflicts: []string{}, Refused: []syncRefusal{}, seenSet: map[string]bool{}}
}

func (r *syncReport) refuse(rel, reason string) {
	if k := rel + "\x00" + reason; !r.seenSet[k] {
		r.seenSet[k] = true
		r.Refused = append(r.Refused, syncRefusal{rel, reason})
	}
}

func (r *syncReport) empty() bool {
	return len(r.Pushed)+len(r.Pulled)+len(r.Conflicts)+len(r.Refused) == 0
}

type syncOptions struct {
	root   string // absolute local directory
	remote string // absolute, validated sandbox directory
	pull   bool
}

type syncer struct {
	c         *sftp.Client
	o         syncOptions
	st        *syncState
	stateFile string
}

type syncFile struct {
	side syncSide
	mode fs.FileMode
}

func localSide(fi fs.FileInfo) syncSide { return syncSide{fi.Size(), fi.ModTime().UnixNano()} }

// remoteSide has second resolution: that is all the sftp attributes carry.
func remoteSide(fi fs.FileInfo) syncSide {
	return syncSide{fi.Size(), fi.ModTime().Unix() * int64(time.Second)}
}

// pass syncs once: it lists both sides, then pushes, and under --pull pulls.
// Nothing is ever deleted on either side.
func (sy *syncer) pass() (*syncReport, error) {
	rep := newSyncReport()
	local, localDirs, err := sy.walkLocal(rep)
	if err != nil {
		return nil, err
	}
	remote, remoteDirs, err := sy.walkRemote(rep)
	if err != nil {
		return nil, err
	}
	all := map[string]bool{}
	for rel := range local {
		all[rel] = true
	}
	for rel := range remote {
		all[rel] = true
	}
	var everything []string
	for rel := range all {
		everything = append(everything, rel)
	}
	everything = append(everything, localDirs...)
	everything = append(everything, remoteDirs...)
	collide := syncCollisions(everything)

	rels := make([]string, 0, len(all))
	for rel := range all {
		rels = append(rels, rel)
	}
	slices.Sort(rels)
	for _, rel := range rels {
		if syncCollides(collide, rel) {
			rep.refuse(rel, "case collision")
			continue
		}
		if err := sy.decide(rel, local, remote, rep); err != nil {
			return nil, err
		}
	}
	if err := sy.st.save(sy.stateFile); err != nil {
		return nil, fmt.Errorf("save sync state: %w", err)
	}
	return rep, nil
}

func syncCollides(collide map[string]bool, rel string) bool {
	for p := rel; p != "." && p != ""; p = path.Dir(p) {
		if collide[p] {
			return true
		}
	}
	return false
}

// decide applies the direction and conflict rules to one path. The laptop
// wins: a file changed on both sides is pushed, never pulled, and reported.
func (sy *syncer) decide(rel string, local map[string]syncFile, remote map[string]os.FileInfo, rep *syncReport) error {
	l, hasL := local[rel]
	r, hasR := remote[rel]
	seen, known := sy.st.Files[rel]
	var rSide syncSide
	if hasR {
		rSide = remoteSide(r)
	}
	lChanged := hasL && (!known || l.side != seen.Local)
	rChanged := hasR && (!known || rSide != seen.Remote)
	switch {
	case hasL && hasR && lChanged && rChanged && (known || sy.o.pull):
		rep.Conflicts = append(rep.Conflicts, rel)
		return sy.push(rel, l, rep)
	case hasL && lChanged:
		return sy.push(rel, l, rep)
	case hasL && !lChanged && rChanged && sy.o.pull:
		return sy.pull(rel, r, rep)
	case !hasL && hasR && sy.o.pull && rChanged:
		if known {
			rep.Conflicts = append(rep.Conflicts, rel)
			return nil
		}
		return sy.pull(rel, r, rep)
	}
	return nil
}

func (sy *syncer) walkLocal(rep *syncReport) (files map[string]syncFile, dirs []string, err error) {
	files = map[string]syncFile{}
	count := 0
	err = filepath.WalkDir(sy.o.root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if p == sy.o.root {
				return werr
			}
			rep.refuse(filepath.ToSlash(strings.TrimPrefix(p, sy.o.root+string(filepath.Separator))), "unreadable: "+werr.Error())
			return nil
		}
		if p == sy.o.root {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, sy.o.root+string(filepath.Separator)))
		if count++; count > maxSyncEntries {
			return fmt.Errorf("%s holds more than %d entries", sy.o.root, maxSyncEntries)
		}
		switch {
		case syncDenied(rel):
			rep.refuse(rel, "denylist")
			return skipIf(d.IsDir())
		case d.Type()&fs.ModeSymlink != 0:
			rep.refuse(rel, "symlink")
		case d.IsDir():
			dirs = append(dirs, rel)
		case d.Type().IsRegular():
			fi, ierr := d.Info()
			if ierr != nil {
				rep.refuse(rel, "unreadable: "+ierr.Error())
				return nil
			}
			files[rel] = syncFile{localSide(fi), fi.Mode()}
		default:
			rep.refuse(rel, "not a regular file")
		}
		return nil
	})
	return files, dirs, err
}

func skipIf(dir bool) error {
	if dir {
		return fs.SkipDir
	}
	return nil
}

func (sy *syncer) walkRemote(rep *syncReport) (files map[string]os.FileInfo, dirs []string, err error) {
	files = map[string]os.FileInfo{}
	count := 0
	var walk func(abs, rel string) error
	walk = func(abs, rel string) error {
		entries, err := sy.c.ReadDir(abs)
		if err != nil {
			return fmt.Errorf("list %s in the sandbox: %w", abs, err)
		}
		for _, fi := range entries {
			if count++; count > maxSyncEntries {
				return fmt.Errorf("the sandbox directory holds more than %d entries", maxSyncEntries)
			}
			name := fi.Name()
			if err := syncEntryName(name); err != nil {
				rep.refuse(path.Join(rel, strings.ReplaceAll(name, "/", "?")), err.Error())
				continue
			}
			childRel := path.Join(rel, name)
			switch {
			case syncDenied(childRel):
				rep.refuse(childRel, "denylist")
			case fi.Mode()&fs.ModeSymlink != 0:
				rep.refuse(childRel, "symlink")
			case fi.IsDir():
				dirs = append(dirs, childRel)
				if err := walk(path.Join(abs, name), childRel); err != nil {
					return err
				}
			case fi.Mode().IsRegular():
				files[childRel] = fi
			default:
				rep.refuse(childRel, "not a regular file")
			}
		}
		return nil
	}
	if err := walk(sy.o.remote, ""); err != nil {
		return nil, nil, err
	}
	return files, dirs, nil
}

func (sy *syncer) push(rel string, l syncFile, rep *syncReport) error {
	abs := filepath.Join(sy.o.root, filepath.FromSlash(rel))
	f, err := os.Open(abs)
	if err != nil {
		rep.refuse(rel, "unreadable: "+err.Error())
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		rep.refuse(rel, "changed while syncing")
		return nil
	}
	if lstat, err := os.Lstat(abs); err != nil || !os.SameFile(lstat, fi) {
		rep.refuse(rel, "symlink")
		return nil
	}
	dst := path.Join(sy.o.remote, rel)
	if err := sy.c.MkdirAll(path.Dir(dst)); err != nil {
		return fmt.Errorf("create %s in the sandbox: %w", path.Dir(dst), err)
	}
	out, err := sy.c.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("write %s in the sandbox: %w", dst, err)
	}
	n, err := io.Copy(out, io.LimitReader(f, l.side.Size))
	rep.BytesOut += n
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s in the sandbox: %w", dst, err)
	}
	mt := time.Unix(0, l.side.MTime)
	_ = sy.c.Chmod(dst, l.mode.Perm())
	_ = sy.c.Chtimes(dst, mt, mt)
	rfi, err := sy.c.Stat(dst)
	if err != nil {
		return fmt.Errorf("stat %s in the sandbox: %w", dst, err)
	}
	sy.st.Files[rel] = syncFileSeen{Local: l.side, Remote: remoteSide(rfi)}
	rep.Pushed = append(rep.Pushed, rel)
	return nil
}

// pull writes a sandbox file onto the laptop: never through a symlink, never
// over anything but a regular file, and never with an exec bit.
func (sy *syncer) pull(rel string, r os.FileInfo, rep *syncReport) error {
	abs, err := syncLocalPath(sy.o.root, rel)
	if err != nil {
		rep.refuse(rel, err.Error())
		return nil
	}
	if fi, err := os.Lstat(abs); err == nil && !fi.Mode().IsRegular() {
		rep.refuse(rel, "a local entry that is not a regular file is in the way")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	src, err := sy.c.Open(path.Join(sy.o.remote, rel))
	if err != nil {
		return fmt.Errorf("read %s in the sandbox: %w", rel, err)
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".wardyn-sync-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(src, r.Size()))
	rep.BytesIn += n
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("read %s in the sandbox: %w", rel, err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	mt := r.ModTime()
	_ = os.Chtimes(tmp.Name(), mt, mt)
	if err := os.Rename(tmp.Name(), abs); err != nil {
		return err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	sy.st.Files[rel] = syncFileSeen{Local: localSide(fi), Remote: remoteSide(r)}
	rep.Pulled = append(rep.Pulled, rel)
	return nil
}
