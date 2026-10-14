// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"filippo.io/age"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
)

// Key use of `wardynd -recovery-rehearse` (recovery-set.md §4.2): every stored row is opened in memory
// with the keys this process is configured with, over the one target backend, and nothing it opened
// leaves this file as anything but a count and a row reference.

// rehearseKeyStore is the pg secret store built from the deployment's key configuration with no
// pool, and the age identity pre-envelope rows open under (nil with none). It never converts a row:
// that is what newSecretStore does, and the target is read-only. A deployment with no key at all
// refuses here rather than mint an ephemeral one that would fail every row for the wrong reason.
func rehearseKeyStore(ctx context.Context, f *bootFlags) (*secretstorepg.Store, *age.X25519Identity, error) {
	platform, err := readPlatformKey(*f.platformKeyFile, *f.ageKey)
	if err != nil {
		return nil, nil, err
	}
	c, err := buildStoreClients(ctx, f)
	if err != nil {
		return nil, nil, err
	}
	storeMode := *f.secretStoreSel != "" && *f.secretStoreSel != "pg"
	if *f.ageKey == "" && !storeMode && !(c.kek != nil && c.kekWrites) {
		return nil, nil, errors.New("-recovery-rehearse needs the deployment's keys: set WARDYN_AGE_KEY (or WARDYN_KEK and its settings), the same as the deployment that wrote the set")
	}
	s, id, err := unconvertedSecretStore(nil, *f.ageKey, platform, *f.secretStoreSel, c)
	if err != nil {
		return nil, nil, err
	}
	ps, ok := s.(*secretstorepg.Store)
	if !ok {
		return nil, nil, fmt.Errorf("secret store %q has no rows to open", s.Name())
	}
	return ps, id, nil
}

// keyUseResult is what opening every row found. Classes counts rows per key class, the figure the
// manifest holds, so "every row was tried" is a comparison and not a claim.
type keyUseResult struct {
	Classes map[string]int
	Counts  map[string]int
	Failing []string
}

// keyClassOf names the key that seals a row, as the manifest counts them: the kek_id for an
// envelope, the generation for a principal key, the store for a pointer, "age" before envelopes.
func keyClassOf(c secretstorepg.CheckedRow) string {
	switch {
	case c.PrincipalKey:
		return "principal-key/" + c.Name + ":" + c.KekID
	case c.EncVersion == 0:
		return "age"
	case c.Outcome == secretstorepg.RowExternal:
		store, _, _ := strings.Cut(c.KekID, ":")
		return "external:" + store
	}
	return c.KekID
}

// keyUse opens every row over the target backend. A row that does not open is a result; only a
// failure to read the tables is an error.
func (s *rehearseSession) keyUse(ctx context.Context, store *secretstorepg.Store, legacy *age.X25519Identity) (keyUseResult, error) {
	res := keyUseResult{Classes: map[string]int{}, Counts: map[string]int{}}
	var opener age.Identity // a nil *X25519Identity in the interface would read as a key
	if legacy != nil {
		opener = legacy
	}
	err := store.CheckRows(secretstore.WithPurpose(ctx, secretstore.PurposeRehearse), s.conn(), opener, func(c secretstorepg.CheckedRow) {
		class := keyClassOf(c)
		res.Classes[class]++
		res.Counts[strings.ReplaceAll(string(c.Outcome), " ", "_")]++
		if c.Outcome == secretstorepg.RowFailed {
			res.Failing = append(res.Failing, class+": "+c.Err.Error())
		}
	})
	return res, err
}
