// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"runtime"

	"github.com/cjohnstoniv/wardyn/internal/managedprefs"
)

const prefsDomain = managedprefs.Domain

// Swapped in tests.
var (
	prefsOS      = runtime.GOOS
	prefsRoot    = "/Library/Managed Preferences"
	prefsExtract = plutilExtract
	plutilPath   = "/usr/bin/plutil"
)

// managedPref is the string MDM set for key in the profile's preference domain, or "" when this is not
// a Mac, no profile is installed or the key is unset or empty. A profile that cannot be read reads as unset
// here; managedPrefErr is the read that tells the two apart.
func managedPref(key string) string {
	v, _ := managedPrefErr(key)
	return v
}

// managedPrefErr is managedPref that also reports a profile that is there but cannot be read.
func managedPrefErr(key string) (string, error) {
	return managedprefs.Read(prefsOS, prefsRoot, prefsDomain, key, prefsExtract)
}

func plutilExtract(file, key string) (string, error) {
	return managedprefs.PlutilExtract(plutilPath, file, key)
}
