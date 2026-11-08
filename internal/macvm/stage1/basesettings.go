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
// outside them is kept as empty, so that no base-settings row is rendered (baseSettingCommands refuses it);
// it never fails a prepare, as the guest agent's route treats the same case as a skip too.
func (s *session) capture(c command, out string) error {
	switch c.Capture {
	case captureVersion:
		s.release.Version, _ = basesettings.ParseVersion(out)
	case captureBuild:
		s.release.Build, _ = basesettings.ParseBuild(out)
	}
	return nil
}

// baseSettingCommands is the base-settings table as stage-1 commands, rendered from the release just
// read. The agent's rows run through asAgent, inside the root row's sudo, as the screen saver row does.
// Every row is Optional here: a setting that cannot be written never stops a base from being published,
// and the next tools update tries it again.
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
		out[i] = command{ID: r.Name, Step: r.Step, Root: true, Argv: argv, Optional: true}
	}
	return out, nil
}
