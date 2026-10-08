// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeComponentStore is an in-memory store.ComponentStore with the real store's
// scoping: every read and write is owner-scoped, so another person's row is
// ErrNotFound, and a name is unique per owner. A zero value works, so a double
// embeds it by value. It records the restrictions CreateRestrictedComponent
// would write, to let a test see them without a database; the transaction
// itself is proved against Postgres.
type fakeComponentStore struct {
	cmu        sync.Mutex
	components map[uuid.UUID]types.Component
	restricted map[string]bool // capability + "/" + id
	createErr  error           // CreateRestrictedComponent fails with this, restricting nothing
}

func (f *fakeComponentStore) ensure() {
	if f.components == nil {
		f.components = map[uuid.UUID]types.Component{}
		f.restricted = map[string]bool{}
	}
}

// seedComponent stores a person's row and returns its id.
func (f *fakeComponentStore) seedComponent(owner string) uuid.UUID {
	c, err := f.CreateComponent(context.Background(), types.Component{ID: uuid.New(), Owner: owner, Name: "seed-" + uuid.NewString()})
	if err != nil {
		panic(err)
	}
	return c.ID
}

func (f *fakeComponentStore) nameTaken(c types.Component) bool {
	for _, o := range f.components {
		if o.Owner == c.Owner && o.Name == c.Name && o.ID != c.ID {
			return true
		}
	}
	return false
}

func (f *fakeComponentStore) CreateComponent(_ context.Context, c types.Component) (types.Component, error) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.ensure()
	if _, dup := f.components[c.ID]; dup || f.nameTaken(c) {
		return types.Component{}, store.ErrConflict
	}
	c.Version, c.CreatedAt, c.UpdatedAt = 1, time.Now(), time.Now()
	f.components[c.ID] = c
	return c, nil
}

func (f *fakeComponentStore) CreateRestrictedComponent(ctx context.Context, c types.Component, capability, _ string) (types.Component, error) {
	if f.createErr != nil {
		return types.Component{}, f.createErr
	}
	created, err := f.CreateComponent(ctx, c)
	if err != nil {
		return types.Component{}, err
	}
	f.cmu.Lock()
	f.restricted[capability+"/"+c.ID.String()] = true
	f.cmu.Unlock()
	return created, nil
}

func (f *fakeComponentStore) DeleteRestrictedComponent(ctx context.Context, id uuid.UUID, capability, _ string) (types.Component, int, error) {
	deleted, err := f.DeleteComponent(ctx, id, "")
	if err != nil {
		return types.Component{}, 0, err
	}
	f.cmu.Lock()
	f.restricted[capability+"/"+id.String()] = true
	f.cmu.Unlock()
	return deleted, 0, nil
}

func (f *fakeComponentStore) UpdateComponent(_ context.Context, c types.Component) (types.Component, error) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.ensure()
	cur, ok := f.components[c.ID]
	if !ok || cur.Owner != c.Owner {
		return types.Component{}, store.ErrNotFound
	}
	if f.nameTaken(c) {
		return types.Component{}, store.ErrConflict
	}
	cur.Name, cur.Definition, cur.Version, cur.UpdatedAt = c.Name, c.Definition, cur.Version+1, time.Now()
	f.components[c.ID] = cur
	return cur, nil
}

func (f *fakeComponentStore) DeleteComponent(_ context.Context, id uuid.UUID, owner string) (types.Component, error) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.ensure()
	cur, ok := f.components[id]
	if !ok || cur.Owner != owner {
		return types.Component{}, store.ErrNotFound
	}
	delete(f.components, id)
	return cur, nil
}

func (f *fakeComponentStore) GetComponent(_ context.Context, id uuid.UUID, owner string) (types.Component, error) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.ensure()
	cur, ok := f.components[id]
	if !ok || cur.Owner != owner {
		return types.Component{}, store.ErrNotFound
	}
	return cur, nil
}

func (f *fakeComponentStore) ListComponents(_ context.Context, owner string) ([]types.Component, error) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.ensure()
	out := []types.Component{}
	for _, c := range f.components {
		if c.Owner == owner {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeComponentStore) ListComponentsByIDs(_ context.Context, owner string, ids []uuid.UUID) ([]types.Component, error) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.ensure()
	out := []types.Component{}
	for _, id := range ids {
		if c, ok := f.components[id]; ok && (c.Owner == "" || c.Owner == owner) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeComponentStore) CountComponents(ctx context.Context, owner string) (int, error) {
	rows, err := f.ListComponents(ctx, owner)
	return len(rows), err
}

func (f *fakeComponentStore) PutRunComponents(context.Context, uuid.UUID, []types.RunComponent) error {
	return nil
}

func (f *fakeComponentStore) ListRunComponents(context.Context, uuid.UUID) ([]types.RunComponent, error) {
	return []types.RunComponent{}, nil
}

func (f *fakeComponentStore) DeleteComponentsByOwner(context.Context, string) (int, error) {
	return 0, nil
}

func (f *fakeComponentStore) EraseRunComponentsByOwner(context.Context, string) (int, error) {
	return 0, nil
}

var (
	_ store.ComponentStore             = (*fakeComponentStore)(nil)
	_ store.RestrictedComponentCreator = (*fakeComponentStore)(nil)
	_ store.RestrictedComponentDeleter = (*fakeComponentStore)(nil)
)
