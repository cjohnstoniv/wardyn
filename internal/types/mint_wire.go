// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// Mint 409-conflict "code" values: the machine-readable
// discriminator between the four distinct fail-closed conditions that all share
// HTTP 409 on POST /wardyn/v1/credentials/mint, so the caller can name the real
// cause instead of guessing from which optional fields happen to be present.
//
// THIS IS THE ONE HOME: internal/api's mint handler and cmd/wardyn-git-helper's
// client both read these, and internal/api's tests assert the LITERAL strings,
// which is what pins a wire contract.
//
// internal/types is a leaf package (no internal imports), so the sandbox-side
// helper takes on no dependency by reading them here.
const (
	// MintConflictPending — the approval is still open; approval_id is set and
	// the caller may poll.
	MintConflictPending = "pending"
	// MintConflictDenied — a human denied the approval; denied/reason are set.
	MintConflictDenied = "denied"
	// MintConflictScopeMismatch — the requested scope widens the grant.
	MintConflictScopeMismatch = "scope_mismatch"
	// MintConflictAlreadyMinted — the grant's single-use slot is already spent.
	MintConflictAlreadyMinted = "already_minted"
)
