// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "fmt"

// The outage allowance bounds the time a run's network keeps serving while Wardyn cannot be reached (#4082): one hour by
// default, up to 24 hours where an admin raises it for the site, and down to 30 minutes where a policy lowers it.
const (
	OutageAllowanceDefaultMinutes = 60
	OutageAllowanceMinMinutes     = 30
	OutageAllowanceMaxMinutes     = 1440
)

// The values of OutageControls.Unattended.
const (
	OutageUnattendedKeep = "keep"
	OutageUnattendedEnd  = "end"
)

// OutageControls is a policy's say over a run's outage terms. Nil, or a zero field, takes the site's value.
type OutageControls struct {
	// AllowanceMinutes lowers the site's allowance for the policy's runs: 0 takes the site's, else 30 to 1440.
	AllowanceMinutes int `json:"allowance_minutes,omitempty"`
	// Strict ends the run at the allowance instead of pausing its network until Wardyn checks it again.
	Strict bool `json:"strict,omitempty"`
	// Unattended is what a long outage does to an unattended run: "" or "keep" keeps it like an attended run, "end"
	// ends it so its pipeline can start a fresh one.
	Unattended string `json:"unattended,omitempty"`
}

// ValidateOutageControls refuses an allowance outside 0 or 30 to 1440 minutes and an unattended choice other than keep or end.
func ValidateOutageControls(o *OutageControls) error {
	if o == nil {
		return nil
	}
	if o.AllowanceMinutes != 0 && (o.AllowanceMinutes < OutageAllowanceMinMinutes || o.AllowanceMinutes > OutageAllowanceMaxMinutes) {
		return fmt.Errorf("outage.allowance_minutes must be 0 or %d to %d", OutageAllowanceMinMinutes, OutageAllowanceMaxMinutes)
	}
	switch o.Unattended {
	case "", OutageUnattendedKeep, OutageUnattendedEnd:
		return nil
	}
	return fmt.Errorf("outage.unattended must be %q or %q", OutageUnattendedKeep, OutageUnattendedEnd)
}

// Meet is the stricter of o and b: the smaller allowance either sets, strict where either is, "end" where either says it.
func (o *OutageControls) Meet(b *OutageControls) OutageControls {
	var out OutageControls
	for _, x := range []*OutageControls{o, b} {
		if x == nil {
			continue
		}
		if x.AllowanceMinutes > 0 && (out.AllowanceMinutes == 0 || x.AllowanceMinutes < out.AllowanceMinutes) {
			out.AllowanceMinutes = x.AllowanceMinutes
		}
		out.Strict = out.Strict || x.Strict
		if x.Unattended == OutageUnattendedEnd {
			out.Unattended = OutageUnattendedEnd
		} else if out.Unattended == "" {
			out.Unattended = x.Unattended
		}
	}
	return out
}

// OutageTerms are a run's effective outage terms, fixed at dispatch.
type OutageTerms struct {
	AllowanceSec  int64
	Strict        bool
	EndUnattended bool
	// ProxyPauses is whether the run's active proxy pauses at its allowance and asks to recover, as its own loaded
	// report said and this server can recover it. It is written per proxy start, not at dispatch, and is not a term:
	// IsDefault ignores it.
	ProxyPauses bool
}

// DefaultOutageTerms are the terms of a run with no site or policy say, and of a run dispatched before terms were kept.
func DefaultOutageTerms() OutageTerms {
	return OutageTerms{AllowanceSec: OutageAllowanceDefaultMinutes * 60}
}

// IsDefault reports whether t are the default terms. ProxyPauses is not compared.
func (t OutageTerms) IsDefault() bool {
	d := DefaultOutageTerms()
	return t.AllowanceSec == d.AllowanceSec && t.Strict == d.Strict && t.EndUnattended == d.EndUnattended
}

// EffectiveOutageTerms are a run's terms from the site's allowance (0 for the default) and its resolved policy's say:
// the policy can only shorten the site's allowance.
func EffectiveOutageTerms(siteMinutes int, policy *OutageControls) OutageTerms {
	minutes := OutageAllowanceDefaultMinutes
	if siteMinutes > 0 {
		minutes = siteMinutes
	}
	t := OutageTerms{}
	if policy != nil {
		if policy.AllowanceMinutes > 0 && policy.AllowanceMinutes < minutes {
			minutes = policy.AllowanceMinutes
		}
		t.Strict = policy.Strict
		t.EndUnattended = policy.Unattended == OutageUnattendedEnd
	}
	t.AllowanceSec = int64(minutes) * 60
	return t
}
