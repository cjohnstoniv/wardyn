// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var secretNameRe = regexp.MustCompile(`(?i)(` + secretMarkers + `)`)

// A secret-named flag (`--password`, `-token`) whose value is the next argv entry, as in a
// Compose `command: [helper, --password, value]`: no ':' or '=' joins them for secretPairRe.
// Flags also take KEY and PASS, since they spell `--api-key` and `--requirepass` where an
// environment name has `_KEY`; secretMarkers stays the key-name vocabulary.
var secretFlagRe = regexp.MustCompile(`(?i)^-{1,2}[A-Za-z0-9_.-]*(?:` + secretMarkers + `|KEY|PASS)[A-Za-z0-9_.-]*$`)

// The same flag with its value after whitespace or '=', as one shell string or argv entry
// (`helper --password value`, `--api-key=value`). The flag may also follow '=', a quote or '('
// (`EXTRA_ARGS=--token value`).
var secretFlagArgRe = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_.-])-{1,2}[A-Za-z0-9_.-]*(?:` + secretMarkers + `|KEY|PASS)[A-Za-z0-9_.-]*(?:\s+|=)\S`)

func redactSecrets(raw []byte) []byte {
	const omitted = "# Compose config omitted: cannot safely redact its YAML.\n"
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc, extra yaml.Node
	if err := dec.Decode(&doc); err == io.EOF {
		if strings.TrimSpace(string(raw)) == "" {
			return raw
		}
		return []byte("# Comments omitted from redacted Compose config.\n")
	} else if err != nil {
		return []byte(omitted)
	}
	if dec.Decode(&extra) != io.EOF {
		return []byte(omitted)
	}
	if !redactYAMLNode(&doc) {
		return raw
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return []byte(omitted)
	}
	return out
}

func redactYAMLNode(n *yaml.Node) bool {
	// Raw-file fallbacks can contain commented-out multiline credentials.
	changed := n.HeadComment != "" || n.LineComment != "" || n.FootComment != ""
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	if n.Kind == yaml.ScalarNode {
		key, _, ok := strings.Cut(n.Value, "=")
		if ok && !strings.ContainsAny(key, " \t\r\n") && secretNameRe.MatchString(key) {
			n.Value, n.Style = key+"=<redacted>", yaml.DoubleQuotedStyle
			return true
		}
		if secretPairRe.MatchString(n.Value) || dsnCredsRe.MatchString(n.Value) || secretFlagArgRe.MatchString(n.Value) {
			redactYAMLValue(n, make(map[*yaml.Node]bool))
			return true
		}
	}
	if n.Kind == yaml.SequenceNode {
		// Back to front: an entry is tested as a flag before an earlier flag redacts it (`--auth --token value`).
		for i := len(n.Content) - 2; i >= 0; i-- {
			flag := n.Content[i]
			if flag.Kind == yaml.AliasNode {
				flag = flag.Alias
			}
			if flag.Kind == yaml.ScalarNode && secretFlagRe.MatchString(flag.Value) {
				redactYAMLValue(n.Content[i+1], make(map[*yaml.Node]bool))
				changed = true
			}
		}
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind == yaml.AliasNode {
				key = key.Alias
			}
			value := n.Content[i+1]
			if secretNameRe.MatchString(key.Value) {
				redactYAMLValue(value, make(map[*yaml.Node]bool))
				changed = true
			}
		}
	}
	for _, child := range n.Content {
		if redactYAMLNode(child) {
			changed = true
		}
	}
	return changed
}

func redactYAMLValue(n *yaml.Node, seen map[*yaml.Node]bool) {
	if seen[n] {
		return
	}
	seen[n] = true
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	switch n.Kind {
	case yaml.AliasNode:
		// The anchor is another spelling of the same secret, not safe diagnostics.
		redactYAMLValue(n.Alias, seen)
	case yaml.ScalarNode:
		n.Value, n.Tag, n.Style = "<redacted>", "!!str", yaml.DoubleQuotedStyle
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			redactYAMLValue(n.Content[i], seen)
		}
	default:
		for _, child := range n.Content {
			redactYAMLValue(child, seen)
		}
	}
}
