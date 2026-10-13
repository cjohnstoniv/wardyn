// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build race

package api

// raceSlowdown scales a wall-clock bound: the race detector runs this code about five times slower.
const raceSlowdown = 5
