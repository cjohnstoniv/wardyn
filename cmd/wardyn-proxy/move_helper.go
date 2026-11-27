// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/movefs"
	"github.com/cjohnstoniv/wardyn/internal/movepack"
)

// The move helper's modes: wardynd's Kubernetes driver runs this binary as a move's helper pod (-move-idle) and
// execs -move-export or -move-import into it, on the claims it mounted there. No config is loaded.
var (
	moveIdleFlag     = flag.Bool("move-idle", false, "internal: wait as a move's helper pod (wardynd's Kubernetes driver only)")
	moveExportFlag   = flag.Bool("move-export", false, "internal: write the carried set's files to stdout as a move stream (wardynd's Kubernetes driver only)")
	moveImportFlag   = flag.Bool("move-import", false, "internal: write a move stream from stdin beneath the mount roots (wardynd's Kubernetes driver only)")
	moveSetFlag      = flag.String("move-set", "", "internal: the carried set, as JSON (with -move-export or -move-import)")
	moveMaxBytesFlag = flag.Int64("move-max-bytes", 0, "internal: the import's bound on the stream in bytes (with -move-import)")
	moveMountsFlag   = flag.String("move-mounts", "", "internal: the mount roots the import writes beneath, a JSON list of absolute paths (with -move-import)")
)

// moveIdleBound is the longest a helper pod's main process waits; the pod's own deadline is the backstop.
const moveIdleBound = 6 * time.Hour

const (
	moveModeIdle   = "idle"
	moveModeExport = "export"
	moveModeImport = "import"
)

// moveMode is the one move mode the flags name, or "" for none.
func moveMode(idle, export, imp bool) (string, error) {
	var modes []string
	for m, on := range map[string]bool{moveModeIdle: idle, moveModeExport: export, moveModeImport: imp} {
		if on {
			modes = append(modes, m)
		}
	}
	switch len(modes) {
	case 0:
		return "", nil
	case 1:
		return modes[0], nil
	}
	return "", errors.New("wardyn-proxy: name one of -move-idle, -move-export and -move-import")
}

// moveArgs is what the move flags carry.
type moveArgs struct {
	set      string
	maxBytes int64
	mounts   string
	// base is where the sandbox's "/" is: "/" in the helper pod.
	base string
}

// exitIfMoveHelper runs the move mode the flags name, if any, and exits with its code.
func exitIfMoveHelper() {
	mode, err := moveMode(*moveIdleFlag, *moveExportFlag, *moveImportFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if mode == "" {
		return
	}
	os.Exit(runMoveHelper(mode, moveArgs{set: *moveSetFlag, maxBytes: *moveMaxBytesFlag, mounts: *moveMountsFlag, base: "/"}, os.Stdin, os.Stdout, os.Stderr))
}

// runMoveHelper runs one move mode and answers the process's exit code: 0 done, 1 failed, 2 a usage error.
func runMoveHelper(mode string, a moveArgs, stdin io.Reader, stdout, stderr io.Writer) int {
	if mode == moveModeIdle {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		select {
		case <-ctx.Done():
		case <-time.After(moveIdleBound):
		}
		return 0
	}
	var set movepack.Set
	if a.set == "" {
		fmt.Fprintln(stderr, "wardyn-proxy: -move-"+mode+" needs -move-set")
		return 2
	}
	if err := json.Unmarshal([]byte(a.set), &set); err != nil {
		fmt.Fprintln(stderr, "wardyn-proxy: -move-set is not a carried set:", err)
		return 2
	}
	if mode == moveModeExport {
		if _, err := movefs.Export(stdout, set, a.base); err != nil {
			fmt.Fprintln(stderr, "wardyn-proxy: move export:", err)
			return 1
		}
		return 0
	}
	var mounts []string
	if a.mounts == "" || a.maxBytes <= 0 {
		fmt.Fprintln(stderr, "wardyn-proxy: -move-import needs -move-mounts and a -move-max-bytes above 0")
		return 2
	}
	if err := json.Unmarshal([]byte(a.mounts), &mounts); err != nil {
		fmt.Fprintln(stderr, "wardyn-proxy: -move-mounts is not a list of paths:", err)
		return 2
	}
	if _, err := movefs.Extract(stdin, movefs.ExtractOptions{Set: set, MaxBytes: a.maxBytes, Mounts: mounts, Base: a.base}); err != nil {
		fmt.Fprintln(stderr, "wardyn-proxy: move import:", err)
		return 1
	}
	return 0
}
