// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner_resident ssh_key or git_pat rides the proxy configuration's
// ResidentMints: the runner fills each Token from its keystore for the proxy
// it starts, at create and on revive, and never for the sandbox; every copy it
// persists keeps the placeholder. Past not_after the fill is refused.
func TestResident_ResidentMintsFilledForTheProxyOnly(t *testing.T) {
	f := newResidentFixture(t, nil)
	key, pat := uuid.New(), uuid.New()
	if err := f.deliver(key, "ssh-private-key", false); err != nil {
		t.Fatal(err)
	}
	if err := f.deliver(pat, "pat-value", false); err != nil {
		t.Fatal(err)
	}
	expires := f.clock.now().Add(time.Hour)
	mints := []proxy.ResidentMint{
		{GrantID: key, Kind: types.GrantSSHKey, Username: "git", Token: placement.ResidentPlaceholder(key), ExpiresAt: expires},
		{GrantID: pat, Kind: types.GrantGitPAT, Username: "pat", Token: placement.ResidentPlaceholder(pat), ExpiresAt: expires},
	}
	var persisted runner.SandboxSpec
	f.d.cfg.Created = func(_ context.Context, s runner.SandboxSpec, _ string) error { persisted = s; return nil }
	if err := f.create(runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{ResidentMints: mints}}); err != nil {
		t.Fatal(err)
	}
	got := f.fake.Created[0].ProxyConfig.ResidentMints
	if len(got) != 2 || got[0].Token != "ssh-private-key" || got[1].Token != "pat-value" || got[0].Username != "git" || !got[1].ExpiresAt.Equal(expires) {
		t.Fatalf("the proxy's resident mints = %+v, want both tokens filled and the rest kept", got)
	}
	if p := persisted.ProxyConfig.ResidentMints; len(p) != 2 || p[0].Token != placement.ResidentPlaceholder(key) || p[1].Token != placement.ResidentPlaceholder(pat) {
		t.Fatalf("the persisted spec carries a value: %+v", p)
	}

	// Revive: the org's stored configuration carries the placeholders again.
	ref := placement.RunnerRefPrefix(f.id) + "fake-" + f.run.String()
	cfg := `{"run_id":"` + f.run.String() + `","resident_mints":[{"grant_id":"` + pat.String() + `","kind":"git_pat","token":"` +
		placement.ResidentPlaceholder(pat) + `","expires_at":"` + expires.UTC().Format(time.RFC3339) + `"}]}`
	data, _ := runnerwire.Marshal(runnerwire.ReplaceProxyArgs{Ref: ref, CfgJSON: []byte(cfg)})
	if _, err := f.d.Call(context.Background(), f.peer, 4, runnerwire.MethodReplaceProxy, data); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(f.fake.Calls, func(c string) bool { return strings.HasPrefix(c, "replace_proxy ") })
	if i < 0 || !strings.Contains(f.fake.Calls[i], `"token":"pat-value"`) {
		t.Fatalf("revive did not fill the resident mint: %v", f.fake.Calls)
	}

	// A proxy-only value never reaches the sandbox, and nothing is read past not_after.
	r := newResidentFixture(t, nil)
	if err := r.deliver(key, "ssh-private-key", false); err != nil {
		t.Fatal(err)
	}
	if err := r.create(runner.SandboxSpec{SecretEnv: map[string]string{"KEY": placement.ResidentPlaceholder(key)}}); err == nil || len(r.fake.Created) != 0 {
		t.Fatalf("a resident ssh_key reached the sandbox (%v)", err)
	}
	late := newResidentFixture(t, nil)
	if err := late.deliver(pat, "pat-value", false); err != nil {
		t.Fatal(err)
	}
	late.clock.add(2 * time.Hour)
	if err := late.create(runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{ResidentMints: mints[1:]}}); err == nil || len(late.fake.Created) != 0 {
		t.Fatalf("a resident mint was filled past its not_after (%v)", err)
	}
}
