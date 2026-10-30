// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// GetSiteConfig lets the macOS placement tests' store answer the maximum base age (macOSVMRefusal reads the
// site config): none, unless a test says otherwise. ownedRunners embeds a nil store.Store, which would panic.
func (ownedRunners) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}

// With a maximum base age set, a Mac whose base is older, or whose runner reports no age, is refused with the
// approved sentence and its pool form; at the maximum, under it, or with no maximum, it is offered.
func TestMacOSVMOffer_BaseAge(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	rn := managedMac("mac-studio-07")
	mac := func(created string) runnerwire.Caps {
		mc := readyMac()
		mc.Base.CreatedAt = created
		return runnerwire.Caps{MacOSVM: mc}
	}
	ago := func(days int) string { return now.AddDate(0, 0, -days).Format(time.RFC3339) }
	limit := macOSBaseLimit{MaxDays: 30, Now: now}

	for name, created := range map[string]string{"under": ago(5), "at the maximum": ago(30)} {
		if o := macOSVMOffer(rn, mac(created), true, 0, false, limit); !o.Offered {
			t.Errorf("%s: refused: %+v", name, o.Refusal)
		}
	}
	if o := macOSVMOffer(rn, mac(ago(400)), true, 0, false); !o.Offered {
		t.Errorf("no maximum: refused: %+v", o.Refusal)
	}
	if o := macOSVMOffer(rn, mac(""), true, 0, false, macOSBaseLimit{}); !o.Offered {
		t.Errorf("a zero limit refused a base with no age: %+v", o.Refusal)
	}

	// The pool sentence is "1 day" at a maximum of 1 and "<n> days" otherwise.
	for maxDays, want := range map[int]string{1: "in the last 1 day,", 2: "in the last 2 days,"} {
		o := macOSVMOffer(rn, mac(ago(41)), true, 0, false, macOSBaseLimit{MaxDays: maxDays, Now: now})
		if !strings.Contains(o.Pool, want) {
			t.Errorf("maximum %d: pool sentence %q, want it to contain %q", maxDays, o.Pool, want)
		}
	}

	o := macOSVMOffer(rn, mac(ago(41)), true, 0, false, limit)
	want := "mac-studio-07's macOS base was prepared 41 days ago, and your organisation allows 30. Its owner runs `wardyn-runnerd macos-vm prepare --new-image`."
	if o.Offered || o.Why != client.MacOSVMWhyBaseTooOld || o.Refusal.Detail != want || o.Have != 41 || o.Need != 30 ||
		o.Pool != "Every runner in this pool must have a macOS base prepared in the last 30 days, and at least one doesn't. Choose another pool, or ask the Mac's owner to prepare a new base." {
		t.Fatalf("too old: %+v", o)
	}

	for name, created := range map[string]string{"no age": "", "not a date": "last week"} {
		o := macOSVMOffer(rn, mac(created), true, 0, false, limit)
		if o.Offered || o.Why != client.MacOSVMWhyBaseTooOld || o.Have != 0 ||
			o.Refusal.Detail != "mac-studio-07 doesn't report its macOS base's age, and your organisation sets a maximum. Update the runner." {
			t.Errorf("%s: %+v", name, o)
		}
	}

	// A base still preparing is told so, not that it is old.
	pre := mac("")
	pre.MacOSVM.Base = runnerwire.MacVMBase{State: runnerwire.MacVMBasePreparing, Progress: 40}
	if o := macOSVMOffer(rn, pre, true, 0, false, limit); o.Why != client.MacOSVMWhyPreparing {
		t.Errorf("a preparing base: %+v", o)
	}

	// The pool list names the runner with the age and the maximum.
	got := poolMacOSVM([]macOSCandidate{macCandidate(rn, mac(ago(41)).MacOSVM, 0)}, false, limit)
	if got.Offered || len(got.Refusals) != 1 || got.Refusals[0].Why != client.MacOSVMWhyBaseTooOld || got.Refusals[0].Have != 41 || got.Refusals[0].Need != 30 ||
		!strings.Contains(got.Refusals[0].Detail, "41 days ago") {
		t.Fatalf("pool: %+v", got)
	}
}

// One day reads "1 day ago", every other count "<n> days ago".
func TestDaysAgoText(t *testing.T) {
	for n, want := range map[int]string{0: "0 days ago", 1: "1 day ago", 2: "2 days ago", 41: "41 days ago"} {
		if got := daysAgoText(n); got != want {
			t.Errorf("daysAgoText(%d) = %q, want %q", n, got, want)
		}
	}
}
