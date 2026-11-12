// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/macvm/basesettings"
)

// maxWarningNames is how many setting names one doctor line lists before it says how many more.
const maxWarningNames = 6

// BaseSettingsWarnings turns tools.json's base_settings_skipped into doctor WARN lines: one per reason, in
// the order the reasons first appear, naming the settings and cutting a long list short. It returns nil when
// nothing was skipped.
func BaseSettingsWarnings(skipped []basesettings.Skipped) []string {
	var reasons []string
	names := map[string][]string{}
	for _, s := range skipped {
		if _, ok := names[s.Reason]; !ok {
			reasons = append(reasons, s.Reason)
		}
		names[s.Reason] = append(names[s.Reason], s.Name)
	}
	var lines []string
	for _, reason := range reasons {
		list := names[reason]
		text := strings.Join(list, ", ")
		if len(list) > maxWarningNames {
			text = strings.Join(list[:maxWarningNames], ", ") + fmt.Sprintf(" and %d more", len(list)-maxWarningNames)
		}
		lines = append(lines, "base settings skipped, "+reason+": "+text)
	}
	return lines
}
