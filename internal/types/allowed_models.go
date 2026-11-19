// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxAllowedModels and MaxAllowedModelLen bound GovernanceLimits.AllowedModels: at most 200 names of at
// most 200 characters each.
const (
	MaxAllowedModels   = 200
	MaxAllowedModelLen = 200
)

// ValidAllowedModels refuses an allowed-models list the run's proxy would refuse to load, or one past its
// bounds: an empty entry, one with leading or trailing space, one listed twice, a control character, an
// entry longer than MaxAllowedModelLen, or more than MaxAllowedModels entries. The message names no field;
// the caller adds it. An empty list is valid and means no limit.
func ValidAllowedModels(list []string) error {
	if len(list) > MaxAllowedModels {
		return fmt.Errorf("at most %d models", MaxAllowedModels)
	}
	seen := make(map[string]bool, len(list))
	for i, m := range list {
		switch {
		case m == "":
			return fmt.Errorf("entry %d is empty", i+1)
		case strings.TrimSpace(m) != m:
			return fmt.Errorf("%q has leading or trailing space", m)
		case strings.ContainsFunc(m, unicode.IsControl) || !utf8.ValidString(m):
			return fmt.Errorf("entry %d has a control character", i+1)
		case utf8.RuneCountInString(m) > MaxAllowedModelLen:
			return fmt.Errorf("entry %d is longer than %d characters", i+1, MaxAllowedModelLen)
		case seen[m]:
			return fmt.Errorf("%q is listed twice", m)
		}
		seen[m] = true
	}
	return nil
}
