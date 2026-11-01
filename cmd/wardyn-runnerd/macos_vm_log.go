// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// prepareLogLineMax is the longest line of prepare.log a reader accepts: the helper's own lines are short,
// but a foreign line (AppKit, a tool's output) can be long, and a line over this stops the scan.
const prepareLogLineMax = 1 << 20

// lastHelperLines is the last progress event in the log at path, and its last line that is the helper's
// own (a progress event or a helperFailurePrefix error). Any other line is skipped: AppKit, while the
// setup-by-hand window is up, writes to the same stderr. The log is small: one job's lines, appended. err
// is why the scan stopped before the end of the log (a line over prepareLogLineMax, say): the lines after
// it are unknown, so last is not the log's last.
func lastHelperLines(path string) (ev macvm.PrepareEvent, haveEvent bool, last string, err error) {
	f, openErr := os.Open(path)
	if openErr != nil {
		return ev, false, "", nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, prepareLogLineMax)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		var e macvm.PrepareEvent
		switch {
		case strings.HasPrefix(l, helperFailurePrefix):
			last = l
		case strings.HasPrefix(l, "{") && json.Unmarshal([]byte(l), &e) == nil && e.Phase != "":
			ev, haveEvent, last = e, true, l
		}
	}
	return ev, haveEvent, last, sc.Err()
}

// lastPrepareToolsDial is the tools_dial of the last progress event in the log at path that carries one, and
// when the helper logged it: "" and the zero time when none does. An event without one, such as the phases
// after the tools phase, does not hide it.
func lastPrepareToolsDial(path string) (class string, at time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return "", time.Time{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, prepareLogLineMax)
	for sc.Scan() {
		var e struct {
			Time      time.Time `json:"time"`
			Phase     string    `json:"phase"`
			ToolsDial string    `json:"tools_dial"`
		}
		if l := strings.TrimSpace(sc.Text()); strings.HasPrefix(l, "{") && json.Unmarshal([]byte(l), &e) == nil && e.Phase != "" && e.ToolsDial != "" {
			class, at = e.ToolsDial, e.Time
		}
	}
	return class, at
}
