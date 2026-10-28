// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PrewrappedImageChecker is a prewrapped image's registration check, run by the control plane's own
// Kubernetes substrate: one short pod per architecture from the pinned image, behind a deny-all
// NetworkPolicy, that looks for the image contract's tools and runs `wardyn-rec -contract`. It lives in this
// tagless package because the control plane cannot see a `k8s`-tagged symbol.
type PrewrappedImageChecker interface {
	// PrewrappedCheckUnavailable says why this substrate cannot run the check, or "" when it can.
	PrewrappedCheckUnavailable() string
	// CheckPrewrappedImage runs attempt's check pods for ref and returns one verdict per candidate
	// architecture. Every pod and policy of the attempt is torn down before it returns, or left to the
	// orphan sweep when that fails.
	CheckPrewrappedImage(ctx context.Context, attempt uuid.UUID, ref string) []ImageArchVerdict
}

// PrewrappedCandidateArches are the architectures a prewrapped image is checked on.
var PrewrappedCandidateArches = []string{"amd64", "arm64"}

// ImageArchVerdict is one architecture's check outcome. Detail is bounded and never carries a credential.
type ImageArchVerdict struct {
	Arch   string
	Code   string
	Detail string
}

// One architecture's check outcome. Anything not ImageCheckPassed is not a pass.
const (
	ImageCheckPassed          = "passed"
	ImageCheckPlatformMissing = "platform_missing" // the image has no build for this architecture
	ImageCheckNoNode          = "no_node"          // no node of this architecture to check on
	ImageCheckUnpullable      = "unpullable"       // the cluster could not pull it; Detail says why
	ImageCheckContractMissing = "contract_missing" // Detail names the missing tool
	ImageCheckContractOld     = "contract_old"     // wardyn-rec predates -contract: built FROM an older agent-base
	ImageCheckTimedOut        = "timed_out"
	ImageCheckFailed          = "failed" // any other outcome; Detail says what was seen
)

// ImageCheckScript is the check pod's whole command (`sh -c`). Exit 3 names a missing tool, exit 4 is a
// wardyn-rec without -contract or with another line; only exit 0 after the exact contract line passes.
var ImageCheckScript = func() string {
	var b strings.Builder
	for _, t := range types.ImageContractTools {
		test := "-x"
		if strings.HasSuffix(t, ".sh") {
			test = "-r" // sourced by agent-run, not executed
		}
		b.WriteString(`[ ` + test + ` /usr/local/bin/` + t + ` ] || { echo "missing ` + t + `"; exit 3; }; `)
	}
	b.WriteString(`out=$(/usr/local/bin/wardyn-rec -contract 2>&1) || { echo "wardyn-rec -contract failed"; exit 4; }; `)
	b.WriteString(`[ "$out" = "` + types.ImageContractLine + `" ] || { echo "wardyn-rec -contract printed another line"; exit 4; }; `)
	b.WriteString(`echo "` + types.ImageContractLine + `"`)
	return b.String()
}()

// AggregateImageCheck turns an attempt's per-architecture verdicts into the row's one verdict. It is ready
// when at least one architecture passed and every other one only lacks a build or a node; the accepted
// architectures are those that passed, and reason then names an architecture left out and why. Anything
// else is refused, with reason naming the first architecture whose outcome refuses it. reason is
// "<code>:<arch>[:<detail>]".
func AggregateImageCheck(vs []ImageArchVerdict) (status, reason string, accepted []string) {
	var leftOut, refusal string
	for _, v := range vs {
		r := v.Code + ":" + v.Arch
		if v.Detail != "" {
			r += ":" + v.Detail
		}
		switch v.Code {
		case ImageCheckPassed:
			accepted = append(accepted, v.Arch)
		case ImageCheckPlatformMissing, ImageCheckNoNode:
			if leftOut == "" {
				leftOut = r
			}
		default:
			if refusal == "" {
				refusal = r
			}
		}
	}
	slices.Sort(accepted)
	switch {
	case refusal != "":
		return types.BaseImageCheckRefused, refusal, nil
	case len(accepted) == 0 && leftOut != "":
		return types.BaseImageCheckRefused, leftOut, nil
	case len(accepted) == 0:
		return types.BaseImageCheckRefused, ImageCheckFailed + "::no architecture was checked", nil
	}
	return types.BaseImageCheckReady, leftOut, accepted
}
