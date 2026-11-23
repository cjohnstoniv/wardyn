// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistant

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The caps on what a first message carries. The whole payload stays under the scanner's default span of
// 1 MiB, so no span is skipped open.
const (
	MaxSectionBytes = 256 << 10
	MaxPayloadBytes = 768 << 10
)

// The section labels, in the order the records appear, in the words the preview draws.
const (
	LabelUnit      = "your unit record"                                   // PROPOSED: the preview's words
	LabelRun       = "the run's policy record"                            // PROPOSED
	LabelTemplates = "template records"                                   // PROPOSED: the preview's words
	LabelAdmins    = "unit admins"                                        // PROPOSED: the preview's words
	LabelGrants    = "credential grants"                                  // PROPOSED: the preview's words
	LabelHistory   = "run history, last 30 days (host · decision · runs)" // PROPOSED: the preview's words
	LabelChange    = "the change under review"                            // PROPOSED: the settings card's words
	LabelBuild     = "build files"                                        // PROPOSED: the settings card's words
)

const truncatedMark = "\n(cut: too long to send)" // PROPOSED

// BuildFile is one confirmed build file of a repository draft.
type BuildFile struct {
	Name string
	Text string
}

// HostCount is how many runs of the last 30 days met one decision at one host: Decision is "allow" or "deny".
type HostCount struct {
	Host     string
	Decision string
	Count    int
}

// Inputs are the records the server found, already cut to what the person may read.
type Inputs struct {
	Purpose types.AssistantPurpose
	// UnitRecord is the person's unit record, cut at the security tier at most, mount sources removed.
	UnitRecord string
	// RunRecord is a run target's policy, cut the same way. A run's free-text task is never sent.
	RunRecord string
	// Templates are one record each: the target template, or for a new draft names and summaries. Untrusted:
	// a template's name and description are text people wrote, other people's included.
	Templates []string
	// UnitAdmins, ChangeProposer and RunOwners are the people the records name, in the order their letters
	// are handed out.
	UnitAdmins     []Person
	ChangeProposer *Person
	RunOwners      []Person
	// ChangePeople are the others a change under review names: whom it assigns, and every address in it.
	ChangePeople []Person
	// Grants are the credential grants by kind and host.
	Grants []string
	// RunHistory is included only when RunHistoryOn.
	RunHistoryOn bool
	RunHistory   []HostCount
	// Change is the change under review, for a review session. Untrusted.
	Change string
	// BuildFiles are the confirmed build files of a repository draft. Untrusted.
	BuildFiles []BuildFile
}

// Payload is what the first message carries: the sections in order, and the map from each letter to the
// name it stands for, which only the person's browser sees.
type Payload struct {
	Sections []types.AssistantSection
	Letters  map[string]string
}

// FirstPayload builds the records of the first message, in the order the preview shows them. Names in any
// text become letters. Nothing else is in it.
func FirstPayload(in Inputs) Payload {
	l := NewLetters()
	for _, p := range in.UnitAdmins {
		l.Add(p)
	}
	if in.ChangeProposer != nil {
		l.Add(*in.ChangeProposer)
	}
	for _, p := range in.RunOwners {
		l.Add(p)
	}
	for _, p := range in.ChangePeople {
		l.Add(p)
	}
	var secs []types.AssistantSection
	add := func(label, text string, untrusted bool) {
		if strings.TrimSpace(text) != "" {
			secs = append(secs, types.AssistantSection{Label: label, Text: l.Apply(text), Untrusted: untrusted})
		}
	}
	add(LabelUnit, in.UnitRecord, false)
	add(LabelRun, in.RunRecord, false)
	add(LabelTemplates, strings.Join(in.Templates, "\n"), true)
	add(LabelAdmins, adminLine(in.UnitAdmins, l), false)
	add(LabelGrants, strings.Join(in.Grants, "\n"), false)
	if in.RunHistoryOn {
		add(LabelHistory, historyText(in.RunHistory), false)
	}
	if in.Purpose == types.AssistantReview {
		add(LabelChange, in.Change, true)
	}
	add(LabelBuild, buildText(in.BuildFiles), true)
	return Payload{Sections: capSections(secs), Letters: l.Map()}
}

func adminLine(admins []Person, l *Letters) string {
	labels := make([]string, 0, len(admins))
	for _, a := range admins {
		if label := l.Add(a); label != "" {
			labels = append(labels, label)
		}
	}
	return strings.Join(labels, ", ")
}

// historyText is one line per host and decision, as the preview draws it: host · decision · runs.
func historyText(h []HostCount) string {
	lines := make([]string, len(h))
	for i, c := range h {
		lines[i] = fmt.Sprintf("%s · %s · %d", c.Host, decisionWord(c.Decision), c.Count) // PROPOSED: the preview's words
	}
	return strings.Join(lines, "\n")
}

// decisionWord is how a history line names a decision: a refused host reads as refused.
func decisionWord(d string) string {
	switch d {
	case "allow":
		return "allowed" // PROPOSED: the preview's words
	case "deny":
		return "refused" // PROPOSED
	}
	return d
}

func buildText(files []BuildFile) string {
	parts := make([]string, len(files))
	for i, f := range files {
		parts[i] = "--- " + f.Name + "\n" + f.Text
	}
	return strings.Join(parts, "\n")
}

// capSections cuts each section to MaxSectionBytes, then, in order, the payload to MaxPayloadBytes: earlier
// sections keep their bytes and a later one is cut or left out.
func capSections(secs []types.AssistantSection) []types.AssistantSection {
	total := 0
	out := make([]types.AssistantSection, 0, len(secs))
	for _, s := range secs {
		s.Text = cutTo(s.Text, MaxSectionBytes)
		if room := MaxPayloadBytes - total; len(s.Text) > room {
			if room <= len(truncatedMark) {
				break
			}
			s.Text = cutTo(s.Text, room)
		}
		total += len(s.Text)
		out = append(out, s)
	}
	return out
}

// cutTo is text within n bytes, ending on a rune boundary and carrying the cut mark when it was cut.
func cutTo(text string, n int) string {
	if len(text) <= n {
		return text
	}
	cut := n - len(truncatedMark)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + truncatedMark
}
