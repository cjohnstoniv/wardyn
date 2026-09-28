// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"math"
	"strings"
)

// minBrandContrast is WCAG AA for body text: every primary/text pair in
// effect, light or dark, must reach it.
const minBrandContrast = 4.5

// darkBackground is theme.css's .dark --background, the surface a derived
// dark primary has to stand out against.
const darkBackground = "#0a0a0a"

type rgb struct{ r, g, b float64 }

// parseHexColour accepts #rgb or #rrggbb (either case) and returns the
// colour and its canonical lower-case #rrggbb spelling.
func parseHexColour(s string) (rgb, string, bool) {
	h, ok := strings.CutPrefix(s, "#")
	if !ok || (len(h) != 3 && len(h) != 6) {
		return rgb{}, "", false
	}
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	var v [3]uint8
	if _, err := fmt.Sscanf(strings.ToLower(h), "%02x%02x%02x", &v[0], &v[1], &v[2]); err != nil {
		return rgb{}, "", false
	}
	c := rgb{float64(v[0]), float64(v[1]), float64(v[2])}
	return c, c.hex(), true
}

func (c rgb) hex() string {
	return fmt.Sprintf("#%02x%02x%02x", uint8(c.r), uint8(c.g), uint8(c.b))
}

// luminance is WCAG 2's relative luminance.
func (c rgb) luminance() float64 {
	ch := func(v float64) float64 {
		v /= 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(c.r) + 0.7152*ch(c.g) + 0.0722*ch(c.b)
}

// contrastRatio is WCAG 2's contrast ratio between two colours (1 to 21).
func contrastRatio(a, b rgb) float64 {
	la, lb := a.luminance()+0.05, b.luminance()+0.05
	return math.Max(la, lb) / math.Min(la, lb)
}

// ratioText renders a ratio the way the Branding card does: rounded DOWN to one
// decimal, so a failing 4.46 never reads as the passing "4.5".
func ratioText(r float64) string {
	return fmt.Sprintf("%.1f", math.Floor(r*10)/10)
}

// deriveDarkPrimary is the dark-mode pair used when the admin sets none: the
// light primary mixed toward white in 5% steps until it reaches 4.5:1 against
// the dark background, with the dark background as its text. That pair passes
// by construction (the ratio is symmetric), and pure white ends the walk at
// 19.8:1. ui/src/app/lib/branding.ts's deriveDarkPrimary is the same walk;
// both are pinned to the same vectors.
func deriveDarkPrimary(light rgb) (primary, text string) {
	bg, _, _ := parseHexColour(darkBackground)
	for i := 0; i <= 20; i++ {
		t := float64(i) / 20
		c := rgb{
			math.Round(light.r + (255-light.r)*t),
			math.Round(light.g + (255-light.g)*t),
			math.Round(light.b + (255-light.b)*t),
		}
		if contrastRatio(c, bg) >= minBrandContrast {
			return c.hex(), darkBackground
		}
	}
	return "#ffffff", darkBackground
}
