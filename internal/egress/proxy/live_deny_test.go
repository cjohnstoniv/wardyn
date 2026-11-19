// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// longEntry is the longest entry a list holds, made distinct by i: `*.`, a 253-byte name, `:65535`.
func longEntry(i int) string {
	label := strings.Repeat("a", 59) + fmt.Sprintf("%04d", i)
	name := label + "." + strings.Repeat("b", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("b", 61)
	return "*." + name + ":65535"
}

const liveBase = `{"run_id":"r","policy":{"allowed_domains":["a.example"]},"listen":":3128"}`

func TestWithLiveDenies_Composable(t *testing.T) {
	x := []byte(liveBase)
	A, B := []string{"b.example", "*.c.example"}, []string{"d.example:443", "b.example"}
	ab, err := WithLiveDenies(x, append(slices.Clone(A), B...))
	if err != nil {
		t.Fatal(err)
	}
	a, err := WithLiveDenies(x, A)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := WithLiveDenies(a, B)
	if err != nil {
		t.Fatal(err)
	}
	if string(twice) != string(ab) {
		t.Fatalf("A then B = %s\nA union B = %s", twice, ab)
	}
	if again, _ := WithLiveDenies(ab, nil); string(again) != string(ab) {
		t.Fatalf("an output with nothing added changed:\n%s\n%s", again, ab)
	}
	// Nothing to add and no key: the same bytes, even though they are not canonical.
	if got, err := WithLiveDenies(x, nil); err != nil || &got[0] != &x[0] {
		t.Fatalf("WithLiveDenies(x, nil) = %s, %v; want x itself", got, err)
	}
	if got, err := WithLiveDenies(x, []string{}); err != nil || &got[0] != &x[0] {
		t.Fatalf("WithLiveDenies(x, []) = %s, %v; want x itself", got, err)
	}
	// Sorted keys, compacted values.
	spaced := []byte("{ \"policy\" : { \"allowed_domains\" : [ \"a.example\" ] } , \"listen\":\":3128\", \"run_id\": \"r\" }")
	got, err := WithLiveDenies(spaced, []string{"b.example"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"listen":":3128","live_denies":["b.example"],"policy":{"allowed_domains":["a.example"]},"run_id":"r"}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestWithLiveDenies_NormalizesAsTheAPI(t *testing.T) {
	got, err := WithLiveDenies([]byte(liveBase), []string{" B.Example ", "a.example", "b.example", "*.C.example"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := liveDenyList(got)
	if err != nil || !slices.Equal(list, []string{"*.c.example", "a.example", "b.example"}) {
		t.Fatalf("list = %v, %v", list, err)
	}
	if _, err := WithLiveDenies([]byte(liveBase), []string{"bad host"}); err == nil {
		t.Fatal("an invalid entry was accepted")
	}
	many := make([]string, MaxLiveDenies+1)
	for i := range many {
		many[i] = fmt.Sprintf("h%d.example", i)
	}
	if _, err := WithLiveDenies([]byte(liveBase), many); err == nil {
		t.Fatal("1025 entries were accepted")
	}
	// The list a config already carries is joined, and checked.
	if _, err := WithLiveDenies([]byte(`{"live_denies":["bad host"]}`), nil); err == nil {
		t.Fatal("an invalid stored entry was accepted")
	}
}

func TestNormalizeLiveDenies_EntryAndListBounds(t *testing.T) {
	long := longEntry(0)
	if len(long) != MaxLiveDenyEntryBytes {
		t.Fatalf("the longest entry is %d bytes, want %d", len(long), MaxLiveDenyEntryBytes)
	}
	if _, err := NormalizeLiveDenies([]string{long}); err != nil {
		t.Fatalf("a %d-byte entry: %v", len(long), err)
	}
	over := "*.a" + strings.TrimPrefix(long, "*.") // a valid entry one byte longer
	if err := ValidDomainEntry(over); err != nil || len(over) != MaxLiveDenyEntryBytes+1 {
		t.Fatalf("the %d-byte entry is not a valid one: %v", len(over), err)
	}
	if _, err := NormalizeLiveDenies([]string{"b.example", over}); err == nil || !strings.Contains(err.Error(), "entry 1") {
		t.Fatalf("a %d-byte entry: %v; want an error naming entry 1", len(over), err)
	}
	full := make([]string, MaxLiveDenies)
	for i := range full {
		full[i] = longEntry(i)
	}
	if out, err := NormalizeLiveDenies(full); err != nil || len(out) != MaxLiveDenies {
		t.Fatalf("1024 long entries: %d, %v", len(out), err)
	}
	if _, err := NormalizeLiveDenies(append(full, longEntry(MaxLiveDenies))); err == nil {
		t.Fatal("1025 entries were accepted")
	}
}

// The value list: how NormalizeLiveDenies reads each kind of input.
func TestNormalizeLiveDenies_Values(t *testing.T) {
	out, err := NormalizeLiveDenies([]string{" A.Example ", "a.example", "*.b.example", "c.example:8443"})
	if err != nil || !slices.Equal(out, []string{"*.b.example", "a.example", "c.example:8443"}) {
		t.Fatalf("got %v, %v", out, err)
	}
	for _, bad := range [][]string{{""}, {"  "}, {"bad host"}, {"a.example", ""}, {"x.example:0"}, {"a.*.example"}} {
		if out, err := NormalizeLiveDenies(bad); err == nil {
			t.Errorf("%q = %v; want an error", bad, out)
		}
	}
	if out, err := NormalizeLiveDenies(nil); err != nil || len(out) != 0 {
		t.Errorf("nil = %v, %v", out, err)
	}
}

func TestNarrowMessageBound_FitsTheLargestAnswer(t *testing.T) {
	list := make([]string, MaxLiveDenies)
	for i := range list {
		list[i] = longEntry(i)
	}
	req, _ := json.Marshal(narrowRequest{Deny: &list})
	hash := ConfigSHA256([]byte("x"))
	ans, _ := json.Marshal(NarrowAnswer{State: "applied", Changed: true, ConfigSHA256: hash, BaseSHA256: hash, LiveDenies: list})
	if len(req) > MaxNarrowMessageBytes || len(ans) > MaxNarrowMessageBytes {
		t.Fatalf("request %d and answer %d bytes against a bound of %d", len(req), len(ans), MaxNarrowMessageBytes)
	}
	arr, _ := json.Marshal(list)
	if want := MaxLiveDenies*(MaxLiveDenyEntryBytes+3) + 1; len(arr) != want || len(req) != len(arr)+len(`{"deny":}`) {
		t.Fatalf("the list is %d bytes (want %d) and the request %d", len(arr), want, len(req))
	}
	if len(ans)-len(req) > 300 {
		t.Fatalf("the answer adds %d bytes to the request; the bound allows under 300", len(ans)-len(req))
	}
	// A two-edit answer of 69,974 bytes.
	var small []string
	for i := 0; ; i++ {
		small = append(small, fmt.Sprintf("host%d.example", i))
		b, _ := json.Marshal(NarrowAnswer{State: "applied", Changed: true, ConfigSHA256: hash, BaseSHA256: hash, LiveDenies: small})
		if len(b) >= 69974-14 {
			pad := 69974 - len(b)
			small[len(small)-1] += strings.Repeat("a", pad)
			b, _ = json.Marshal(NarrowAnswer{State: "applied", Changed: true, ConfigSHA256: hash, BaseSHA256: hash, LiveDenies: small})
			if len(b) != 69974 || len(b) > MaxNarrowMessageBytes {
				t.Fatalf("answer is %d bytes, want 69974 within %d", len(b), MaxNarrowMessageBytes)
			}
			return
		}
	}
}

func TestLiveDenyBaseSHA256_IgnoresTheList(t *testing.T) {
	base := []byte(liveBase)
	want, err := LiveDenyBaseSHA256(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range [][]string{{"a.example"}, {"a.example", "*.b.example"}} {
		with, err := WithLiveDenies(base, list)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := LiveDenyBaseSHA256(with); err != nil || got != want {
			t.Errorf("list %v: %s, %v; want %s", list, got, err, want)
		}
	}
	other, _ := LiveDenyBaseSHA256([]byte(`{"run_id":"r","policy":{"allowed_domains":["z.example"]},"listen":":3128"}`))
	if other == want {
		t.Error("a changed policy gave the same hash")
	}
}

func TestEvalHost_LiveDenyBeatsAllow(t *testing.T) {
	deny := []string{"a.example", "*.b.example", "c.example:8443"}
	for name, spec := range map[string]types.RunPolicySpec{
		"allowlist":  {AllowedDomains: []string{"a.example", "x.b.example", "c.example"}},
		"allow all":  {AllowAllEgress: true},
		"first use":  {AllowedDomains: []string{"a.example"}, FirstUseApproval: types.FirstUseDenyWithReview},
		"no policy":  {},
		"port allow": {AllowedDomains: []string{"c.example:8443", "c.example:443"}},
	} {
		p := CompilePolicy(spec)
		before := map[string]hostDecision{
			"a.example": p.evalHost("a.example", 443), "x.b.example": p.evalHost("x.b.example", 443),
			"c.example": p.evalHost("c.example", 8443), "c.example:443": p.evalHost("c.example", 443),
		}
		p.live.set(deny)
		for _, tc := range []struct {
			host string
			port int
			deny bool
		}{
			{"a.example", 443, true}, {"A.Example.", 80, true}, {"x.b.example", 443, true}, {"b.example", 443, false},
			{"c.example", 8443, true}, {"c.example", 443, false}, {"other.example", 443, false},
		} {
			got := p.evalHost(tc.host, tc.port)
			if tc.deny && got != hostDeny {
				t.Errorf("%s: %s:%d = %s, want deny", name, tc.host, tc.port, got)
			}
			if !tc.deny && got == hostDeny {
				t.Errorf("%s: %s:%d denied, want it left alone", name, tc.host, tc.port)
			}
		}
		if got := p.evalHost("c.example", 443); got != before["c.example:443"] {
			t.Errorf("%s: c.example:443 = %s, was %s: a port-qualified entry denied another port", name, got, before["c.example:443"])
		}
		p.live.set(nil)
		if got := p.evalHost("a.example", 443); got != before["a.example"] {
			t.Errorf("%s: an emptied list left a.example at %s, was %s", name, got, before["a.example"])
		}
	}
	var none *liveDenies
	if none.denies("a.example", 443) {
		t.Error("a nil list denied")
	}
	if (&Policy{}).evalHost("a.example", 443) == hostDeny {
		t.Error("a Policy with no live list denied")
	}
}

// The proxy's verdicts under the launch policy plus the list equal those of a policy that holds the list
// as denied domains and has taken its entries off the allowlist.
func TestLiveDenyMatchesPolicyDeny(t *testing.T) {
	list := []string{"a.example", "*.corp.example", "svc.example:443", "x.example:8443"}
	for name, launch := range map[string]types.RunPolicySpec{
		"allowlist": {AllowedDomains: []string{"a.example", "*.corp.example", "svc.example:443", "svc.example:80", "x.example", "keep.example"}},
		"allow all": {AllowAllEgress: true, DeniedDomains: []string{"old.example"}},
	} {
		live := CompilePolicy(launch)
		live.live.set(list)
		model := launch
		model.AllowedDomains = nil
		for _, d := range launch.AllowedDomains {
			if !slices.Contains(list, d) {
				model.AllowedDomains = append(model.AllowedDomains, d)
			}
		}
		model.DeniedDomains = append(slices.Clone(launch.DeniedDomains), list...)
		want := CompilePolicy(model)
		for _, host := range []string{"a.example", "A.EXAMPLE", "sub.a.example", "corp.example", "x.corp.example", "y.x.corp.example", "svc.example", "x.example", "keep.example", "old.example", "other.example", "10.0.0.1"} {
			for _, port := range []int{80, 443, 8443} {
				if got, w := live.evalHost(host, port), want.evalHost(host, port); got != w {
					t.Errorf("%s: %s:%d = %s, the model policy says %s", name, host, port, got, w)
				}
			}
		}
	}
}
