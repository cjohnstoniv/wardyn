// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// prefsDomain is the preference domain of the configuration profile (deploy/runner/macos/
// wardyn-runnerd.mobileconfig) that device management pushes to a managed Mac.
const prefsDomain = "com.wardyn.runnerd"

// Swapped in tests.
var (
	prefsOS      = runtime.GOOS
	prefsRoot    = "/Library/Managed Preferences"
	prefsExtract = plutilExtract
)

// managedPref is the string MDM set for key in the profile's preference domain, or "" when this is not
// a Mac, no profile is installed or the key is unset or empty. The person's own file under
// /Library/Managed Preferences/<user>/ outranks the device-wide one. The files are root-owned and may
// be binary plists, so plutil reads them.
func managedPref(key string) string {
	if prefsOS != "darwin" {
		return ""
	}
	files := []string{filepath.Join(prefsRoot, prefsDomain+".plist")}
	if u, err := user.Current(); err == nil {
		files = append([]string{filepath.Join(prefsRoot, u.Username, prefsDomain+".plist")}, files...)
	}
	for _, f := range files {
		if v := prefsExtract(f, key); v != "" {
			return v
		}
	}
	return ""
}

func plutilExtract(file, key string) string {
	if _, err := os.Stat(file); err != nil {
		return ""
	}
	out, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", "-o", "-", "--", file).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// setting is a serve default: the managed preference key, else the environment variable (which
// the service definition `setup` writes sets).
func setting(prefKey, envName string) string {
	return cmp.Or(managedPref(prefKey), os.Getenv(envName))
}
