// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package component provides the shared registry that backs Wardyn's pluggable
// component seams (identity provider, secret store, recording store, policy
// evaluator, …). One generic implementation of name→constructor registration,
// default resolution, and duplicate detection, so every seam selects an
// implementation uniformly via a WARDYN_<SEAM> name while keeping a type-safe,
// seam-specific constructor signature.
//
// Implementations self-register at init() time (like database/sql drivers): a
// blank import of an impl package makes it selectable. The registered default
// name maps to the current built-in, so an unset selector reproduces today's
// behavior exactly.
//
// A registered implementation is not "blessed" until it passes its seam's
// RunConformance suite (see the per-seam *test packages) — registration makes an
// impl selectable; conformance makes it trustworthy.
package component

import (
	"fmt"
	"sort"
	"sync"
)

// Registry is a name→constructor registry for one pluggable seam. C is the
// seam's constructor FUNC type (e.g. func(Deps) (Provider, error)), so each seam
// stays type-safe while sharing this implementation. Construct with NewRegistry.
type Registry[C any] struct {
	mu    sync.RWMutex
	ctors map[string]C
	def   string
}

// NewRegistry returns an empty registry whose default selection is def.
func NewRegistry[C any](def string) *Registry[C] {
	return &Registry[C]{ctors: make(map[string]C), def: def}
}

// Register adds ctor under name. It PANICS on an empty or duplicate name: this
// is an init-time programmer error (cf. sql.Register), surfaced loudly rather
// than silently shadowing a built-in.
func (r *Registry[C]) Register(name string, ctor C) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == "" {
		panic("component: Register called with an empty name")
	}
	if _, dup := r.ctors[name]; dup {
		panic(fmt.Sprintf("component: duplicate registration for %q", name))
	}
	r.ctors[name] = ctor
}

// Lookup returns the constructor for name (an empty name resolves to the
// default) and whether it was found.
func (r *Registry[C]) Lookup(name string) (C, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name == "" {
		name = r.def
	}
	c, ok := r.ctors[name]
	return c, ok
}

// Names returns the registered names sorted (for /healthz and error messages).
func (r *Registry[C]) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.ctors))
	for n := range r.ctors {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Default returns the default component name.
func (r *Registry[C]) Default() string { return r.def }

// Resolve maps name (empty → default) to its constructor and the resolved name,
// or a helpful error listing the registered names. Seam New() wrappers call this.
func (r *Registry[C]) Resolve(name string) (ctor C, resolved string, err error) {
	resolved = name
	if resolved == "" {
		resolved = r.def
	}
	c, ok := r.Lookup(resolved)
	if !ok {
		var zero C
		return zero, resolved, fmt.Errorf("component %q is not registered (available: %v)", resolved, r.Names())
	}
	return c, resolved, nil
}
