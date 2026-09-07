// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// Mint 409-conflict "code" values (W19-W19a-2): the machine-readable
// discriminator between the four distinct fail-closed conditions that all share
// HTTP 409 on POST /wardyn/v1/credentials/mint, so the caller can name the real
// cause instead of guessing from which optional fields happen to be present.
//
// THIS IS THE ONE HOME. The values were declared twice — internal/api's mint
// handler and cmd/wardyn-git-helper's client — and the helper's copy claimed to
// be "the ONE place both sides of this wire contract must agree", which it was
// not: the two blocks could drift silently, and the server-side tests compared
// the decoded JSON against the very constants the handler had written, so all
// four could be renamed with the suite green (F134). Both sides now read these,
// and internal/api's tests assert the LITERAL strings, which is what actually
// pins a wire contract.
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
