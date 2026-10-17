// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"slices"

	"github.com/google/uuid"
)

// selectionDomain versions the rendezvous score. Changing how the score is built is a new domain, so
// two server versions never disagree about a pinned run's winner by accident.
const selectionDomain = "wardyn/runner-pool-selection/v1"

// Select picks the one target a run goes to from the fully eligible targets of its pool: the highest
// SHA-256 rendezvous score over an unambiguous (length-prefixed) encoding of the domain, the pool id,
// the server-allocated run id and the stable target id; a bytewise tie on the target id breaks the
// theoretical hash tie. No list order, label, clock or random draw takes part, so the same inputs
// choose the same target on every replica and on a retry, and eligible requests spread across the
// pool without any claim to know its free capacity. It returns "" for no targets.
func Select(pool, run uuid.UUID, eligible []string) string {
	best, bestScore := "", [sha256.Size]byte{}
	for _, id := range eligible {
		score := selectionScore(pool, run, id)
		if best == "" || bytes.Compare(score[:], bestScore[:]) > 0 || (score == bestScore && id < best) {
			best, bestScore = id, score
		}
	}
	return best
}

func selectionScore(pool, run uuid.UUID, target string) [sha256.Size]byte {
	var buf []byte
	for _, field := range [][]byte{[]byte(selectionDomain), pool[:], run[:], []byte(target)} {
		buf = binary.BigEndian.AppendUint32(buf, uint32(len(field)))
		buf = append(buf, field...)
	}
	return sha256.Sum256(buf)
}

// SortedUnique returns ids in bytewise order without repeats, the stable order every caller hands
// Select and shows.
func SortedUnique(ids []string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}
