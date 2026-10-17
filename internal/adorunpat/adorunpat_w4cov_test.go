// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adorunpat

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

var w4CovRun = uuid.MustParse("dddddddd-0000-0000-0000-000000000001")

// w4CovKeys hands out one key (or one error) and records the Current lookups.
type w4CovKeys struct {
	key     []byte
	err     error
	current []string
}

func (k *w4CovKeys) Current(_ context.Context, owner, purpose string) (int, []byte, error) {
	k.current = append(k.current, owner+"/"+purpose)
	if k.err != nil {
		return 0, nil, k.err
	}
	return 4, bytes.Clone(k.key), nil
}

func (k *w4CovKeys) Key(context.Context, string, string, int) ([]byte, error) {
	return nil, errors.New("w4cov: Key is not part of a Save")
}

func TestW4CovAADBindsRunRenderingAndVersion(t *testing.T) {
	other := uuid.MustParse("dddddddd-0000-0000-0000-000000000002")
	base := aad(w4CovRun, renderingToken, 3)
	if !bytes.Equal(base, aad(w4CovRun, renderingToken, 3)) {
		t.Fatal("aad is not deterministic")
	}
	for name, o := range map[string][]byte{
		"run":       aad(other, renderingToken, 3),
		"rendering": aad(w4CovRun, "basic", 3),
		"version":   aad(w4CovRun, renderingToken, 4),
	} {
		if bytes.Equal(base, o) {
			t.Errorf("changing the %s did not change the aad", name)
		}
	}
}

func TestW4CovSaveRefusesBeforeTouchingPostgres(t *testing.T) {
	ctx := context.Background()

	t.Run("no owner", func(t *testing.T) {
		keys := &w4CovKeys{}
		err := New(nil, keys).Save(ctx, w4CovRun, "", Record{Token: "a-token"})
		if !errors.Is(err, maskmanifest.ErrNoOwner) {
			t.Fatalf("Save = %v, want ErrNoOwner", err)
		}
		if len(keys.current) != 0 {
			t.Errorf("an ownerless save asked for a key: %v", keys.current)
		}
	})

	t.Run("the owner's key is unavailable", func(t *testing.T) {
		injected := errors.New("key service down")
		keys := &w4CovKeys{err: injected}
		err := New(nil, keys).Save(ctx, w4CovRun, "alice", Record{Token: "a-token"})
		if !errors.Is(err, injected) || !strings.Contains(err.Error(), "adorunpat: the owner's key") {
			t.Fatalf("Save = %v, want the injected error wrapped with context", err)
		}
		if len(keys.current) != 1 || keys.current[0] != "alice/"+subjectkey.PurposeCred {
			t.Errorf("key lookups = %v, want one for alice's cred key", keys.current)
		}
	})

	t.Run("the key cannot seal", func(t *testing.T) {
		keys := &w4CovKeys{key: []byte("too-short")}
		err := New(nil, keys).Save(ctx, w4CovRun, "alice", Record{Token: "a-token"})
		if err == nil || !strings.Contains(err.Error(), "adorunpat: seal the run's token") {
			t.Fatalf("Save = %v, want the seal failure", err)
		}
	})
}
