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
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// driveOpenLog, in the VM directory, is the last drive-open helpers' log. A clone's directory goes with it.
const driveOpenLog = "drive-open.log"

// cmdDriveOpen is `wardyn-macvm drive-open`, which `wardyn-runnerd drive open` starts in its own session
// under a ledger entry it took: macvm.DriveOpen. Once the guest has the drive mounted, stdout gets one
// macvm.DriveOpenReady line. Stdin's end stops the VM, so a killed caller cannot leave it holding the
// drive's lock; the caller keeps a pipe open there. An error or a refusal is the last stderr line, exit 1.
func cmdDriveOpen(ctx context.Context, args []string, d deps) int {
	fs := flag.NewFlagSet(macvm.DriveOpenCommand, flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	j := newJobFlags(fs)
	object := fs.String(macvm.FlagDrive, "", "the disk drive's object name")
	label := fs.String(macvm.FlagDriveLabel, "", "the name of the disk drive's volume")
	if code := j.parse(fs, args, d); code != 0 {
		return code
	}
	if *j.entry == "" || *j.runner == "" || *object == "" || *label == "" {
		fmt.Fprintf(d.stderr, "wardyn-macvm %s: --%s, --%s, --%s and --%s are required\n", macvm.DriveOpenCommand,
			macvm.FlagRunnerID, macvm.FlagLedgerEntry, macvm.FlagDrive, macvm.FlagDriveLabel)
		return 2
	}
	if err := os.MkdirAll(*j.root, 0o700); err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	logf, err := macvm.OpenRotatingLog(filepath.Join(*j.root, driveOpenLog), macvm.HelperLogMax)
	if err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	defer logf.Close()
	eng, err := d.engine()
	if err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, d.stdin); cancel() }()
	err = macvm.DriveOpen(ctx, macvm.DriveOpenOptions{
		Engine: eng, Layout: macvm.Layout{Root: *j.root}, Slot: *j.slot, Object: *object, Label: *label,
		CPUs: *j.cpus, MemoryMiB: *j.memMiB, RunnerID: *j.runner, Entry: *j.entry, LedgerDir: *j.ledgerDir, SockRoot: *j.sockRoot,
		Claim:         func(c macvm.RunConfig) error { return claim(c, d) },
		Leaked:        func(c macvm.RunConfig) error { return leaked(c, d) },
		HoldIdleSleep: holdIdleSleep, SockFstat: d.sockFstat, Log: slog.New(slog.NewJSONHandler(logf, nil)),
		Ready: func(r macvm.DriveOpenReady) { _ = json.NewEncoder(d.stdout).Encode(r) },
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	return 0
}
