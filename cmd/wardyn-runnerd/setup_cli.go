// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"

	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// installCLI installs the wardyn CLI that sits beside this runnerd (the release bundle and the macOS package
// carry both), next to the runner under the data directory. It never downloads anything, and does nothing
// when no CLI sits beside this binary, when that CLI is already the one on the person's PATH, or when the
// copy already installed is newer than this release.
func (h setupHost) installCLI(ctx context.Context, p setupPaths, out io.Writer) error {
	src := filepath.Join(filepath.Dir(h.exe), "wardyn")
	srcInfo, err := os.Stat(src)
	if err != nil || !srcInfo.Mode().IsRegular() {
		return nil
	}
	if onPath, err := exec.LookPath("wardyn"); err == nil {
		if pathInfo, err := os.Stat(onPath); err == nil && os.SameFile(srcInfo, pathInfo) {
			return nil
		}
	}
	if dstInfo, err := os.Stat(p.cliInstalled); err == nil {
		if os.SameFile(srcInfo, dstInfo) {
			return nil
		}
		if b, _ := h.run(ctx, p.cliInstalled, "--version"); newerThanThisRelease(string(b)) {
			fmt.Fprintf(out, "A newer wardyn CLI is already installed at %s; left it as it is.\n", p.cliInstalled)
			return nil
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(p.cliInstalled), 0o755); err != nil {
		return err
	}
	if err := writeAtomic(p.cliInstalled, 0o755, func(w io.Writer) error { _, err := io.Copy(w, in); return err }); err != nil {
		return fmt.Errorf("install %s: %w", p.cliInstalled, err)
	}
	fmt.Fprintf(out, "Installed the wardyn CLI: %s\n", p.cliInstalled)
	if dir := filepath.Dir(p.cliInstalled); !slices.Contains(filepath.SplitList(os.Getenv("PATH")), dir) {
		fmt.Fprintf(out, "To run it as `wardyn`, add it to your PATH: export PATH=%s:$PATH\n", shellQuote(dir))
	}
	return nil
}

// seedCLIProfile points the wardyn CLI at the organisation this computer was set up for, so
// `wardyn run attach <id>` needs no URL. A profile that already names a URL is the person's choice and is
// kept.
func (h setupHost) seedCLIProfile(p setupPaths, org string, out io.Writer) error {
	have, err := cliprofile.Read(p.cliProfile)
	if err != nil {
		fmt.Fprintf(out, "Left the wardyn CLI profile as it is (%v).\n", err)
		return nil
	}
	switch have {
	case org:
		return nil
	case "":
		if err := cliprofile.Write(p.cliProfile, org); err != nil {
			return err
		}
		fmt.Fprintf(out, "The wardyn CLI now points at %s (change it with wardyn auth set-url).\n", org)
	default:
		fmt.Fprintf(out, "The wardyn CLI profile points at %s, not %s; left it as it is (wardyn auth set-url changes it).\n", have, org)
	}
	return nil
}

var versionTriple = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// newerThanThisRelease is whether a `--version` line names a release after this one. A line that names
// none (a binary that would not run) is not newer.
func newerThanThisRelease(line string) bool {
	have, ours := versionTriple.FindStringSubmatch(line), versionTriple.FindStringSubmatch(version.Version)
	if have == nil || ours == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		h, _ := strconv.Atoi(have[i])
		o, _ := strconv.Atoi(ours[i])
		if h != o {
			return h > o
		}
	}
	return false
}
