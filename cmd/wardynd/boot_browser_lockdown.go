// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/cjohnstoniv/wardyn/internal/browserpolicy"
)

// browserLockdownPolicy is WARDYN_BROWSER_LOCKDOWN and WARDYN_BROWSER_POLICY_FILE read once (#2970):
// nil when off, else the rendered Chromium managed policy.
func browserLockdownPolicy(lockdown, file string) ([]byte, error) {
	switch lockdown {
	case "off":
		if file != "" {
			return nil, errors.New("WARDYN_BROWSER_POLICY_FILE is set but WARDYN_BROWSER_LOCKDOWN is off: set it to on, or unset the file")
		}
		return nil, nil
	case "on":
	default:
		return nil, fmt.Errorf("invalid WARDYN_BROWSER_LOCKDOWN=%q: want on or off", lockdown)
	}
	var org []byte
	if file != "" {
		fh, err := os.Open(file)
		if err == nil {
			org, err = io.ReadAll(io.LimitReader(fh, browserpolicy.MaxOrgBytes+1))
			_ = fh.Close()
		}
		if err != nil {
			return nil, fmt.Errorf("WARDYN_BROWSER_POLICY_FILE: %w", err)
		}
	}
	out, err := browserpolicy.Render(org)
	if err != nil {
		return nil, fmt.Errorf("WARDYN_BROWSER_POLICY_FILE: %w", err)
	}
	return out, nil
}

// exitOnBadBrowserLockdown refuses to boot on a lockdown setting browserLockdownPolicy refuses: an
// operator who meant the sandbox browser locked must not get it unlocked.
func exitOnBadBrowserLockdown(f *bootFlags) {
	out, err := browserLockdownPolicy(*f.browserLockdown, *f.browserPolicyFile)
	if err != nil {
		fmt.Fprintln(flag.CommandLine.Output(), err)
		os.Exit(2)
	}
	f.browserPolicy = out
}
