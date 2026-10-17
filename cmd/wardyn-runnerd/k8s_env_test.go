// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"
)

func k8sEnvFixture(extra ...string) []string {
	return append([]string{
		"POD_IP=10.244.0.12",
		"WARDYN_RUNNERD_RELAY_PORT=8443",
		"WARDYN_RUNNERD_RELAY_SERVICE_PORT=8443",
		"WARDYN_RUNNERD_SINK_PORT=9443",
		"WARDYN_RUNNERD_CLUSTER_DOMAIN=cluster.local",
		`WARDYN_RUNNERD_POD_SELECTOR={"app.kubernetes.io/name":"wardyn-runnerd"}`,
		"PATH=/usr/bin",
	}, extra...)
}

func TestReadK8sEnv(t *testing.T) {
	e, err := readK8sEnv(k8sEnvFixture(
		`WARDYN_RUNNERD_APISERVER_PEERS=[{"ipBlock":{"cidr":"172.20.0.1/32"}}]`,
		"WARDYN_RUNNERD_HARD_DENY_CIDRS=10.96.0.0/12, 10.244.0.0/16",
		"WARDYN_CONFINEMENT_MAP=CC2=runsc;CC3=kata-qemu",
		"WARDYN_RUNNERD_ARCH=arm64",
	))
	if err != nil {
		t.Fatal(err)
	}
	if e.podIP.String() != "10.244.0.12" || e.relayPort != 8443 || e.sink != 9443 || e.clusterDomain != "cluster.local" ||
		len(e.apiserverPeers) != 1 || len(e.hardDenyCIDRs) != 2 || e.podSelector["app.kubernetes.io/name"] != "wardyn-runnerd" || e.confinement["CC3"] != "kata-qemu" {
		t.Fatalf("env %+v", e)
	}
	for _, bad := range []struct{ name, kv, want string }{
		{"unknown runnerd name", "WARDYN_RUNNERD_RELAY_PROT=8443", "WARDYN_RUNNERD_RELAY_PROT is not a setting"},
		{"peer without ipBlock", `WARDYN_RUNNERD_APISERVER_PEERS=[{"namespaceSelector":{}}]`, "has no ipBlock"},
		{"peers not JSON", "WARDYN_RUNNERD_APISERVER_PEERS=10.0.0.1", "WARDYN_RUNNERD_APISERVER_PEERS"},
		{"bad CIDR", "WARDYN_RUNNERD_HARD_DENY_CIDRS=10.96.0.0/12,nope", "WARDYN_RUNNERD_HARD_DENY_CIDRS"},
		{"bad port", "WARDYN_RUNNERD_SINK_PORT=0", "WARDYN_RUNNERD_SINK_PORT"},
		{"bad class", "WARDYN_CONFINEMENT_MAP=CC9=runsc", "unknown confinement class"},
		{"empty selector", "WARDYN_RUNNERD_POD_SELECTOR={}", "WARDYN_RUNNERD_POD_SELECTOR"},
		{"bad arch", "WARDYN_RUNNERD_ARCH=x86_64", "WARDYN_RUNNERD_ARCH"},
		{"empty arch", "WARDYN_RUNNERD_ARCH=", "WARDYN_RUNNERD_ARCH"},
		{"unenforced netpol override", "WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1", "WARDYN_K8S_ALLOW_UNENFORCED_NETPOL is set"},
		{"unenforced netpol override off", "WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=false", "WARDYN_K8S_ALLOW_UNENFORCED_NETPOL is set"},
		{"ambient deny override", "WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY=true", "WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY is set"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if _, err := readK8sEnv(k8sEnvFixture(bad.kv)); err == nil || !strings.Contains(err.Error(), bad.want) {
				t.Fatalf("readK8sEnv = %v, want an error naming %q", err, bad.want)
			}
		})
	}
	if _, err := readK8sEnv([]string{"PATH=/usr/bin"}); err == nil {
		t.Fatal("missing settings accepted")
	}
}

func TestSubstrateFlagChecks(t *testing.T) {
	changed := func(set ...string) func(string) bool {
		return func(f string) bool { return strings.Contains(" "+strings.Join(set, " ")+" ", " "+f+" ") }
	}
	if err := checkSubstrateFlags("docker", changed()); err != nil {
		t.Fatal(err)
	}
	if err := checkSubstrateFlags("docker", changed("runs-namespace")); err == nil {
		t.Fatal("docker took a k8s flag")
	}
	if err := checkSubstrateFlags("k8s", changed("internal-network")); err == nil {
		t.Fatal("k8s took a docker flag")
	}
	if err := checkSubstrateFlags("podman", changed()); err == nil {
		t.Fatal("unknown substrate accepted")
	}
	if checkArch("amd64") != nil || checkArch("") == nil || checkArch("arm64") != nil || checkArch("x86_64") == nil {
		t.Fatal("arch check")
	}
	if checkMaxPodLifetime(168*time.Hour) != nil || checkMaxPodLifetime(0) == nil || checkMaxPodLifetime(1500*time.Millisecond+time.Minute) == nil {
		t.Fatal("max pod lifetime check")
	}
}

func TestOrgHTTPClientTrustsBundle(t *testing.T) {
	if c, err := orgHTTPClient(""); c != nil || err != nil {
		t.Fatalf("no bundle: %v %v", c, err)
	}
	if _, err := orgHTTPClient("not a certificate"); err == nil {
		t.Fatal("a bundle with no certificate accepted")
	}
}

// TestServeArchEnvRefusedOnDocker: WARDYN_RUNNERD_ARCH counts as --arch, so a Docker serve refuses it
// before reading any identity, as it refuses the flag.
func TestServeArchEnvRefusedOnDocker(t *testing.T) {
	t.Setenv(archEnv, "amd64")
	cmd := newServeCommand(nil)
	cmd.SetArgs([]string{"--org", "https://wardyn.example.com", "--state-dir", t.TempDir()})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), archEnv+" in the environment) applies only to --substrate=k8s") {
		t.Fatalf("docker serve with %s = %v, want the k8s-only refusal", archEnv, err)
	}
}
