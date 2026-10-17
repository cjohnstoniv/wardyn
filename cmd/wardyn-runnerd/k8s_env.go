// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// k8sEnvRead is every environment variable a k8s runnerd reads beside its flags' own twins: the
// chart's settings that have no flag (deploy/helm/wardyn-runnerd/templates/deployment.yaml). The
// chart test holds the chart to this list, and an unknown WARDYN_RUNNERD_* name refuses startup, so
// a renamed or mistyped security knob is never silently dropped.
var k8sEnvRead = []string{
	"POD_IP",
	"WARDYN_RUNNERD_RELAY_PORT",
	"WARDYN_RUNNERD_RELAY_SERVICE_PORT",
	"WARDYN_RUNNERD_SINK_PORT",
	"WARDYN_RUNNERD_CLUSTER_DOMAIN",
	"WARDYN_RUNNERD_APISERVER_PEERS",
	"WARDYN_RUNNERD_HARD_DENY_CIDRS",
	"WARDYN_RUNNERD_POD_SELECTOR",
	archEnv,
	"WARDYN_CONFINEMENT_MAP",
	"WARDYN_K8S_READ_NODES",
	"WARDYN_K8S_IMAGE_PULL_SECRET",
	"WARDYN_K8S_SANDBOX_PLACEMENT",
}

// k8sEnv is the parsed chart settings.
type k8sEnv struct {
	podIP                             netip.Addr
	relayPort, relayServicePort, sink int32
	clusterDomain                     string
	apiserverPeers, hardDenyCIDRs     []string
	podSelector                       map[string]string
	confinement                       map[types.ConfinementClass]string
}

// readK8sEnv parses environ (os.Environ's form). Every value is checked: a malformed or missing
// security setting is an error, never a default.
func readK8sEnv(environ []string) (k8sEnv, error) {
	env := map[string]string{}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	var e k8sEnv
	var errs []error
	for k := range env {
		if strings.HasPrefix(k, "WARDYN_RUNNERD_") && !slices.Contains(k8sEnvRead, k) {
			errs = append(errs, fmt.Errorf("%s is not a setting this runnerd reads", k))
		}
	}
	var err error
	if e.podIP, err = netip.ParseAddr(env["POD_IP"]); err != nil {
		errs = append(errs, fmt.Errorf("POD_IP: %w", err))
	}
	port := func(name string) int32 {
		n, perr := strconv.ParseUint(env[name], 10, 16)
		if perr != nil || n == 0 {
			errs = append(errs, fmt.Errorf("%s: want a port, got %q", name, env[name]))
		}
		return int32(n)
	}
	e.relayPort, e.relayServicePort, e.sink = port("WARDYN_RUNNERD_RELAY_PORT"), port("WARDYN_RUNNERD_RELAY_SERVICE_PORT"), port("WARDYN_RUNNERD_SINK_PORT")
	if e.relayPort == e.sink && e.sink != 0 {
		errs = append(errs, errors.New("WARDYN_RUNNERD_RELAY_PORT and WARDYN_RUNNERD_SINK_PORT are the same port"))
	}
	e.clusterDomain = strings.TrimSuffix(env["WARDYN_RUNNERD_CLUSTER_DOMAIN"], ".")
	if e.clusterDomain == "" || strings.ContainsAny(e.clusterDomain, " /:@") {
		errs = append(errs, fmt.Errorf("WARDYN_RUNNERD_CLUSTER_DOMAIN: want a DNS name, got %q", env["WARDYN_RUNNERD_CLUSTER_DOMAIN"]))
	}
	if v, ok := env["WARDYN_RUNNERD_APISERVER_PEERS"]; ok {
		if e.apiserverPeers, err = apiserverPeerCIDRs(v); err != nil {
			errs = append(errs, err)
		}
	}
	if v, ok := env["WARDYN_RUNNERD_HARD_DENY_CIDRS"]; ok {
		for _, c := range strings.Split(v, ",") {
			p, perr := netip.ParsePrefix(strings.TrimSpace(c))
			if perr != nil {
				errs = append(errs, fmt.Errorf("WARDYN_RUNNERD_HARD_DENY_CIDRS: %q: %w", c, perr))
				continue
			}
			e.hardDenyCIDRs = append(e.hardDenyCIDRs, p.String())
		}
	}
	if err := json.Unmarshal([]byte(env["WARDYN_RUNNERD_POD_SELECTOR"]), &e.podSelector); err != nil || len(e.podSelector) == 0 {
		errs = append(errs, fmt.Errorf("WARDYN_RUNNERD_POD_SELECTOR: want a JSON object of runnerd's pod labels, got %q", env["WARDYN_RUNNERD_POD_SELECTOR"]))
	}
	if v, ok := env[archEnv]; ok {
		if err := checkArch(v); err != nil {
			errs = append(errs, err)
		}
	}
	if e.confinement, err = parseConfinementMap(env["WARDYN_CONFINEMENT_MAP"]); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return k8sEnv{}, fmt.Errorf("runner k8s settings: %w", err)
	}
	return e, nil
}

// apiserverPeerCIDRs reads apiServer.peers (a JSON list of NetworkPolicyPeer) into the CIDRs the
// proxy's hard deny covers. A peer with no ipBlock names nothing the proxy could match, so it is
// refused rather than dropped.
func apiserverPeerCIDRs(v string) ([]string, error) {
	var peers []struct {
		IPBlock *struct {
			CIDR string `json:"cidr"`
		} `json:"ipBlock"`
	}
	if err := json.Unmarshal([]byte(v), &peers); err != nil {
		return nil, fmt.Errorf("WARDYN_RUNNERD_APISERVER_PEERS: %w", err)
	}
	var out []string
	for i, p := range peers {
		if p.IPBlock == nil {
			return nil, fmt.Errorf("WARDYN_RUNNERD_APISERVER_PEERS: peer %d has no ipBlock, which the proxy's hard deny cannot hold", i)
		}
		pfx, err := netip.ParsePrefix(p.IPBlock.CIDR)
		if err != nil {
			return nil, fmt.Errorf("WARDYN_RUNNERD_APISERVER_PEERS: peer %d: %w", i, err)
		}
		out = append(out, pfx.String())
	}
	return out, nil
}

// parseConfinementMap reads WARDYN_CONFINEMENT_MAP ("CC2=runsc;CC3=kata-qemu") as wardynd does:
// an unknown class, a malformed entry or an empty runtime is an error.
func parseConfinementMap(s string) (map[types.ConfinementClass]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	out := map[types.ConfinementClass]string{}
	for _, part := range strings.Split(s, ";") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		class, val := types.ConfinementClass(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch {
		case !ok:
			return nil, fmt.Errorf("WARDYN_CONFINEMENT_MAP: malformed entry %q (want CLASS=runtime)", part)
		case class != types.CC1 && class != types.CC2 && class != types.CC3:
			return nil, fmt.Errorf("WARDYN_CONFINEMENT_MAP: unknown confinement class %q", class)
		case val == "" || strings.Contains(val, ":"):
			return nil, fmt.Errorf("WARDYN_CONFINEMENT_MAP: runtime %q for %s", val, class)
		}
		out[class] = val
	}
	return out, nil
}

// k8sServeFlags are the serve flags only a k8s runnerd takes.
var k8sServeFlags = []string{"runs-namespace", "relay-service", "arch", "max-pod-lifetime"}

// checkSubstrateFlags refuses a flag the chosen substrate does not read.
func checkSubstrateFlags(substrate string, changed func(string) bool) error {
	switch substrate {
	case "docker":
		for _, f := range k8sServeFlags {
			if changed(f) {
				return fmt.Errorf("--%s applies only to --substrate=k8s", f)
			}
		}
	case "k8s":
		for _, f := range []string{"relay-host-target", "internal-network"} {
			if changed(f) {
				return fmt.Errorf("--%s does not apply to --substrate=k8s", f)
			}
		}
	default:
		return fmt.Errorf("--substrate %q: want docker or k8s", substrate)
	}
	return nil
}

// archEnv is --arch's environment twin, on serve and register alike.
const archEnv = "WARDYN_RUNNERD_ARCH"

// checkArch accepts the architectures runner images are built for. A k8s runner requires one: it
// pins every sandbox pod to nodes of that arch (design K1 §6.1).
func checkArch(a string) error {
	if a != "amd64" && a != "arm64" {
		return fmt.Errorf("--arch (%s) %q: want amd64 or arm64", archEnv, a)
	}
	return nil
}

// checkMaxPodLifetime refuses a lifetime that is not a positive whole number of seconds.
func checkMaxPodLifetime(d time.Duration) error {
	if d < time.Minute || d%time.Second != 0 {
		return fmt.Errorf("--max-pod-lifetime %s: want a whole number of seconds, at least 1m", d)
	}
	return nil
}

// orgHTTPClient trusts the system roots plus the runner's CA bundle for the organisation session
// (design K1 §5.2): the stream dials with the transport's roots. nil leaves the default transport.
func orgHTTPClient(bundle string) (*http.Client, error) {
	if bundle == "" {
		return nil, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pool = pool.Clone()
	if !pool.AppendCertsFromPEM([]byte(bundle)) {
		return nil, errors.New("trusted CA bundle holds no certificate")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: tr}, nil
}
