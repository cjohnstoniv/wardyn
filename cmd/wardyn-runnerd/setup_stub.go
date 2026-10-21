// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !docker

package main

import "context"

// The docker build holds setup's engine check and relay probe; this build cannot tell the engine is usable.
func readySetupEngine(context.Context, string, relayPair) (dockerEngine, error) {
	return dockerEngine{}, errSetupNeedsDocker
}

func probeSetupRelay(context.Context, string, string, dockerEngine, relayPair) (relayPair, error) {
	return relayPair{}, errSetupNeedsDocker
}

func inspectSetupEngine(context.Context, relayPair) (dockerEngine, error) {
	return dockerEngine{}, errSetupNeedsDocker
}

func resolveProxyImage(context.Context, string) (string, error) { return "", errSetupNeedsDocker }
