// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"context"
	"errors"
	"testing"
)

// capableStore is the fallback fake plus every optional capability the audit
// wrapper forwards.
type capableStore struct {
	*fallbackStore
	rev string
}

func (c *capableStore) Revision(context.Context, string) (string, error) { return c.rev, nil }
func (c *capableStore) StoresExternally() string                         { return "vault://kv" }
func (c *capableStore) KeyService() string                               { return "transit" }
func (c *capableStore) MarkUsed(context.Context, string) error           { return nil }
func (c *capableStore) Metadata(_ context.Context, names []string) ([]Meta, error) {
	return make([]Meta, len(names)), nil
}
func (c *capableStore) MetadataEverywhere(_ context.Context, names []string) ([]Meta, error) {
	return make([]Meta, 2*len(names)), nil
}
func (c *capableStore) DeleteExpired(context.Context) ([]Expired, error) {
	return []Expired{{Owner: "u", Name: "n"}}, nil
}

func newBare() *fallbackStore { return &fallbackStore{rows: map[[2]string][]byte{}} }

func TestAuditedForwardsTheCapabilitiesTheInnerStoreHas(t *testing.T) {
	ctx := context.Background()
	a := Audited(&capableStore{fallbackStore: newBare(), rev: "r7"}, &recorded{})

	if rev, err := a.(Revisioned).Revision(ctx, "n"); err != nil || rev != "r7" {
		t.Fatalf("Revision = %q, %v; want r7", rev, err)
	}
	if got := a.(interface{ StoresExternally() string }).StoresExternally(); got != "vault://kv" {
		t.Fatalf("StoresExternally = %q", got)
	}
	if got := a.(interface{ KeyService() string }).KeyService(); got != "transit" {
		t.Fatalf("KeyService = %q", got)
	}
	m := a.(MetaStore)
	if err := m.MarkUsed(ctx, "n"); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if got, err := m.Metadata(ctx, []string{"a"}); err != nil || len(got) != 1 {
		t.Fatalf("Metadata = %v, %v", got, err)
	}
	if got, err := m.MetadataEverywhere(ctx, []string{"a"}); err != nil || len(got) != 2 {
		t.Fatalf("MetadataEverywhere = %v, %v", got, err)
	}
	exp, err := a.(interface {
		DeleteExpired(context.Context) ([]Expired, error)
	}).DeleteExpired(ctx)
	if err != nil || len(exp) != 1 || exp[0].Name != "n" {
		t.Fatalf("DeleteExpired = %v, %v", exp, err)
	}
}

func TestAuditedAnswersHonestlyWhenTheInnerStoreLacksACapability(t *testing.T) {
	ctx := context.Background()
	a := Audited(newBare(), &recorded{})

	if _, err := a.(Revisioned).Revision(ctx, "n"); !errors.Is(err, ErrNoRevision) {
		t.Fatalf("Revision err = %v, want ErrNoRevision", err)
	}
	if got := a.(interface{ StoresExternally() string }).StoresExternally(); got != "" {
		t.Fatalf("StoresExternally = %q, want empty", got)
	}
	if got := a.(interface{ KeyService() string }).KeyService(); got != "" {
		t.Fatalf("KeyService = %q, want empty", got)
	}
	m := a.(MetaStore)
	if err := m.MarkUsed(ctx, "n"); !errors.Is(err, ErrNoMetadata) {
		t.Fatalf("MarkUsed err = %v, want ErrNoMetadata", err)
	}
	if _, err := m.Metadata(ctx, nil); !errors.Is(err, ErrNoMetadata) {
		t.Fatalf("Metadata err = %v, want ErrNoMetadata", err)
	}
	if _, err := m.MetadataEverywhere(ctx, nil); !errors.Is(err, ErrNoMetadata) {
		t.Fatalf("MetadataEverywhere err = %v, want ErrNoMetadata", err)
	}
	if _, err := a.(interface {
		DeleteExpired(context.Context) ([]Expired, error)
	}).DeleteExpired(ctx); !errors.Is(err, ErrNoExpirySweep) {
		t.Fatalf("DeleteExpired err = %v, want ErrNoExpirySweep", err)
	}
}

func TestAuditedUnwrapReachesTheInnerStoreAcrossFor(t *testing.T) {
	inner := newBare()
	a := Audited(inner, &recorded{})
	if got := a.(interface{ Unwrap() Store }).Unwrap(); got != Store(inner) {
		t.Fatalf("Unwrap = %v, want the inner store", got)
	}
}

func TestRevisionOf(t *testing.T) {
	ctx := context.Background()
	if _, ok, err := RevisionOf(ctx, newBare(), "n"); ok || err != nil {
		t.Fatalf("a store with no revision seam: ok=%v err=%v, want false,nil", ok, err)
	}
	// An audit wrapper over a store with no revision answers ErrNoRevision,
	// which RevisionOf reads as "keeps none", not as a failure.
	if _, ok, err := RevisionOf(ctx, Audited(newBare(), &recorded{}), "n"); ok || err != nil {
		t.Fatalf("wrapped bare store: ok=%v err=%v, want false,nil", ok, err)
	}
	rev, ok, err := RevisionOf(ctx, &capableStore{fallbackStore: newBare(), rev: "r9"}, "n")
	if rev != "r9" || !ok || err != nil {
		t.Fatalf("RevisionOf = %q,%v,%v; want r9,true,nil", rev, ok, err)
	}
}
