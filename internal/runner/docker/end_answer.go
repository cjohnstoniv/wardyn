// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// endAnswer is a lease end's answer once what it leaves behind is known: left is a part of the end that did
// not finish (a VM sidecar that would not go, a macOS VM helper still running), which a retry has to clear.
// A wipe warning (runner.ErrAWSCacheWipeFailed) tells a caller the end succeeded, so beside left it is kept
// as words only, and the caller retries the end instead of recording it as done.
func endAnswer(endErr, left error) error {
	if left == nil {
		return endErr
	}
	if errors.Is(endErr, runner.ErrAWSCacheWipeFailed) {
		endErr = errors.New(endErr.Error())
	}
	return errors.Join(endErr, left)
}
