// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"

	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
)

// seedCLIProfile points the wardyn CLI at the organisation this computer was set up for, so
// `wardyn run attach <id>` needs no URL. A profile that already names a URL is the person's choice and is
// kept. It installs no CLI: the CLI is the person's, installed before this runner.
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
		fmt.Fprintf(out, "The wardyn CLI now points at %s (change it with wardyn login <url>).\n", org)
	default:
		fmt.Fprintf(out, "The wardyn CLI profile points at %s, not %s; left it as it is (wardyn login <url> changes it).\n", have, org)
	}
	return nil
}
