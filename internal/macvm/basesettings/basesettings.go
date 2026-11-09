// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package basesettings is the one table of settings Wardyn writes into a macOS base so that a run's first
// desktop is clean: Setup Assistant's panes marked as seen for the agent account, no Dictation or Fn-key
// prompt, no Spotlight indexing, no diagnostics upload, and no Terminal windows restored from an earlier
// session.
//
// Stage 1 writes the rows over SSH when it prepares a base, and the guest agent's base-settings route
// writes them again at every tools update (a base's stage 1 runs once, so an existing base changes only
// through the update). Both read this table, so there is one set of rows and no drift between them. The
// package imports none of macvm, stage1 or the guest agent, which import it.
//
// Every key is an Apple preference or a documented command, and the ones whose names come from no
// documentation are Optional: a failed Optional row is skipped and reported, never fatal to a prepare.
package basesettings

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// User is the account a row runs as.
type User string

const (
	// Root rows run as root, directly.
	Root User = "root"
	// Agent rows run as the agent account: through sudo -H -u agent in stage 1, and in the console
	// session (launchctl asuser) in the guest agent's route.
	Agent User = "agent"
)

// Step is the stage-1 step the rows belong to; it runs after 7d and before 7e's Remote Login off.
const Step = "7f"

// Row is one setting. Argv is a whole command, absolute path first, and runs with no shell.
type Row struct {
	Name     string
	User     User
	Argv     []string
	Optional bool
	Step     string
	// Check, when set, is read back from disk after the rows are written.
	Check *Check
	// Stage2Only keeps the row out of stage 1: only the guest agent's route writes it.
	Stage2Only bool
	// Read, when set, is read back by running it as the row's user, once and with no retry.
	Read *Read
	// Protected marks a row whose write goes into a container macOS protects: a write macOS refuses with
	// "Could not write domain" needs Full Disk Access for the guest agent.
	Protected bool
}

// Read is a read-back by command: Argv, run as the row's user, must print Want (trailing space aside).
type Read struct {
	Argv []string
	Want string
}

// Check reads one key of a property list back with `plutil -p Path`. Want holds the forms plutil prints
// for the value written: a bool prints as 1 or true, a string in quotes.
type Check struct {
	Path string
	Key  string
	Want []string
}

// Holds says whether plutil's output lists Key at the top level with one of the forms in Want.
func (c Check) Holds(plutilOutput string) bool {
	for _, line := range strings.Split(plutilOutput, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), " => ")
		if !ok || strings.Trim(key, `"`) != c.Key {
			continue
		}
		for _, w := range c.Want {
			if val == w {
				return true
			}
		}
	}
	return false
}

// Kill is a process the guest agent's route ends by signal before it writes: with no quit handler it cannot
// save its resume state over the rows. A process that is not running counts as ended.
type Kill struct {
	Name    string
	Process string // the process name, as `pkill -x` matches it
	// Terminal marks the entries edit-base's finish runs on their own.
	Terminal bool
}

// Removal is a saved state the route removes as the agent after the rows are written.
type Removal struct {
	Name     string
	Path     string
	Terminal bool
}

// Skipped is a row (or a whole route) that did not apply, with the reason. tools.json records the list as
// base_settings_skipped.
type Skipped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Kills are ended first, by the agent's uid. Setup Assistant is killed so it does not rewrite its keys
// over the ones the route writes; Terminal so that the saved state removed afterwards is not written
// back.
var Kills = []Kill{
	{Name: "kill.setup-assistant", Process: "Setup Assistant"},
	{Name: "kill.terminal", Process: "Terminal", Terminal: true},
}

// Removals run after the rows, as the agent.
var Removals = []Removal{
	{Name: "remove.terminal-saved-state", Path: "/Users/agent/Library/Saved Application State/com.apple.Terminal.savedState", Terminal: true},
}

// ErrRelease is a macOS product version or build outside the fixed patterns.
var ErrRelease = errors.New("basesettings: the guest's macOS version or build is not in the expected form")

var (
	versionRe = regexp.MustCompile(`^\d+\.\d+(\.\d+)?$`)
	buildRe   = regexp.MustCompile(`^[0-9]{2}[A-Z][0-9]{1,4}[a-z]?$`)
)

// Release is the guest's macOS: sw_vers -productVersion and -buildVersion. The stamps the rows write are
// rendered from it, so a guest macOS update is covered by the next tools update.
type Release struct{ Version, Build string }

// ParseVersion takes sw_vers -productVersion's output, trailing space removed, and checks its form.
func ParseVersion(out string) (string, error) {
	v := strings.TrimSpace(out)
	if !versionRe.MatchString(v) {
		return "", fmt.Errorf("%w: product version %q", ErrRelease, truncate(v))
	}
	return v, nil
}

// ParseBuild takes sw_vers -buildVersion's output, trailing space removed, and checks its form.
func ParseBuild(out string) (string, error) {
	b := strings.TrimSpace(out)
	if !buildRe.MatchString(b) {
		return "", fmt.Errorf("%w: build %q", ErrRelease, truncate(b))
	}
	return b, nil
}

// ParseRelease checks both sw_vers outputs.
func ParseRelease(version, build string) (Release, error) {
	v, err := ParseVersion(version)
	if err != nil {
		return Release{}, err
	}
	b, err := ParseBuild(build)
	if err != nil {
		return Release{}, err
	}
	return Release{Version: v, Build: b}, nil
}

func truncate(s string) string {
	if len(s) > 32 {
		return s[:32] + "..."
	}
	return s
}

const (
	tokVersion = "{{version}}"
	tokBuild   = "{{build}}"
)

// Stage1Rows is Rows without the rows only the guest agent's route writes.
func Stage1Rows(r Release) ([]Row, error) {
	rows, err := Rows(r)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(rows, func(r Row) bool { return r.Stage2Only }), nil
}

// Rows is the table with the release's values in it, in the order they run. A release outside the patterns
// is ErrRelease and no row is made.
func Rows(r Release) ([]Row, error) {
	if _, err := ParseRelease(r.Version, r.Build); err != nil {
		return nil, err
	}
	render := strings.NewReplacer(tokVersion, r.Version, tokBuild, r.Build).Replace
	out := make([]Row, len(table))
	for i, t := range table {
		row := t
		row.Argv = make([]string, len(t.Argv))
		for j, w := range t.Argv {
			row.Argv[j] = render(w)
		}
		if t.Check != nil {
			c := *t.Check
			c.Want = make([]string, len(t.Check.Want))
			for j, w := range t.Check.Want {
				c.Want[j] = render(w)
			}
			row.Check = &c
		}
		out[i] = row
	}
	return out, nil
}
