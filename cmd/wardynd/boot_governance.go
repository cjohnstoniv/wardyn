// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// exitOnGovernanceFlags refuses to boot on a governance four-eyes setting this build cannot honour,
// exit 2 like every other malformed setting, in every auth mode.
//
//   - WARDYN_GOVERNANCE_CHANGE_TTL must be positive: a change that expires the moment it is held (or
//     before) could never be approved. A value that is not a duration was already refused by the flag.
//   - WARDYN_GOVERNANCE_SECOND_HUMAN is refused outright for now. The API holds a governance profile
//     or assignment write for a second human, but the rest of the covered set, the console and the
//     first-party clients are not all pending-aware yet, and a tree that accepted the switch would
//     advertise four-eyes over writes that are still single-human. The lane that completes the set
//     removes this refusal; the boot warning for local mode (resolveLocalMode) applies from then on.
func exitOnGovernanceFlags(f *bootFlags) {
	if *f.governanceChangeTTL <= 0 {
		fmt.Fprintf(flag.CommandLine.Output(), "invalid WARDYN_GOVERNANCE_CHANGE_TTL=%q: want a positive duration such as 72h\n",
			f.governanceChangeTTL.String())
		os.Exit(2)
	}
	if api.GovernanceSecondHumanEnabled() {
		fmt.Fprintln(flag.CommandLine.Output(), "WARDYN_GOVERNANCE_SECOND_HUMAN is not yet available: this build holds governance profile and assignment "+
			"writes for a second human, but the remaining writes it covers and the console are not pending-aware yet. Unset it.")
		os.Exit(2)
	}
}
