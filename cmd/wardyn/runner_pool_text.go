// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"regexp"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runner_pool_text.go — what `runner pool create`, `update` and `delete` say when they succeed. --json
// prints the pool as the server returned it, and nothing else.

// printPoolCreated says a pool now exists. A user pool made with no runner also says how to put one in,
// after its own limits when it was given any: only a pool with none follows its runners outright.
func printPoolCreated(w io.Writer, p sdk.RunnerPool, asJSON, withRunners bool) error {
	if asJSON {
		return emitJSON(w, p)
	}
	if p.Scope == sdk.RunnerPoolOrganization {
		_, err := fmt.Fprintf(w, "created organization pool %s (%s), revision %d\n", p.Name, p.ID, p.Revision)
		return err
	}
	if _, err := fmt.Fprintf(w, "created user pool %s (%s), yours, revision %d\n", p.Name, p.ID, p.Revision); err != nil || withRunners {
		return err
	}
	limits := "it follows its runners; it has none yet"
	if own := poolLimitsText(p, sdk.RunnerAmounts{}); own != "none of its own: it follows its runners" {
		limits = own + "; it has no runners yet"
	}
	_, err := fmt.Fprintf(w, "  limits: %s. Put a runner in it: wardyn runner pool add %s <runner>\n", limits, poolWord(p))
	return err
}

func printPoolUpdated(w io.Writer, p sdk.RunnerPool, asJSON bool) error {
	if asJSON {
		return emitJSON(w, p)
	}
	_, err := fmt.Fprintf(w, "updated pool %s, revision %d\n", p.Name, p.Revision)
	return err
}

func printPoolDeleted(w io.Writer, p sdk.RunnerPool) error {
	_, err := fmt.Fprintf(w, "pool %s (%s) deleted\n"+
		"  runs that were waiting for it ended: \"The pool this run was waiting for was removed.\"\n"+
		"  runs already running were not stopped; its runners were taken out of it, not touched\n", p.Name, p.ID)
	return err
}

// plainName is a pool name that is one safe word on any shell's command line.
var plainName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// poolWord is the pool as a printed command names it: by its name when that is one plain word, so the
// line reads as the design's, and otherwise by its id, which is always safe to paste.
func poolWord(p sdk.RunnerPool) string {
	if plainName.MatchString(p.Name) {
		return p.Name
	}
	return p.ID.String()
}
