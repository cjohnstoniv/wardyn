// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistrelay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestToolServerListAndCall: tools/list and tools/call round-trip against the fake server, the call carrying
// the session id fixed at start and the harness's arguments.
func TestToolServerListAndCall(t *testing.T) {
	f := newFakeWardyn(t)
	session := uuid.NewString()
	f.setLive(session)
	c := startTools(f, session)
	c.send(1, "initialize", `{"protocolVersion":"2025-06-18","capabilities":{}}`)
	var init struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if !c.out.Scan() || json.Unmarshal(c.out.Bytes(), &init) != nil || init.Result.ServerInfo.Name != "wardyn" ||
		init.Result.ProtocolVersion != "2025-06-18" {
		t.Fatalf("initialize reply %s", c.out.Bytes())
	}
	_, _ = io.WriteString(c.in, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	c.send(2, "tools/list", `{}`)
	var list struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if !c.out.Scan() || json.Unmarshal(c.out.Bytes(), &list) != nil || len(list.Result.Tools) != 1 ||
		list.Result.Tools[0].Name != "read" || list.Result.Tools[0].Description != "Read a record." ||
		!strings.Contains(string(list.Result.Tools[0].InputSchema), `"what"`) {
		t.Fatalf("tools/list reply %s", c.out.Bytes())
	}
	c.send(3, "tools/call", `{"name":"read","arguments":{"what":"unit"}}`)
	isErr, text := c.reply(t)
	if isErr || strings.TrimSpace(text) != `{"answer":"a record"}` {
		t.Fatalf("tools/call reply isError=%v %q; want the server's JSON as text", isErr, text)
	}
	_, calls := f.snapshot()
	if len(calls) != 1 || calls[0].Name != "read" || calls[0].Session != session || string(calls[0].Args) != `{"what":"unit"}` {
		t.Fatalf("calls %+v", calls)
	}
	_ = c.in.Close()
}

func TestToolServerRefusalIsAnErrorResult(t *testing.T) {
	f := newFakeWardyn(t)
	f.setLive(uuid.NewString())
	c := startTools(f, uuid.NewString())
	c.send(1, "tools/call", `{"name":"read","arguments":{}}`)
	if isErr, text := c.reply(t); !isErr || text != "That session is not the live one." {
		t.Fatalf("reply isError=%v %q; want the server's message as an error", isErr, text)
	}
	c.send(2, "tools/call", `{"name":"Read","arguments":{}}`)
	if isErr, text := c.reply(t); !isErr || text != "unknown tool" {
		t.Fatalf("a badly named tool: isError=%v %q", isErr, text)
	}
	if _, calls := f.snapshot(); len(calls) != 1 {
		t.Fatalf("calls %+v; the badly named one must not reach the server", calls)
	}
	_ = c.in.Close()
}

// TestToolServerExitsWithoutTools: on any answer but a usable tool list, Serve fails before reading a request,
// so the harness is never offered an empty list.
func TestToolServerExitsWithoutTools(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{404, `{}`}, {500, `{}`}, {200, `[]`}, {200, `[{"name":"Bad-Name"}]`}} {
		f := newFakeWardyn(t)
		f.toolsStatus, f.toolsBody = tc.status, tc.body
		var out, log bytes.Buffer
		ts := &ToolServer{API: f.api(), Session: uuid.NewString(), Log: &log}
		in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
		if err := ts.Serve(context.Background(), in, &out); err == nil {
			t.Fatalf("%d %s: Serve succeeded", tc.status, tc.body)
		}
		if out.Len() != 0 || !strings.Contains(log.String(), "unavailable") {
			t.Fatalf("%d %s: out %q log %q", tc.status, tc.body, out.String(), log.String())
		}
	}
}

func TestToolServerUnknownMethod(t *testing.T) {
	f := newFakeWardyn(t)
	var out bytes.Buffer
	ts := &ToolServer{API: f.api(), Session: uuid.NewString(), Log: io.Discard}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":"a","method":"resources/list"}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n")
	if err := ts.Serve(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(&out)
	var replies []map[string]json.RawMessage
	for sc.Scan() {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(sc.Bytes(), &m)
		replies = append(replies, m)
	}
	if len(replies) != 2 || !strings.Contains(string(replies[0]["error"]), "-32601") || string(replies[0]["id"]) != `"a"` ||
		string(replies[1]["result"]) != "{}" {
		t.Fatalf("replies %s", out.String())
	}
}
