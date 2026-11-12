// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"fmt"
	"path/filepath"
)

// PrintUntrusted is printUntrusted: edit-base's list of what the base does not trust yet.
func (o *EditOptions) PrintUntrusted(base string) { o.printUntrusted(base) }

// SignWithCodesign is the codesign fallback alone, on this host, with this Mac's checks of the copy: the path of
// the signed copy in a temp directory, which the caller removes.
func (s *HostAgentSigner) SignWithCodesign(ctx context.Context, path, tmp string) (string, error) {
	cert, err := AgentSigningIdentity(ctx, s.Dir)
	if err != nil {
		return "", err
	}
	run := func(argv ...string) (string, error) {
		out, err := hostExec(ctx, argv)
		if err != nil {
			return "", fmt.Errorf("%s %s: %w: %s", argv[0], argv[1], err, outputTail(string(out)))
		}
		return string(out), nil
	}
	cp := filepath.Join(tmp, "wardyn-guest-agent")
	if err := s.signCodesign(ctx, hostExec, run, cert, path, cp, tmp); err != nil {
		return "", err
	}
	return cp, proveSigned(run, cp, cert)
}
