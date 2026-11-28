// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package spire

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// The environment the constructor reads (identity.Deps: a provider reads its own config).
const (
	envAgentSocket   = "WARDYN_SPIRE_AGENT_SOCKET"
	envServerAddress = "WARDYN_SPIRE_SERVER_ADDRESS"
	envParentID      = "WARDYN_SPIRE_PARENT_ID"
	envSelectors     = "WARDYN_SPIRE_SELECTORS"
	envPodSA         = "WARDYN_POD_SERVICE_ACCOUNT"
	envRunsNamespace = "WARDYN_K8S_NAMESPACE"
	envProxySA       = "WARDYN_K8S_SPIFFE_PROXY_SERVICE_ACCOUNT"
)

// runIDPlaceholder is replaced by the run's id in every selector of a run's entry.
const runIDPlaceholder = "{run_id}"

// The selectors every run entry must carry, each exactly once: the runs namespace, the proxy's own
// service account, the proxy component and the run. Without the namespace and account, a pod in another
// namespace carrying the proxy's labels would attest as the run's proxy.
const (
	selNSPrefix  = "ns:"
	selSAPrefix  = "sa:"
	selComponent = "pod-label:wardyn.component:proxy"
	selRunLabel  = "pod-label:wardyn.run-id:" + runIDPlaceholder
	selRunPrefix = "pod-label:wardyn.run-id:"
)

type config struct {
	agentSocket   string
	serverAddress string
	parentID      spiffeid.ID
	selectors     []Selector
	podSA         string
	proxySA       string
}

// loadConfig reads and checks the environment. Every refusal names what is wrong, so a hand-set variable
// with a typo refuses boot instead of registering entries for another namespace.
func loadConfig(getenv func(string) string, td spiffeid.TrustDomain) (config, error) {
	var c config
	for _, v := range []struct {
		name string
		dst  *string
	}{{envAgentSocket, &c.agentSocket}, {envServerAddress, &c.serverAddress}, {envPodSA, &c.podSA}} {
		if *v.dst = strings.TrimSpace(getenv(v.name)); *v.dst == "" {
			return config{}, fmt.Errorf("spire identity: %s is required", v.name)
		}
	}
	parent, err := spiffeid.FromString(strings.TrimSpace(getenv(envParentID)))
	if err != nil {
		return config{}, fmt.Errorf("spire identity: %s must be a SPIFFE ID: %w", envParentID, err)
	}
	if parent.TrustDomain() != td {
		return config{}, fmt.Errorf("spire identity: %s %s is not in trust domain %s", envParentID, parent, td)
	}
	c.parentID = parent
	if c.selectors, err = parseSelectors(getenv(envSelectors)); err != nil {
		return config{}, err
	}
	c.proxySA = strings.TrimSpace(getenv(envProxySA))
	if err := checkRunSelectors(c.selectors, strings.TrimSpace(getenv(envRunsNamespace)), c.proxySA); err != nil {
		return config{}, err
	}
	if err := checkPodSA(c.podSA, c.proxySA); err != nil {
		return config{}, err
	}
	return c, nil
}

func parseSelectors(raw string) ([]Selector, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("spire identity: %s is required", envSelectors)
	}
	var out []Selector
	for _, f := range strings.Split(raw, ",") {
		f = strings.TrimSpace(f)
		typ, val, ok := strings.Cut(f, ":")
		if !ok || typ == "" || val == "" {
			return nil, fmt.Errorf("spire identity: selector %q in %s is not <type>:<value>", f, envSelectors)
		}
		if typ != "k8s" {
			return nil, fmt.Errorf("spire identity: selector %s is not a k8s selector: this release attests proxies on Kubernetes only", f)
		}
		out = append(out, Selector{Type: typ, Value: val})
	}
	return out, nil
}

// checkRunSelectors requires each run-entry selector exactly once, and the namespace and account values
// to be byte-equal to this install's runs namespace and proxy account.
func checkRunSelectors(sels []Selector, runsNS, proxySA string) error {
	ns, sa, comp, run := pick(sels, selNSPrefix), pick(sels, selSAPrefix), exact(sels, selComponent), exact(sels, selRunLabel)
	switch {
	case len(ns) != 1 || ns[0] == selNSPrefix:
		return fmt.Errorf("spire identity: %s needs exactly one k8s:ns:<namespace> selector", envSelectors)
	case len(sa) != 1 || sa[0] == selSAPrefix:
		return fmt.Errorf("spire identity: %s needs exactly one k8s:sa:<service account> selector", envSelectors)
	case sa[0] == selSAPrefix+"default":
		return fmt.Errorf("spire identity: selector k8s:sa:default does not tell the proxy from the agent pod, which runs as default")
	case comp != 1:
		return fmt.Errorf("spire identity: %s needs the selector k8s:%s exactly once", envSelectors, selComponent)
	case run != 1:
		return fmt.Errorf("spire identity: %s needs the selector k8s:%s exactly once", envSelectors, selRunLabel)
	}
	if err := equalsVar("k8s:"+ns[0], strings.TrimPrefix(ns[0], selNSPrefix), envRunsNamespace, runsNS); err != nil {
		return err
	}
	return equalsVar("k8s:"+sa[0], strings.TrimPrefix(sa[0], selSAPrefix), envProxySA, proxySA)
}

func equalsVar(sel, got, name, want string) error {
	if want == "" {
		return fmt.Errorf("spire identity: selector %s needs %s set, and it is empty", sel, name)
	}
	if got != want {
		return fmt.Errorf("spire identity: selector %s does not equal %s (%s)", sel, name, want)
	}
	return nil
}

// checkPodSA: wardynd's own account must name it alone. A proxy pod running as wardynd's account would
// match wardynd's admin entry, which no run label tells apart.
func checkPodSA(podSA, proxySA string) error {
	switch podSA {
	case "default":
		return fmt.Errorf("spire identity: %s is default, which every pod in the namespace shares", envPodSA)
	case proxySA:
		return fmt.Errorf("spire identity: %s equals %s (%s): the proxies' account must differ from wardynd's", envPodSA, envProxySA, proxySA)
	}
	return nil
}

func pick(sels []Selector, prefix string) []string {
	var out []string
	for _, s := range sels {
		if strings.HasPrefix(s.Value, prefix) {
			out = append(out, s.Value)
		}
	}
	return out
}

func exact(sels []Selector, value string) int {
	n := 0
	for _, s := range sels {
		if s.Value == value {
			n++
		}
	}
	return n
}

// runSelectors is sels with {run_id} filled in for one run.
func runSelectors(sels []Selector, runID string) []Selector {
	out := make([]Selector, len(sels))
	for i, s := range sels {
		out[i] = Selector{Type: s.Type, Value: strings.ReplaceAll(s.Value, runIDPlaceholder, runID)}
	}
	return out
}

// errNoControlPlaneEntry and checkControlPlaneEntries pin wardynd's own admin entry to wardynd: an
// admin SVID can mint any SVID in the trust domain, and every proxy mounts the Workload API socket.
var errNoControlPlaneEntry = errors.New("spire identity: the SPIRE server has no entry for wardynd's control-plane SPIFFE ID")

func checkControlPlaneEntries(entries []Entry, namespace, podSA string) error {
	if len(entries) == 0 {
		return errNoControlPlaneEntry
	}
	for _, e := range entries {
		var ns, sa []string
		for _, s := range e.Selectors {
			if s.Type != "k8s" {
				return fmt.Errorf("spire identity: control-plane entry %s has selector %s, not a k8s one", e.ID, s)
			}
			switch {
			case strings.HasPrefix(s.Value, selNSPrefix):
				ns = append(ns, strings.TrimPrefix(s.Value, selNSPrefix))
			case strings.HasPrefix(s.Value, selSAPrefix):
				sa = append(sa, strings.TrimPrefix(s.Value, selSAPrefix))
			}
		}
		if len(ns) != 1 || ns[0] != namespace {
			return fmt.Errorf("spire identity: control-plane entry %s must select exactly k8s:ns:%s (wardynd's namespace), has %q", e.ID, namespace, ns)
		}
		if len(sa) != 1 || sa[0] != podSA {
			return fmt.Errorf("spire identity: control-plane entry %s must select exactly k8s:sa:%s (%s), has %q", e.ID, podSA, envPodSA, sa)
		}
	}
	return nil
}
