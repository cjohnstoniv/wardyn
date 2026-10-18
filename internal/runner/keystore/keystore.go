// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package keystore holds a runner's runner_resident values in the operating
// system's keystore (design 0.9 §7.4). There is no file fallback: a keystore
// that cannot be written refuses the delivery.
package keystore

import "errors"

// ErrUnavailable marks a keystore that cannot hold or return a value.
var ErrUnavailable = errors.New("keystore: the OS keystore is unavailable")

// Store is one runner's items in the OS keystore. Each runner has its own service,
// wardyn-runnerd:<runner-id>, so two runner daemons for one OS user never touch
// each other's items.
type Store struct{ service string }

// For is the store of the runner runnerID.
func For(runnerID string) Store { return Store{service: "wardyn-runnerd:" + runnerID} }
