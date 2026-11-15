// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package k8s

// Warm pools: idle placeholder pods that hold room on a node with a pool's agent image already pulled. A run that fits a
// pool removes one placeholder and prefers its node; the run's own pods are still made fresh and checked. No build tag:
// these helpers are pure and tested without a cluster.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// WarmPool is one entry of WARDYN_K8S_WARM_POOLS.
type WarmPool struct {
	Name   string `json:"name"`   // a DNS label of at most warmPoolNameMax characters, unique
	Image  string `json:"image"`  // compared with a run's image as an exact string
	Class  string `json:"class"`  // CC1, CC2 or CC3
	CPU    string `json:"cpu"`    // the agent CPU request a placeholder reserves, a quantity above zero
	Memory string `json:"memory"` // the agent memory request a placeholder reserves, a quantity above zero
	Size   int    `json:"size"`   // placeholders to keep, 0 to warmPoolMaxSize
}

// warmPoolWire is WarmPool as WARDYN_K8S_WARM_POOLS writes it: cpu and memory may be a JSON number or a JSON string,
// since a chart value `cpu: 1` renders as the number 1.
type warmPoolWire struct {
	Name   string       `json:"name"`
	Image  string       `json:"image"`
	Class  string       `json:"class"`
	CPU    warmQuantity `json:"cpu"`
	Memory warmQuantity `json:"memory"`
	Size   int          `json:"size"`
}

// warmQuantity reads a JSON string as is and a JSON number by its literal text. Any other JSON value is kept in raw
// with bad set, so parseWarmPools can refuse it by pool and field.
type warmQuantity struct {
	text string
	raw  string
	bad  bool
}

func (q *warmQuantity) UnmarshalJSON(b []byte) error {
	switch {
	case len(b) > 0 && b[0] == '"':
		return json.Unmarshal(b, &q.text)
	case len(b) > 0 && (b[0] == '-' || (b[0] >= '0' && b[0] <= '9')):
		q.text = string(b)
	default:
		q.raw, q.bad = string(b), true
	}
	return nil
}

const (
	warmPoolMaxSize  = 20
	warmPoolMaxTotal = 50
	warmPoolNameMax  = 30
	labelWarmPool    = "wardyn.warm-pool"
	componentWarm    = "warm"
	warmDenyPolicy   = "wardyn-warm-deny"
)

const warmPoolsRefusal = "k8s: refusing to boot: WARDYN_K8S_WARM_POOLS: "

var warmPoolNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// parseWarmPools reads WARDYN_K8S_WARM_POOLS: empty is no pools, anything malformed is a boot refusal.
func parseWarmPools(raw string) ([]WarmPool, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var wire []warmPoolWire
	if err := dec.Decode(&wire); err != nil {
		return nil, fmt.Errorf("%scannot read the JSON array of pools: %w", warmPoolsRefusal, err)
	}
	// only white space may follow the array; More alone would pass a stray "]" or "}"
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%sdata after the array of pools", warmPoolsRefusal)
	}
	pools := make([]WarmPool, 0, len(wire))
	for _, w := range wire {
		for _, f := range []struct {
			field string
			q     warmQuantity
		}{{"cpu", w.CPU}, {"memory", w.Memory}} {
			if f.q.bad {
				return nil, fmt.Errorf("%spool %q: %s must be a number or a string, not %s", warmPoolsRefusal, w.Name, f.field, f.q.raw)
			}
		}
		pools = append(pools, WarmPool{Name: w.Name, Image: w.Image, Class: w.Class, CPU: w.CPU.text, Memory: w.Memory.text, Size: w.Size})
	}
	seen := map[string]bool{}
	total := 0
	for _, p := range pools {
		if err := checkWarmPool(p, seen); err != nil {
			return nil, err
		}
		total += p.Size
	}
	if total > warmPoolMaxTotal {
		return nil, fmt.Errorf("%sthe sizes add up to %d, above %d", warmPoolsRefusal, total, warmPoolMaxTotal)
	}
	return pools, nil
}

// checkWarmPool judges one pool and records its name in seen.
func checkWarmPool(p WarmPool, seen map[string]bool) error {
	switch {
	case !warmPoolNameRE.MatchString(p.Name) || len(p.Name) > warmPoolNameMax:
		return fmt.Errorf("%spool %q: name must be a DNS label of at most %d characters", warmPoolsRefusal, p.Name, warmPoolNameMax)
	case seen[p.Name]:
		return fmt.Errorf("%spool %q: name is repeated", warmPoolsRefusal, p.Name)
	case p.Image == "" || strings.ContainsAny(p.Image, " \t\r\n\v\f"):
		return fmt.Errorf("%spool %q: image must be set and hold no whitespace", warmPoolsRefusal, p.Name)
	case p.Class != string(types.CC1) && p.Class != string(types.CC2) && p.Class != string(types.CC3):
		return fmt.Errorf("%spool %q: class must be CC1, CC2 or CC3, not %q", warmPoolsRefusal, p.Name, p.Class)
	case p.Size < 0 || p.Size > warmPoolMaxSize:
		return fmt.Errorf("%spool %q: size must be 0 to %d, not %d", warmPoolsRefusal, p.Name, warmPoolMaxSize, p.Size)
	}
	for _, f := range [][2]string{{"cpu", p.CPU}, {"memory", p.Memory}} {
		q, err := resource.ParseQuantity(f[1])
		if err != nil || q.Sign() <= 0 {
			return fmt.Errorf("%spool %q: %s must be a quantity above zero, not %q", warmPoolsRefusal, p.Name, f[0], f[1])
		}
	}
	seen[p.Name] = true
	return nil
}

// checkWarmPoolClasses refuses a CC2 or CC3 pool whose RuntimeClass is not pinned.
func checkWarmPoolClasses(pools []WarmPool, runtimes map[types.ConfinementClass]string) error {
	for _, p := range pools {
		class := types.ConfinementClass(p.Class)
		if (class == types.CC2 || class == types.CC3) && runtimes[class] == "" {
			return fmt.Errorf("%spool %q is %s, and no RuntimeClass is pinned for it", warmPoolsRefusal, p.Name, p.Class)
		}
	}
	return nil
}

func warmPodName(pool string, i int) string { return fmt.Sprintf("wardyn-warm-%s-%d", pool, i) }

// warmIndex is the i of a name warmPodName made for pool, written without leading zeros.
func warmIndex(name, pool string) (int, bool) {
	rest, ok := strings.CutPrefix(name, "wardyn-warm-"+pool+"-")
	if !ok || rest == "" || (len(rest) > 1 && rest[0] == '0') {
		return 0, false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	i, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return i, true
}

// lowestFreeIndex is the smallest i >= 0 not in used.
func lowestFreeIndex(used []int) int {
	taken := make(map[int]bool, len(used))
	for _, i := range used {
		taken[i] = true
	}
	i := 0
	for taken[i] {
		i++
	}
	return i
}

// matchWarmPool is the first pool with room to keep, the run's image and class, and requests that fit one placeholder.
func matchWarmPool(pools []WarmPool, image string, class types.ConfinementClass, cpu, mem resource.Quantity) (WarmPool, bool) {
	if class == "" {
		class = types.CC1
	}
	for _, p := range pools {
		if p.Size <= 0 || p.Image != image || p.Class != string(class) {
			continue
		}
		poolCPU, poolMem := resource.MustParse(p.CPU), resource.MustParse(p.Memory) // validated at boot
		if cpu.Cmp(poolCPU) <= 0 && mem.Cmp(poolMem) <= 0 {
			return p, true
		}
	}
	return WarmPool{}, false
}

// preferNode asks the scheduler for node without requiring it: the placement's required terms still decide where a run may go.
func preferNode(pod *corev1.Pod, node string) {
	if node == "" {
		return
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	na := pod.Spec.Affinity.NodeAffinity
	na.PreferredDuringSchedulingIgnoredDuringExecution = append(na.PreferredDuringSchedulingIgnoredDuringExecution,
		corev1.PreferredSchedulingTerm{Weight: 100, Preference: corev1.NodeSelectorTerm{MatchFields: []corev1.NodeSelectorRequirement{
			{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}},
		}}})
}

// placeholderHoldsNothing refuses a placeholder that carries anything of a run, or that runs anything but the pool's
// image in its own namespaces: it holds room only, and is deleted before the run's own objects exist. image is the
// pool's image.
func placeholderHoldsNothing(pod *corev1.Pod, image string) error {
	s := &pod.Spec
	switch {
	case len(s.Volumes) > 0:
		return fmt.Errorf("placeholder %s has a volume", pod.Name)
	case len(s.InitContainers) > 0:
		return fmt.Errorf("placeholder %s has an init container", pod.Name)
	case len(s.EphemeralContainers) > 0:
		return fmt.Errorf("placeholder %s has an ephemeral container", pod.Name)
	case len(s.Containers) != 1:
		return fmt.Errorf("placeholder %s has %d containers, not one", pod.Name, len(s.Containers))
	case s.HostNetwork || s.HostPID || s.HostIPC:
		// a host namespace puts the pod beside the node's own network or processes, outside the deny-all policy
		return fmt.Errorf("placeholder %s shares a host namespace", pod.Name)
	case s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken:
		return fmt.Errorf("placeholder %s may mount a service account token", pod.Name)
	case s.EnableServiceLinks == nil || *s.EnableServiceLinks:
		return fmt.Errorf("placeholder %s takes service link env", pod.Name)
	}
	c := &s.Containers[0]
	switch {
	case c.Name != "warm": // the one container warmPlaceholderPod makes
		return fmt.Errorf("placeholder %s has container %q, not warm", pod.Name, c.Name)
	case c.Image != image:
		return fmt.Errorf("placeholder %s runs image %q, not the pool's %q", pod.Name, c.Image, image)
	case len(c.Env) > 0 || len(c.EnvFrom) > 0 || len(c.VolumeMounts) > 0:
		return fmt.Errorf("placeholder %s has env, envFrom or a volume mount", pod.Name)
	}
	if _, ok := pod.Labels["wardyn.run-id"]; ok {
		return fmt.Errorf("placeholder %s has a run label", pod.Name)
	}
	return nil
}
