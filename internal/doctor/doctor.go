// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package doctor holds the read-only staged checks behind `wardyn doctor` and `wardyn-runnerd doctor`: one
// line per stage, and a failing stage names the setting that fixes it. Names are shown, values are not.
package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Status is how one stage ended.
type Status string

// Stage statuses. A skip is a stage that did not apply or could not run because an earlier one failed; only a
// FAIL makes a report fail.
const (
	StatusOK   Status = "ok"
	StatusFail Status = "FAIL"
	StatusSkip Status = "skip"
)

// Stage is one check. Setting is the name (never the value) of the setting that fixes a failure, and Fix says
// what to set it to.
type Stage struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Detail  string `json:"detail"`
	Setting string `json:"setting,omitempty"`
	Fix     string `json:"fix,omitempty"`
}

// Report is a run's stages in order.
type Report struct {
	Stages []Stage `json:"stages"`
}

// OK is whether no stage failed.
func (r Report) OK() bool {
	for _, s := range r.Stages {
		if s.Status == StatusFail {
			return false
		}
	}
	return true
}

// WriteText prints one line per stage: the status, the name, the detail, and for a failure the fix.
func (r Report) WriteText(w io.Writer) error {
	for _, s := range r.Stages {
		line := fmt.Sprintf("%-4s  %-12s %s", s.Status, s.Name, s.Detail)
		if s.Fix != "" {
			line += "; " + s.Fix
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// WriteJSON prints the same stages as data.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		OK     bool    `json:"ok"`
		Stages []Stage `json:"stages"`
	}{r.OK(), r.Stages})
}

// userinfo matches the credentials of any URL in a text.
var userinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/\s@]*@`)

// Scrub removes every secret from s: the credentials of any URL in it, then each exact value given (a token,
// a proxy password). It is applied to every stage's text, so a library error that quotes a proxy URL cannot
// carry its credentials into either output.
func Scrub(s string, secrets ...string) string {
	s = userinfo.ReplaceAllString(s, "${1}***@")
	for _, secret := range secrets {
		if len(secret) >= 4 {
			s = strings.ReplaceAll(s, secret, "***")
		}
	}
	return s
}

// Scrubbed is the report with the credentials of any URL removed from every stage's text.
func (r Report) Scrubbed() Report {
	out := Report{Stages: make([]Stage, len(r.Stages))}
	for i, s := range r.Stages {
		out.Stages[i] = s.scrub(nil)
	}
	return out
}

func (s Stage) scrub(secrets []string) Stage {
	s.Detail, s.Fix = Scrub(s.Detail, secrets...), Scrub(s.Fix, secrets...)
	return s
}

// OKStage, SkipStage and FailStage build a stage. For a failure, setting is the name of the setting to change
// and fix says what to set it to ("" setting: the fix is free text).
func OKStage(name, detail string) Stage { return Stage{Name: name, Status: StatusOK, Detail: detail} }

func SkipStage(name, detail string) Stage {
	return Stage{Name: name, Status: StatusSkip, Detail: detail}
}

func FailStage(name, detail, setting, fix string) Stage {
	if setting != "" {
		fix = "set " + setting + " to " + fix
	}
	return Stage{Name: name, Status: StatusFail, Detail: detail, Setting: setting, Fix: fix}
}
