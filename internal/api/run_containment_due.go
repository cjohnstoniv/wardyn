// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// containmentLag is L: how long after a run's end the containment loop owns it. It is longer than the
// longest end hold, 4 min 30 s.
const containmentLag = store.ContainmentLag
