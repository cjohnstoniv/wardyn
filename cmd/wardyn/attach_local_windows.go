// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import "errors"

// localAttachOS: no local transport on Windows, where the runner lives in WSL2 (local attach from
// Windows is the ATT-WSL spike). auto attaches through the organisation; --via local refuses.
const localAttachOS = false

func defaultLocalAttachDir() (string, error) { return "", errors.New(localAttachWindowsRefusal) }

func localSocketFor(_, _, _ string) (string, string) { return "", localAttachWindowsRefusal }
