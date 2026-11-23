// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"strings"

	"github.com/distribution/reference"
)

// MaxComponentImageBytes bounds a component's image requirement.
const MaxComponentImageBytes = 255

// validateImage checks the definition's image requirement (#1972): empty, or
// one image reference as a run's image field takes it. Which images meet it is
// the run gate's question, not this one's.
func (d ComponentDefinition) validateImage() error {
	if d.Image == "" {
		return nil
	}
	if len(d.Image) > MaxComponentImageBytes || strings.TrimSpace(d.Image) != d.Image {
		return fmt.Errorf("image: give one image reference of at most %d bytes, with no spaces", MaxComponentImageBytes)
	}
	if _, err := reference.ParseNormalizedNamed(d.Image); err != nil {
		return fmt.Errorf("image: %q is not an image reference", d.Image)
	}
	return nil
}
