// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package managedprefs reads the macOS configuration profile (deploy/runner/macos/wardyn-runnerd.mobileconfig)
// that device management pushes to a managed Mac, for wardyn-runnerd and the wardyn CLI alike.
package managedprefs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// Domain is the profile's preference domain.
const Domain = "com.wardyn.runnerd"

// Read is the string MDM set for key in domain's profile under root, "" when goos is not darwin, no profile
// is installed or the key is unset or empty. The person's own file under <root>/<user>/ outranks the
// device-wide one. The files are root-owned and may be binary plists, so extract (PlutilExtract) reads them.
func Read(goos, root, domain, key string, extract func(file, key string) (string, error)) (string, error) {
	if goos != "darwin" {
		return "", nil
	}
	for _, f := range Files(root, domain) {
		v, err := extract(f, key)
		if err != nil {
			return "", err
		}
		if v != "" {
			return v, nil
		}
	}
	return "", nil
}

// Files are the profile files of domain under root, in the order Read consults them: the person's own file
// under <root>/<user>/, then the device-wide one.
func Files(root, domain string) []string {
	files := []string{filepath.Join(root, domain+".plist")}
	if u, err := user.Current(); err == nil {
		files = append([]string{filepath.Join(root, u.Username, domain+".plist")}, files...)
	}
	return files
}

// Bool is a value Read or PlutilExtract returned, read as a boolean: set is false for "" (the key is unset).
// plutil prints a plist boolean as true or false; a profile that holds the word as a string is read the same.
func Bool(v string) (val, set bool, err error) {
	switch strings.ToLower(v) {
	case "":
		return false, false, nil
	case "true", "1", "yes":
		return true, true, nil
	case "false", "0", "no":
		return false, true, nil
	}
	return false, true, fmt.Errorf("%q is not true or false", v)
}

// PlutilExtract is key's value in the plist at file, read with the plutil at plutil. Only a missing file, or
// a readable plist without the key, is "unset"; any other failure is an error, because a profile that is
// there but unreadable must not read as one that sets nothing.
func PlutilExtract(plutil, file, key string) (string, error) {
	return plutilExtract(plutil, file, key, "raw")
}

// PlutilExtractJSON is PlutilExtract that prints the value as JSON: a plist array is a JSON array, which raw
// would print as its length. A string comes back quoted.
func PlutilExtractJSON(plutil, file, key string) (string, error) {
	return plutilExtract(plutil, file, key, "json")
}

func plutilExtract(plutil, file, key, format string) (string, error) {
	if _, err := os.Stat(file); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("managed preferences %s: %w", file, err)
	}
	out, err := exec.Command(plutil, "-extract", key, format, "-o", "-", "--", file).Output()
	if err != nil {
		// Only a plain exit 1 can mean an absent key: a plutil that could not run, or one a signal ended
		// (ExitCode -1), proves nothing about the profile.
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return "", fmt.Errorf("managed preferences %s: %w", file, err)
		}
		// plutil exits 1 for an absent key as well as for a file it cannot parse: a plist that lints
		// clean is the first.
		if exec.Command(plutil, "-lint", "-s", "--", file).Run() == nil {
			return "", nil
		}
		return "", fmt.Errorf("managed preferences %s: plutil cannot read it", file)
	}
	return strings.TrimSpace(string(out)), nil
}
