// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"context"
	"errors"
	"testing"
)

func TestRegistry_DefaultSelectionAndList(t *testing.T) {
	a := &FakeComposer{Result: Proposal{Summary: "from-a"}}
	b := &FakeComposer{Result: Proposal{Summary: "from-b"}}
	reg, err := NewRegistry("b", []RegistryEntry{
		{Info: BackendInfo{Name: "a", Provider: "anthropic", Model: "claude"}, Composer: a},
		{Info: BackendInfo{Name: "b", Provider: "openai", Model: "gpt"}, Composer: b},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reg.Enabled() || reg.Default() != "b" {
		t.Fatalf("default = %q enabled=%v", reg.Default(), reg.Enabled())
	}
	// empty backend → default (b).
	p, err := reg.Propose(context.Background(), "", ComposeRequest{Prompt: "x"})
	if err != nil || p.Summary != "from-b" {
		t.Errorf("default propose = %+v err=%v", p, err)
	}
	// explicit selection.
	p, _ = reg.Propose(context.Background(), "a", ComposeRequest{Prompt: "x"})
	if p.Summary != "from-a" {
		t.Errorf("explicit select a = %+v", p)
	}
	// List marks the default.
	var sawDefault string
	for _, bi := range reg.List() {
		if bi.IsDefault {
			sawDefault = bi.Name
		}
	}
	if sawDefault != "b" {
		t.Errorf("List should mark b default, got %q", sawDefault)
	}
}

func TestRegistry_UnknownBackend(t *testing.T) {
	reg, _ := NewRegistry("a", []RegistryEntry{
		{Info: BackendInfo{Name: "a"}, Composer: &FakeComposer{}},
	})
	_, err := reg.Propose(context.Background(), "nope", ComposeRequest{Prompt: "x"})
	if !errors.Is(err, ErrUnknownBackend) {
		t.Errorf("expected ErrUnknownBackend, got %v", err)
	}
}

func TestRegistry_EmptyIsDisabled(t *testing.T) {
	reg, err := NewRegistry("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Enabled() {
		t.Errorf("empty registry must report Enabled()==false")
	}
}

func TestNewRegistry_ValidationErrors(t *testing.T) {
	// duplicate name
	if _, err := NewRegistry("a", []RegistryEntry{
		{Info: BackendInfo{Name: "a"}, Composer: &FakeComposer{}},
		{Info: BackendInfo{Name: "a"}, Composer: &FakeComposer{}},
	}); err == nil {
		t.Errorf("duplicate name should error")
	}
	// default not configured
	if _, err := NewRegistry("missing", []RegistryEntry{
		{Info: BackendInfo{Name: "a"}, Composer: &FakeComposer{}},
	}); err == nil {
		t.Errorf("unknown default should error")
	}
	// nil composer
	if _, err := NewRegistry("a", []RegistryEntry{{Info: BackendInfo{Name: "a"}}}); err == nil {
		t.Errorf("nil composer should error")
	}
}
