// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package stage1

import (
	"github.com/cjohnstoniv/wardyn/internal/macvm/basesettings"
)

// capture says which of sw_vers's outputs a command's stdout is.
type capture int

const (
	captureNone capture = iota
	captureVersion
	captureBuild
)

// capture keeps the output of a sw_vers command once it is checked against the fixed patterns. A value
// outside them fails the command's step, and no base-settings row runs.
func (s *session) capture(c command, out string) error {
	var err error
	switch c.Capture {
	case captureVersion:
		s.release.Version, err = basesettings.ParseVersion(out)
	case captureBuild:
		s.release.Build, err = basesettings.ParseBuild(out)
	}
	return err
}

// baseSettingCommands is the base-settings table as stage-1 commands, rendered from the release just
// read. The agent's rows run through asAgent, inside the root row's sudo, as the screen saver row does.
func (s *session) baseSettingCommands() ([]command, error) {
	rows, err := basesettings.Stage1Rows(s.release)
	if err != nil {
		return nil, err
	}
	out := make([]command, len(rows))
	for i, r := range rows {
		argv := r.Argv
		if r.User == basesettings.Agent {
			argv = asAgent(argv...)
		}
		out[i] = command{ID: r.Name, Step: r.Step, Root: true, Argv: argv, Optional: r.Optional}
	}
	return out, nil
}
