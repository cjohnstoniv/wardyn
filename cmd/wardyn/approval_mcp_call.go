// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// printMCPCall adds the tool, the server and the call's arguments to `approval get` for an MCP call row.
// The arguments print as the server's text, or for an older server as the same text made from the raw
// scope: either way no number goes through a float.
func printMCPCall(w io.Writer, a types.ApprovalRequest) {
	if printKubeCall(w, a) { // a held kubectl write (approval_kube_call.go)
		return
	}
	text := a.CallText
	if text == nil {
		text = types.MCPCallTextOf(a.RequestedScope)
	}
	if text == nil {
		return
	}
	var sc types.MCPCallScope
	_ = json.Unmarshal(a.RequestedScope, &sc)
	server := sc.MCPServer
	if sc.MCPHost != "" {
		server += " (" + sc.MCPHost + ")"
	}
	// The call key binds the raw method the server knows, which the display name can hide: delete.issue and
	// delete_issue both read mcp__jira__delete_issue. The raw name has its own line.
	raw := sc.MCPTool
	if raw == "" {
		raw = strings.TrimPrefix(sc.Tool, "mcp__"+sc.MCPServer+"__")
	}
	// The names come from the remote server and the arguments from the agent: print the names quoted and
	// escaped, and the JSON with its format and control runes as \u escapes (the same string), so nothing in
	// them reaches the terminal as a control, a line break or a reordering mark.
	fmt.Fprintf(w, "TOOL:      %s\nSERVER TOOL: %s\nSERVER:    %s\nARGUMENTS:\n%s\n",
		strconv.QuoteToASCII(sc.Tool), strconv.QuoteToASCII(raw), strconv.QuoteToASCII(server), jsonForTerminal(text.Arguments))
	if text.Extra != "" {
		fmt.Fprintf(w, "EXTRA:\n%s\n", jsonForTerminal(text.Extra))
	}
}

// jsonForTerminal escapes every control and format rune in indented JSON text as \uXXXX, keeping its newlines.
// Such a rune can only sit inside a JSON string, where the escape reads as the same character.
func jsonForTerminal(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError:
			if r > 0xFFFF {
				fmt.Fprintf(&b, `\u%04x\u%04x`, 0xD800+((r-0x10000)>>10), 0xDC00+((r-0x10000)&0x3FF))
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
