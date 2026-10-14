// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Whether a CLI is on an image's PATH: yes, no, or unknown for an image Wardyn
// did not build.
const (
	cliInImageYes     = "yes"
	cliInImageNo      = "no"
	cliInImageUnknown = "unknown"
)

// cliImageFact says, for one image, whether each CLI card's tool is installed.
type cliImageFact struct{ AZ, AWS, GH string }

// cliConventionImages covers Wardyn's convention image keys. A column turns to
// yes in the same change that installs that tool into the image, so the table
// never promises a tool an image lacks.
var cliConventionImages = map[string]cliImageFact{
	"base":        {cliInImageNo, cliInImageYes, cliInImageYes},
	"claude-code": {cliInImageNo, cliInImageYes, cliInImageYes},
	"codex-cli":   {cliInImageNo, cliInImageYes, cliInImageYes},
	"full":        {cliInImageYes, cliInImageYes, cliInImageYes},
	"harnesses":   {cliInImageYes, cliInImageNo, cliInImageNo},
}

// cliInImage is the install fact for an image key. Anything not in the table,
// an operator's own image included, is unknown: nothing inspects its contents.
func cliInImage(imageKey string) cliImageFact {
	if f, ok := cliConventionImages[imageKey]; ok {
		return f
	}
	return cliImageFact{cliInImageUnknown, cliInImageUnknown, cliInImageUnknown}
}
