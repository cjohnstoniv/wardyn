// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io/fs"
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
	plutilPath   = "/usr/bin/plutil"
)

// managedPref is the string MDM set for key in the profile's preference domain, or "" when this is not
// a Mac, no profile is installed or the key is unset or empty. A profile that cannot be read reads as unset
// here; managedPrefErr is the read that tells the two apart.
func managedPref(key string) string {
	v, _ := managedPrefErr(key)
	return v
}

// managedPrefErr is managedPref that also reports a profile that is there but cannot be read. The person's
// own file under /Library/Managed Preferences/<user>/ outranks the device-wide one. The files are root-owned
// and may be binary plists, so plutil reads them.
func managedPrefErr(key string) (string, error) {
	if prefsOS != "darwin" {
		return "", nil
	}
	files := []string{filepath.Join(prefsRoot, prefsDomain+".plist")}
	if u, err := user.Current(); err == nil {
		files = append([]string{filepath.Join(prefsRoot, u.Username, prefsDomain+".plist")}, files...)
	}
	for _, f := range files {
		v, err := prefsExtract(f, key)
		if err != nil {
			return "", err
		}
		if v != "" {
			return v, nil
		}
	}
	return "", nil
}

// plutilExtract is key's value in the plist at file. Only a missing file, or a readable plist without the
// key, is "unset"; any other failure is an error, because a profile that is there but unreadable must not
// read as one that sets nothing.
func plutilExtract(file, key string) (string, error) {
	if _, err := os.Stat(file); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("managed preferences %s: %w", file, err)
	}
	out, err := exec.Command(plutilPath, "-extract", key, "raw", "-o", "-", "--", file).Output()
	if err != nil {
		// Only a plain exit 1 can mean an absent key: a plutil that could not run, or one a signal ended
		// (ExitCode -1), proves nothing about the profile.
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return "", fmt.Errorf("managed preferences %s: %w", file, err)
		}
		// plutil exits 1 for an absent key as well as for a file it cannot parse: a plist that lints
		// clean is the first.
		if exec.Command(plutilPath, "-lint", "-s", "--", file).Run() == nil {
			return "", nil
		}
		return "", fmt.Errorf("managed preferences %s: plutil cannot read it", file)
	}
	return strings.TrimSpace(string(out)), nil
}
