// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import "time"

// ContainmentLag is the api's containmentLag: how long after a run's end the containment loop owns it.
// It is longer than the longest end hold, 4 min 30 s. It lives here because the api imports store,
// never the reverse.
const ContainmentLag = 6 * time.Minute

// ContainmentDueSQL is the store's SQL for the api's containmentDue; change both together.
const ContainmentDueSQL = "(COALESCE(ended_at, updated_at) + interval '6 minutes')"
