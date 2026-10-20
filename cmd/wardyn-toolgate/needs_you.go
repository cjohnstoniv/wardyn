// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// "Needs you" (#2963): `wardyn needs-you <reason>` (the image's /usr/local/bin/wardyn shim runs
// `wardyn-toolgate needs-you`) asks a person to take over this run's desktop or terminal, then blocks
// until they hand back. The request rides the same brokered route as a tool approval (the proxy
// injects the run identity and keeps only the clamped reason). It grants the run nothing: the person
// drives what the agent already has.
//
// Exit status: 0 the person handed control back; 3 they declined; 4 nobody took over in time (the
// command withdraws its own request) or the request was closed; 2 usage; 1 any other failure.
const (
	exitHandedBack = 0
	exitFailed     = 1
	exitUsage      = 2
	exitDeclined   = 3
	exitNobody     = 4
)

// defaultTakeoverWait is how long a request waits for someone to take over (the approved default).
const defaultTakeoverWait = 30 * time.Minute

// needsYou is the needs-you subcommand; it returns the exit status.
func needsYou(args []string, errOut io.Writer) int {
	fs := flag.NewFlagSet("wardyn needs-you", flag.ContinueOnError)
	fs.SetOutput(errOut)
	base := fs.String("base", os.Getenv("HTTP_PROXY"), "brokered API base URL (default: $HTTP_PROXY — the egress proxy)")
	poll := fs.Duration("poll", 2*time.Second, "poll interval")
	wait := fs.Duration("wait", defaultTakeoverWait, "give up when nobody has taken over after this long")
	fs.Usage = func() {
		fmt.Fprintln(errOut, "usage: wardyn needs-you <reason>")
		fmt.Fprintln(errOut, "Asks a person to take over this run's desktop or terminal, and waits until they hand back.")
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	reason := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if reason == "" {
		fs.Usage()
		return exitUsage
	}
	if *base == "" {
		fmt.Fprintln(errOut, "wardyn needs-you: no -base and no HTTP_PROXY in the environment")
		return exitFailed
	}
	t := &takeoverWait{base: strings.TrimRight(*base, "/"), client: directClient(), poll: *poll, wait: *wait,
		now: time.Now, sleep: time.Sleep, out: errOut}
	return t.run(reason)
}

// takeoverWait raises one takeover request and follows it to its end.
type takeoverWait struct {
	base   string
	client *http.Client
	poll   time.Duration
	wait   time.Duration
	now    func() time.Time
	sleep  func(time.Duration)
	out    io.Writer
}

// takeoverRow is what the command reads of its request.
type takeoverRow struct {
	ID           string     `json:"id"`
	State        string     `json:"state"`
	HandedBackAt *time.Time `json:"handed_back_at"`
}

func (t *takeoverWait) run(reason string) int {
	body, _ := json.Marshal(map[string]any{"kind": "human_takeover", "payload": map[string]any{"reason": reason}})
	row, err := t.call(http.MethodPost, "/wardyn/v1/approvals", body)
	if err != nil || row.ID == "" {
		fmt.Fprintf(t.out, "wardyn needs-you: the request was not raised: %v\n", err)
		return exitFailed
	}
	fmt.Fprintf(t.out, "Wardyn: asked a person to take over. Waiting up to %s for someone to take over.\n", t.wait)
	giveUp := t.now().Add(t.wait)
	told := false
	for {
		switch {
		case row.State == "APPROVED" && row.HandedBackAt != nil:
			fmt.Fprintln(t.out, "Wardyn: control was handed back. Carry on.")
			return exitHandedBack
		case row.State == "APPROVED":
			if !told {
				fmt.Fprintln(t.out, "Wardyn: a person has control. Waiting for them to hand back.")
				told = true
			}
		case row.State == "DENIED":
			fmt.Fprintln(t.out, "Wardyn: the person declined. Carry on without them.")
			return exitDeclined
		case row.State == "EXPIRED" || row.State == "CANCELLED":
			fmt.Fprintln(t.out, "Wardyn: nobody took over. Carry on without a person.")
			return exitNobody
		case t.now().After(giveUp):
			// Withdraw the request so nobody takes over a session the agent has stopped waiting on.
			// A take over that won the race is honoured: the next read sees APPROVED.
			if _, err := t.call(http.MethodPost, "/wardyn/v1/approvals/"+row.ID+"/expire", nil); err != nil {
				fmt.Fprintf(t.out, "wardyn needs-you: withdraw request %s: %v\n", row.ID, err)
			}
		}
		t.sleep(t.poll)
		next, err := t.call(http.MethodGet, "/wardyn/v1/approvals/"+row.ID, nil)
		if err != nil {
			fmt.Fprintf(t.out, "wardyn needs-you: read request %s: %v (retrying)\n", row.ID, err)
			continue
		}
		row = next
	}
}

// call sends one request to the brokered route and decodes the approval row it answers with.
func (t *takeoverWait) call(method, path string, body []byte) (takeoverRow, error) {
	req, err := http.NewRequest(method, t.base+path, bytes.NewReader(body))
	if err != nil {
		return takeoverRow{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return takeoverRow{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return takeoverRow{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var row takeoverRow
	if err := json.Unmarshal(b, &row); err != nil {
		return takeoverRow{}, fmt.Errorf("unreadable response")
	}
	return row, nil
}
