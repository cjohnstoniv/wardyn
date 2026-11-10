// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package foldersync

import "os"

func replaceFile(root *os.Root, from, to string) error { return root.Rename(from, to) }

// placeNew links to at from; it fails with fs.ErrExist when to exists.
func placeNew(root *os.Root, from, to string) error { return root.Link(from, to) }

func syncDirIn(root *os.Root, dir string) error {
	d, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return fsyncDir(d)
}

func syncDirPath(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return fsyncDir(d)
}
