// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package main

import "os"

func fileOwner(os.FileInfo) (int, bool) { return 0, false }

func openOrgCAFile(path string) (*os.File, error) { return os.Open(path) }
