// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package admission

// The ledger runs only on macOS (the helper is darwin-only) and is exercised on
// Linux; it needs unix file, lock and socket semantics, so no file in the package
// builds elsewhere. This empty file keeps `GOOS=windows go build/vet` of the
// package green (CI runs it) and makes any importer fail to compile there on the
// undefined admission symbols rather than get a stub that appears to work.
