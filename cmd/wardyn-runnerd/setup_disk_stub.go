// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !docker

package main

import "context"

// This build has no Docker: no VM disk is measured and no disk cap is claimed either way.
func vmEngineDiskMiB(context.Context, string) (int64, bool) { return 0, false }

func diskCapEnforced(context.Context) (capped, ok bool) { return false, false }
