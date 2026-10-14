// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package keystore holds a runner's runner_resident values in the operating
// system's keystore (design 0.9 §7.4). There is no file fallback: a keystore
// that cannot be written refuses the delivery.
package keystore

import "errors"

// ErrUnavailable marks a keystore that cannot hold or return a value.
var ErrUnavailable = errors.New("keystore: the OS keystore is unavailable")

// service names every item wardyn-runnerd stores.
const service = "wardyn-runnerd"
