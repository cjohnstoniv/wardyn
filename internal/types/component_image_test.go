// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types_test

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestComponentDefinitionValidate_Image(t *testing.T) {
	digest := strings.Repeat("a", 64)
	accepted := []string{
		"",
		"nginx",
		"ghcr.io/acme/tools",
		"ghcr.io/acme/tools:1.2",
		"ghcr.io/acme/tools@sha256:" + digest,
	}
	for _, x := range accepted {
		d := types.ComponentDefinition{Hosts: []string{"svc.example"}, Image: x}
		if err := d.Validate(proxy.ValidDomainEntry, true); err != nil {
			t.Errorf("image %q: want accepted, got %v", x, err)
		}
	}
	refused := []string{
		" ghcr.io/acme/tools",
		"ghcr.io/acme/tools ",
		"Ghcr.io/Acme/Tools",
		"a b",
		"tools:",
		strings.Repeat("a", 256),
	}
	for _, x := range refused {
		d := types.ComponentDefinition{Hosts: []string{"svc.example"}, Image: x}
		if err := d.Validate(proxy.ValidDomainEntry, true); err == nil || !strings.HasPrefix(err.Error(), "image: ") {
			t.Errorf("image %q: want an error starting \"image: \", got %v", x, err)
		}
		c := types.Component{Name: "t", Definition: d}
		if err := c.Validate(proxy.ValidDomainEntry); err == nil || !strings.HasPrefix(err.Error(), "definition.image: ") {
			t.Errorf("component with image %q: want an error starting \"definition.image: \", got %v", x, err)
		}
	}
}
