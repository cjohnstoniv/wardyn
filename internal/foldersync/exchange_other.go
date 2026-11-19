// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package foldersync

import (
	"errors"
	"os"
)

// exchangeFiles is not offered here (Windows among them): a pull renames over the file as before.
var exchangeFiles = func(*os.Root, string, string) error { return errors.ErrUnsupported }
