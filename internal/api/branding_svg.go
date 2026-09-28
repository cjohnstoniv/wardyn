// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// An uploaded SVG logo is REBUILT, never stored as sent. Rasterising would be
// the other safe answer, but the standard library has no SVG renderer and a
// third-party one is a parser of its own to trust. Instead the upload is
// parsed with encoding/xml and re-emitted from an allowlist of shape, text and
// gradient elements and presentation attributes: a byte the parser did not
// understand never reaches a browser, so a parser differential (DOCTYPE
// entities, CDATA tricks, odd namespaces) has nothing to smuggle. Anything
// that can run script or fetch — <script>, <foreignObject>, <image>, <a>,
// <style>, animation, on* handlers, an href or url() that is not a same-
// document #fragment, a DOCTYPE — is REFUSED with a named reason rather than
// dropped, so the admin learns why; editor metadata (unknown elements and
// namespaced attributes) is dropped. The console CSP (script-src 'self', no
// inline script) is the second wall for anyone who opens the logo URL itself.

const (
	svgNS   = "http://www.w3.org/2000/svg"
	xlinkNS = "http://www.w3.org/1999/xlink"
)

var svgElements = setOf("svg", "g", "defs", "title", "desc", "path", "rect", "circle", "ellipse",
	"line", "polyline", "polygon", "text", "tspan", "linearGradient", "radialGradient", "stop",
	"clipPath", "mask", "symbol", "use")

var svgRefusedElements = setOf("script", "foreignObject", "image", "a", "style", "iframe", "embed",
	"object", "animate", "animateMotion", "animateTransform", "set", "feImage", "audio", "video",
	"handler", "listener", "discard")

// svgPresentation is every attribute that may also appear as a style
// declaration.
var svgPresentation = setOf("fill", "fill-opacity", "fill-rule", "stroke", "stroke-width",
	"stroke-linecap", "stroke-linejoin", "stroke-miterlimit", "stroke-dasharray", "stroke-dashoffset",
	"stroke-opacity", "opacity", "stop-color", "stop-opacity", "clip-rule", "clip-path", "mask",
	"font-family", "font-size", "font-weight", "font-style", "text-anchor", "dominant-baseline",
	"letter-spacing", "visibility", "display", "color")

var svgGeometry = setOf("id", "class", "viewBox", "width", "height", "x", "y", "x1", "y1", "x2",
	"y2", "cx", "cy", "r", "rx", "ry", "fx", "fy", "d", "points", "transform", "gradientUnits",
	"gradientTransform", "spreadMethod", "offset", "clipPathUnits", "maskUnits", "maskContentUnits",
	"preserveAspectRatio", "version", "dx", "dy", "style")

var svgTextElements = setOf("title", "desc", "text", "tspan")

var (
	svgFragmentRef = regexp.MustCompile(`^#[A-Za-z_][A-Za-z0-9_.:-]*$`)
	svgURLRef      = regexp.MustCompile(`(?i)url\(\s*['"]?\s*([^)'"\s]*)`)
	svgStyleValue  = regexp.MustCompile(`^[#%(),.\w\s-]*$`)
	svgFunction    = regexp.MustCompile(`([A-Za-z-]+)\s*\(`)
)

// svgFunctions are the only CSS functions a value may call: colours, the
// transform list, and url() (checked separately to be a #fragment). Anything
// else — src(), image-set(), cross-fade() — could name a resource.
var svgFunctions = setOf("url", "rgb", "rgba", "hsl", "hsla", "translate", "scale", "rotate", "matrix", "skewx", "skewy")

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// errSVG is a refusal the Branding card shows as it stands.
func errSVG(format string, a ...any) error {
	return fmt.Errorf("logo: this SVG "+format, a...)
}

// sanitizeSVG rebuilds an uploaded SVG from the allowlist above, or refuses it.
func sanitizeSVG(in []byte) ([]byte, error) {
	dec := xml.NewDecoder(bytes.NewReader(in))
	dec.Strict = true
	w := svgWriter{}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errSVG("is not well-formed XML")
		}
		if err := w.token(tok); err != nil {
			return nil, err
		}
	}
	if !w.rooted || len(w.stack) > 0 {
		return nil, errSVG("has no complete <svg> root element")
	}
	return w.out.Bytes(), nil
}

type svgWriter struct {
	out    bytes.Buffer
	stack  []string // elements emitted and still open
	skip   int      // depth inside a dropped element
	rooted bool
}

func (w *svgWriter) token(tok xml.Token) error {
	switch t := tok.(type) {
	case xml.StartElement:
		return w.start(t)
	case xml.EndElement:
		if w.skip > 0 {
			w.skip--
			return nil
		}
		w.out.WriteString("</" + w.stack[len(w.stack)-1] + ">")
		w.stack = w.stack[:len(w.stack)-1]
	case xml.CharData:
		if w.skip == 0 && len(w.stack) > 0 && svgTextElements[w.stack[len(w.stack)-1]] {
			_ = xml.EscapeText(&w.out, t)
		}
	case xml.ProcInst:
		if t.Target != "xml" {
			return errSVG("carries a processing instruction (<?%s?>)", t.Target)
		}
	case xml.Directive:
		return errSVG("carries a DOCTYPE or entity declaration")
	}
	return nil // comments are dropped
}

func (w *svgWriter) start(t xml.StartElement) error {
	name := t.Name.Local
	if svgRefusedElements[name] {
		return errSVG("contains <%s>, which can run script or load content", name)
	}
	if w.skip > 0 {
		w.skip++
		return nil
	}
	if !w.rooted {
		if name != "svg" || t.Name.Space != svgNS {
			return errSVG("does not start with an <svg> element in the SVG namespace")
		}
		w.rooted = true
	} else if len(w.stack) == 0 {
		return errSVG("has more than one root element")
	}
	if t.Name.Space != svgNS || !svgElements[name] {
		w.skip = 1
		return nil
	}
	attrs, err := svgAttrs(name, t.Attr)
	if err != nil {
		return err
	}
	w.out.WriteString("<" + name)
	if len(w.stack) == 0 {
		w.out.WriteString(` xmlns="` + svgNS + `"`)
	}
	w.out.WriteString(attrs + ">")
	w.stack = append(w.stack, name)
	return nil
}

// svgAttrs returns the element's allowed attributes, serialised.
func svgAttrs(elem string, in []xml.Attr) (string, error) {
	var b bytes.Buffer
	for _, a := range in {
		name, space := a.Name.Local, a.Name.Space
		if strings.HasPrefix(strings.ToLower(name), "on") {
			return "", errSVG("has an event handler attribute (%s on <%s>)", name, elem)
		}
		if name == "href" && (space == "" || space == xlinkNS || space == "xlink") {
			if !svgFragmentRef.MatchString(strings.TrimSpace(a.Value)) {
				return "", errSVG("links outside itself (href on <%s>)", elem)
			}
			writeSVGAttr(&b, "href", strings.TrimSpace(a.Value))
			continue
		}
		if space != "" || (!svgGeometry[name] && !svgPresentation[name]) {
			continue // xmlns declarations and editor metadata
		}
		if err := svgValueOK(elem, name, a.Value); err != nil {
			return "", err
		}
		writeSVGAttr(&b, name, a.Value)
	}
	return b.String(), nil
}

func svgValueOK(elem, name, value string) error {
	// A CSS escape spells url( without the letters (u\72l), in a presentation
	// attribute as much as in style.
	if strings.ContainsRune(value, '\\') {
		return errSVG("has an escaped value Wardyn can't check (%s on <%s>)", name, elem)
	}
	for _, m := range svgFunction.FindAllStringSubmatch(value, -1) {
		if !svgFunctions[strings.ToLower(m[1])] {
			return errSVG("calls %s() in %s on <%s>", m[1], name, elem)
		}
	}
	for _, m := range svgURLRef.FindAllStringSubmatch(value, -1) {
		if !svgFragmentRef.MatchString(m[1]) {
			return errSVG("refers to something outside itself (url() in %s on <%s>)", name, elem)
		}
	}
	if name != "style" {
		return nil
	}
	for _, decl := range strings.Split(value, ";") {
		if strings.TrimSpace(decl) == "" {
			continue
		}
		prop, val, ok := strings.Cut(decl, ":")
		if !ok || !svgPresentation[strings.ToLower(strings.TrimSpace(prop))] || !svgStyleValue.MatchString(val) {
			return errSVG("has a style attribute Wardyn can't check (on <%s>) — export it with presentation attributes", elem)
		}
	}
	return nil
}

func writeSVGAttr(b *bytes.Buffer, name, value string) {
	b.WriteString(" " + name + `="`)
	_ = xml.EscapeText(b, []byte(value))
	b.WriteString(`"`)
}
