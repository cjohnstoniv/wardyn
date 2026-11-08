// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"slices"
	"strconv"
	"strings"
)

// maxAuditedTools bounds how many tool names one allow row lists.
const maxAuditedTools = 8

// mcpToolCause names the tools of the tools/call messages in body for the
// allow row's cause, in first-seen order: `tool "a"` or `tools "a", "b"`. It
// never carries arguments. A body with no tools/call, or one that does not
// parse, gives "".
func mcpToolCause(body []byte) string {
	msgs, _, err := parseMCPMessages(body)
	if err != nil {
		return ""
	}
	var names []string
	for _, m := range msgs {
		if m.tool != "" && !slices.Contains(names, m.tool) {
			names = append(names, m.tool)
		}
	}
	if len(names) == 0 {
		return ""
	}
	more := len(names) - min(len(names), maxAuditedTools)
	quoted := make([]string, 0, maxAuditedTools)
	for _, n := range names[:len(names)-more] {
		quoted = append(quoted, strconv.Quote(n))
	}
	out := strings.Join(quoted, ", ")
	if more > 0 {
		out += " and " + strconv.Itoa(more) + " more"
	}
	if len(names) == 1 {
		return "tool " + out
	}
	return "tools " + out
}
