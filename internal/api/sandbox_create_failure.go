// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// sandboxCreateFailure is the FAILED hint and the audit reason for a CreateSandbox error. A runner's
// image_arch refusal is a message written for the person, so it is shown without the wire prefix
// and with where to fix it; everything else keeps the generic sentence and reason.
func sandboxCreateFailure(err error) (hint, reason string) {
	if !errors.Is(err, runner.ErrImageArch) {
		return "the sandbox could not be created: " + err.Error(), "sandbox_create"
	}
	msg := err.Error()
	var we *runnerwire.Error
	if errors.As(err, &we) {
		msg = we.Message
	}
	return "the sandbox could not be created: " + msg +
		` If this image comes from WARDYN_AGENT_IMAGES, an administrator can name an image for this runner's architecture with a "<key>@<arch>" entry.`, "image_arch"
}
