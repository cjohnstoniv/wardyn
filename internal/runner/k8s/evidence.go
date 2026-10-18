// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// Completion evidence for a k8s runner's pods (design K1 §3.1, R4-M2, R6-M1, R7-M1, R8-M1, R8-M2,
// owner #8). A runner never trusts a delete: NotFound, a force delete, an eviction, a deleted Node,
// the pod garbage collector's Failed, or a passed deadline is never evidence that a pod's processes
// stopped. This file holds the record a run keeps in its offline state and the pure checks of each
// arm; identity.go does the reads, stop_proxy.go the witness, finalizer and teardown order.

// stopProofFinalizer holds a runner-mode pod object until runnerd has read its kubelet's terminal
// status: the API keeps an object with finalizers, marked for deletion, even through a force delete.
const stopProofFinalizer = "wardyn.dev/stop-proof"

// reasonDeletionByPodGC is the DisruptionTarget reason the pod garbage collector sets when it marks
// a pod Failed without any kubelet, before deleting the pod of a Node that is gone.
const reasonDeletionByPodGC = "DeletionByPodGC"

// assoc is how a pod's node binding was associated with a Node UID by bracketing reads.
type assoc string

const (
	assocCausal assoc = "causal" // the same UID held N in the before-list and the after-list
	assocU1     assoc = "u1"     // no Node in scope held N in the before-list
	assocU2     assoc = "u2"     // a different UID held N in the before-list: a replacement
	assocU3     assoc = "u3"     // no before-list at all
)

// nodeMap is a scoped node list: name to UID, with the list's resourceVersion.
type nodeMap struct {
	Version string            `json:"version"`
	Names   map[string]string `json:"names"`
}

func (m *nodeMap) sameNames(o *nodeMap) bool {
	if m == nil || o == nil || len(m.Names) != len(o.Names) {
		return false
	}
	for k, v := range m.Names {
		if o.Names[k] != v {
			return false
		}
	}
	return true
}

// nodeHolder is what one name-scoped node list showed: no Node, or one Node with its identity.
type nodeHolder struct {
	None         bool              `json:"none,omitempty"`
	Node         runner.NodeSample `json:"node"`
	OutOfService bool              `json:"out_of_service,omitempty"`
	At           time.Time         `json:"at"`
}

// containerIDs are one container's recorded runtime IDs: the first non-empty ID ever read is the
// baseline, written once; any later different ID is latched beside it, never replacing it.
type containerIDs struct {
	Baseline string   `json:"baseline"`
	Anchored bool     `json:"anchored,omitempty"`
	Latched  []string `json:"latched,omitempty"`
}

// witnessContainer is one container's terminal state as the witness read it.
type witnessContainer struct {
	Name        string    `json:"name"`
	ExitCode    int32     `json:"exit_code"`
	Reason      string    `json:"reason,omitempty"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	ContainerID string    `json:"container_id,omitempty"`
	Tier        string    `json:"tier,omitempty"`
}

// Arms a witness can record.
const (
	armTerminal  = "terminal"
	armNeverInit = "never_initialized"
	armUnbound   = "unbound_deleted"
	armTaint     = "taint"
	armRecord    = "record"
	tier1        = "tier1"
	tier2        = "tier2"
)

// witness is runnerd's own copy of the evidence a held object showed, persisted before the
// finalizer is removed (R7-M1). A persisted witness is completion evidence for its pod UID.
type witness struct {
	RunID           uuid.UUID          `json:"run_id"`
	PodUID          string             `json:"pod_uid"`
	NodeName        string             `json:"node_name,omitempty"`
	ResourceVersion string             `json:"resource_version,omitempty"`
	Arm             string             `json:"arm"`
	Phase           string             `json:"phase,omitempty"`
	Containers      []witnessContainer `json:"containers,omitempty"`
	// PostStatus is what the tier's post-status list showed, stored as None when it showed no Node.
	PostStatus *nodeHolder `json:"post_status,omitempty"`
	// Fence arms: the causal UID only when one was recorded (absent otherwise), the taint
	// observation, or the record with its bound tuple.
	CausalUID   string            `json:"causal_uid,omitempty"`
	Taint       *nodeHolder       `json:"taint,omitempty"`
	RecordID    uuid.UUID         `json:"record_id,omitempty"`
	Attestation string            `json:"attestation,omitempty"`
	Tuple       *runner.NodeTuple `json:"tuple,omitempty"`
	At          time.Time         `json:"at"`
}

// podEvidence is one pod's recorded identity and evidence.
type podEvidence struct {
	Name     string `json:"name"`
	UID      string `json:"uid,omitempty"`
	NodeName string `json:"node_name,omitempty"`
	PodIP    string `json:"pod_ip,omitempty"`
	HostIP   string `json:"host_ip,omitempty"`
	// Before is the latest scoped node map an unbound read of this pod followed, rewritten only when
	// its content changes; persisted before the create request when the create is the lower end.
	Before          *nodeMap                 `json:"before,omitempty"`
	Assoc           assoc                    `json:"assoc,omitempty"`
	PostBind        *nodeHolder              `json:"post_bind,omitempty"`
	BeforeHolderUID string                   `json:"before_holder_uid,omitempty"`
	Containers      map[string]*containerIDs `json:"containers,omitempty"`
	DeleteAt        *time.Time               `json:"delete_at,omitempty"`
	Closing         *nodeHolder              `json:"closing,omitempty"`
	Witness         *witness                 `json:"witness,omitempty"`
	FinalizerOff    bool                     `json:"finalizer_off,omitempty"`
}

// relayFence is the durable per-execution relay fence (R4-M1): written before runnerd's first delete
// of either pod, replayed into the relay before Online, dropped only with the run's offline state.
type relayFence struct {
	ProxyUID string    `json:"proxy_uid,omitempty"`
	ProxyIP  string    `json:"proxy_ip,omitempty"`
	At       time.Time `json:"at"`
}

// runEvidence is one run's k8s record, kept in its offline record by the manager's atomic write.
type runEvidence struct {
	Pods  map[string]*podEvidence `json:"pods,omitempty"`
	Fence *relayFence             `json:"fence,omitempty"`
	// Reuse holds each reuse generation (owner #8): never cleared, each new UID or new
	// (machineID, systemUUID) pair under a name latches one.
	Reuse  []runner.NodeSample `json:"reuse,omitempty"`
	Record *runner.NodeRecord  `json:"record,omitempty"`
	// Hold marks a run that entered pending_delete: its NetworkPolicies stay until completion evidence
	// for both pods.
	Hold bool `json:"hold,omitempty"`
	// Lapsed is a validated lapsed frame from the run's proxy: its egress stopped. The finalizer,
	// the NetworkPolicies and the producer's loss still wait for terminal status or a fence.
	Lapsed       bool       `json:"lapsed,omitempty"`
	PendingSince *time.Time `json:"pending_since,omitempty"`
	// SinkKeyHash is the hex sha256 of the run's decision sink key, re-registered at startup.
	SinkKeyHash string `json:"sink_key_hash,omitempty"`
	// ProducerGone marks the sink producer settled on the proxy's completion evidence.
	ProducerGone bool `json:"producer_gone,omitempty"`
}

func decodeEvidence(b []byte) (*runEvidence, error) {
	ev := &runEvidence{}
	if len(b) > 0 {
		if err := json.Unmarshal(b, ev); err != nil {
			return nil, err
		}
	}
	if ev.Pods == nil {
		ev.Pods = map[string]*podEvidence{}
	}
	return ev, nil
}

func (ev *runEvidence) pod(component, name string) *podEvidence {
	pe := ev.Pods[component]
	if pe == nil {
		pe = &podEvidence{Name: name}
		ev.Pods[component] = pe
	}
	return pe
}

// anchor is the Node a pod's baselines are anchored to: the causal UID on a causal run, the
// post-bind holder on an unknown one; nil when no Node held the name after binding.
func (pe *podEvidence) anchor() *runner.NodeSample {
	if pe.PostBind == nil || pe.PostBind.None {
		return nil
	}
	return &pe.PostBind.Node
}

func (pe *podEvidence) causalUID() string {
	if pe.Assoc != assocCausal || pe.anchor() == nil {
		return ""
	}
	return pe.anchor().UID
}

// sameMachine: both machine IDs non-empty and equal. InternalIP is not part of it (Fable r12 M4).
func sameMachine(a, b runner.NodeSample) bool {
	return a.MachineID != "" && a.SystemUUID != "" && a.MachineID == b.MachineID && a.SystemUUID == b.SystemUUID
}

// sameAnchorIdentity is the anchor check: machine IDs and InternalIP, all non-empty and equal.
func sameAnchorIdentity(a, b runner.NodeSample) bool {
	return sameMachine(a, b) && a.InternalIP != "" && a.InternalIP == b.InternalIP
}

// latchReuse records a name reuse that h shows for pe's node name, after binding: a UID other than
// the post-bind holder's, or the holder's UID with another machineID or systemUUID. It reports
// whether a NEW reuse generation was latched, which the caller persists before comparing any
// record with its tuple. A repeated pair, an InternalIP-only change or an empty list latch nothing.
func (ev *runEvidence) latchReuse(pe *podEvidence, h *nodeHolder) bool {
	if pe == nil || pe.PostBind == nil || h == nil || h.None {
		return false
	}
	pb := pe.PostBind
	if !pb.None && h.Node.UID == pb.Node.UID && h.Node.MachineID == pb.Node.MachineID && h.Node.SystemUUID == pb.Node.SystemUUID {
		return false
	}
	for _, s := range ev.Reuse {
		if s.Name == h.Node.Name && s.UID == h.Node.UID && s.MachineID == h.Node.MachineID && s.SystemUUID == h.Node.SystemUUID {
			return false
		}
	}
	ev.Reuse = append(ev.Reuse, h.Node)
	return true
}

// containerStatuses is every init and main container status; ephemeral containers are never
// evidence inputs (Fable r10 L2).
func containerStatuses(pod *corev1.Pod) []corev1.ContainerStatus {
	return slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses)
}

// recordIdentity records what a read of the pod shows: the pod IP and host IP when first seen, and
// each container's runtime ID as a baseline or a latch. It reports whether a new baseline appeared
// (the caller then sends an anchor list) and whether anything changed (the caller persists).
func (pe *podEvidence) recordIdentity(pod *corev1.Pod) (newBaseline, changed bool) {
	if pe.PodIP == "" && pod.Status.PodIP != "" {
		pe.PodIP, changed = pod.Status.PodIP, true
	}
	if pe.HostIP == "" && pod.Status.HostIP != "" {
		pe.HostIP, changed = pod.Status.HostIP, true
	}
	for _, cs := range containerStatuses(pod) {
		id := cs.ContainerID
		if id == "" && cs.State.Terminated != nil {
			id = cs.State.Terminated.ContainerID
		}
		if id == "" {
			continue
		}
		if pe.Containers == nil {
			pe.Containers = map[string]*containerIDs{}
		}
		c := pe.Containers[cs.Name]
		if c == nil {
			c = &containerIDs{}
			pe.Containers[cs.Name] = c
		}
		switch {
		case c.Baseline == "":
			c.Baseline = id
			newBaseline, changed = true, true
		case id != c.Baseline && !slices.Contains(c.Latched, id):
			c.Latched = append(c.Latched, id)
			changed = true
		}
	}
	return newBaseline, changed
}

// anchorBaselines marks every unanchored baseline anchored when h, a name-scoped list sent after the
// recording read returned, shows the anchor UID still holding the name with its recorded machineID,
// systemUUID and InternalIP, all non-empty (Fable r10 M1).
func (pe *podEvidence) anchorBaselines(h *nodeHolder) bool {
	a := pe.anchor()
	if a == nil || h == nil || h.None || h.Node.UID != a.UID || !sameAnchorIdentity(h.Node, *a) {
		return false
	}
	changed := false
	for _, c := range pe.Containers {
		if c.Baseline != "" && !c.Anchored {
			c.Anchored, changed = true, true
		}
	}
	return changed
}

// hasLatch reports any latched ID on any container of the pod.
func (pe *podEvidence) hasLatch() bool {
	for _, c := range pe.Containers {
		if len(c.Latched) > 0 {
			return true
		}
	}
	return false
}

func podGCMarked(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.DisruptionTarget && c.Reason == reasonDeletionByPodGC {
			return true
		}
	}
	return false
}

// specContainerNames is every init and main container the spec declares.
func specContainerNames(pod *corev1.Pod) (inits, mains []string) {
	for _, c := range pod.Spec.InitContainers {
		inits = append(inits, c.Name)
	}
	for _, c := range pod.Spec.Containers {
		mains = append(mains, c.Name)
	}
	return inits, mains
}

func statusByName(pod *corev1.Pod) map[string]corev1.ContainerStatus {
	m := map[string]corev1.ContainerStatus{}
	for _, cs := range containerStatuses(pod) {
		m[cs.Name] = cs
	}
	return m
}

// verdict is an arm's answer for one pod.
type verdict struct {
	arm   string
	tiers map[string]string
	why   string
}

func (v verdict) ok() bool { return v.arm != "" }

// tier2Node is tier 2's node check: a causal UID whose Node, on a name-scoped list sent after the
// status read returned, still holds the name with the machineID and systemUUID recorded at
// association, both non-empty, and a non-empty status.hostIP equal to that Node's InternalIP.
func (pe *podEvidence) tier2Node(pod *corev1.Pod, post *nodeHolder) bool {
	causal := pe.causalUID()
	return causal != "" && post != nil && !post.None && post.Node.UID == causal && sameMachine(post.Node, *pe.anchor()) &&
		pod.Status.HostIP != "" && pod.Status.HostIP == post.Node.InternalIP
}

// tier1Pod is tier 1's pod-level part: an association that is causal or (u1), the closing anchor on
// the anchor UID with its identity, and a post-status list showing the name held by the anchor UID
// with its recorded machineID and systemUUID, or by no Node (Fable r12 M2).
func (pe *podEvidence) tier1Pod(post *nodeHolder) bool {
	a := pe.anchor()
	if a == nil || (pe.Assoc != assocCausal && pe.Assoc != assocU1) {
		return false
	}
	if pe.Closing == nil || pe.Closing.None || pe.Closing.Node.UID != a.UID || !sameAnchorIdentity(pe.Closing.Node, *a) {
		return false
	}
	return post != nil && (post.None || (post.Node.UID == a.UID && sameMachine(post.Node, *a)))
}

// containerTier binds one terminated container: tier 1 when its terminated ID equals its anchored
// baseline and the pod-level tier-1 checks hold; tier 2 for a container with no anchored baseline
// or no terminated ID when the node checks hold. refuse marks a different execution: a terminated
// ID unlike the baseline, or any latch, refuses the whole status (R10-L2).
func (pe *podEvidence) containerTier(name string, t *corev1.ContainerStateTerminated, t1, t2 bool) (tier string, refuse bool) {
	c := pe.Containers[name]
	if c != nil && len(c.Latched) > 0 {
		return "", true
	}
	if t.ContainerID != "" && c != nil && c.Baseline != "" && t.ContainerID != c.Baseline {
		return "", true
	}
	anchored := c != nil && c.Anchored && c.Baseline != ""
	switch {
	case anchored && t.ContainerID != "" && t1:
		return tier1, false
	case (!anchored || t.ContainerID == "") && t2:
		return tier2, false
	}
	return "", false
}

// evaluate checks every arm for one pod against one status read and the name-scoped list sent
// after it returned (post). It reads nothing and never treats a missing pod as evidence.
func (ev *runEvidence) evaluate(pe *podEvidence, pod *corev1.Pod, post *nodeHolder) verdict {
	if pod == nil || string(pod.UID) != pe.UID || pe.UID == "" {
		return verdict{why: "no read of the recorded pod UID"}
	}
	if pod.Spec.NodeName == "" && pod.DeletionTimestamp != nil && pe.NodeName == "" {
		// The API refuses to bind a pod being deleted, and a deletionTimestamp is never unset.
		// Release-gated on K6's bind/delete race (design K1 §3.1 "Release gate").
		return verdict{arm: armUnbound}
	}
	if ev.Record != nil {
		// An accepted record releases the run only while the tuple it was approved for still holds:
		// a later list that latched another holder of the node name changed it, and the run stays
		// contained until a record for the new tuple is accepted.
		if !sameTuple(*ev.tuple(ev.Record.Tuple.RunID), ev.Record.Tuple) {
			return verdict{why: "the node record's approved tuple no longer matches the run's: a new record is needed"}
		}
		return verdict{arm: armRecord}
	}
	if len(ev.Reuse) > 0 {
		return verdict{why: "node name reused: only a two-person node powered off record releases the run"}
	}
	terminal := ev.terminalArm(pe, pod, post)
	if terminal.ok() {
		return terminal
	}
	if v := ev.neverInitializedArm(pe, pod, post); v.ok() {
		return v
	}
	// The taint is an infrastructure fence on the causal machine itself, so a latched ID (a second
	// execution) does not stop it: that machine is confirmed powered off either way.
	if causal := pe.causalUID(); causal != "" && post != nil && !post.None && post.Node.UID == causal && post.OutOfService && sameMachine(post.Node, *pe.anchor()) {
		return verdict{arm: armTaint}
	}
	if terminal.why != "" {
		return terminal
	}
	return verdict{why: "no arm holds: termination unknown"}
}

const whyDifferentExecution = "a container ID differs from its baseline or is latched: a different execution"

// terminalArm is arm (a): the kubelet's terminal status for the recorded pod UID, every container
// terminated, no DeletionByPodGC, and every container bound by tier 1 or tier 2.
func (ev *runEvidence) terminalArm(pe *podEvidence, pod *corev1.Pod, post *nodeHolder) verdict {
	if pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded {
		return verdict{}
	}
	if podGCMarked(pod) {
		return verdict{}
	}
	inits, mains := specContainerNames(pod)
	statuses := statusByName(pod)
	t1, t2 := pe.tier1Pod(post), pe.tier2Node(pod, post)
	tiers := map[string]string{}
	for _, name := range slices.Concat(inits, mains) {
		cs, ok := statuses[name]
		if !ok || cs.State.Terminated == nil {
			return verdict{}
		}
		tier, refuse := pe.containerTier(name, cs.State.Terminated, t1, t2)
		if refuse {
			return verdict{why: whyDifferentExecution}
		}
		if tier == "" {
			return verdict{}
		}
		tiers[name] = tier
	}
	return verdict{arm: armTerminal, tiers: tiers}
}

// neverInitialized reports the never-initialized arm's status conditions alone (Fable r10 H1, r11
// L3): Failed, deleting, not by the pod GC, Initialized=False present, a status for every spec
// container, every main container waiting with no baseline or latch, and every init container
// waiting with none or terminated. The node binding is checked separately.
func (pe *podEvidence) neverInitialized(pod *corev1.Pod) (terminatedInits []string, ok bool) {
	if pod.Status.Phase != corev1.PodFailed || pod.DeletionTimestamp == nil || podGCMarked(pod) {
		return nil, false
	}
	initialized := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodInitialized {
			if c.Status != corev1.ConditionFalse {
				return nil, false
			}
			initialized = true
		}
	}
	if !initialized {
		return nil, false
	}
	inits, mains := specContainerNames(pod)
	statuses := statusByName(pod)
	clean := func(name string) bool {
		c := pe.Containers[name]
		return c == nil || (c.Baseline == "" && len(c.Latched) == 0)
	}
	for _, name := range mains {
		cs, ok := statuses[name]
		if !ok || cs.State.Waiting == nil || !clean(name) {
			return nil, false
		}
	}
	for _, name := range inits {
		cs, ok := statuses[name]
		switch {
		case !ok:
			return nil, false
		case cs.State.Waiting != nil && clean(name):
		case cs.State.Terminated != nil:
			terminatedInits = append(terminatedInits, name)
		default:
			return nil, false
		}
	}
	return terminatedInits, true
}

// neverInitializedArm binds the never-initialized status to a node: tier 2's node checks on a causal
// run; on the no-association path only the accepted "no prior holder" record, which evaluate has
// already checked. A terminated init container must also be bound by tier 1 or the node binding.
func (ev *runEvidence) neverInitializedArm(pe *podEvidence, pod *corev1.Pod, post *nodeHolder) verdict {
	inits, ok := pe.neverInitialized(pod)
	if !ok || !pe.tier2Node(pod, post) {
		return verdict{}
	}
	statuses := statusByName(pod)
	t1 := pe.tier1Pod(post)
	tiers := map[string]string{}
	for _, name := range inits {
		tier, refuse := pe.containerTier(name, statuses[name].State.Terminated, t1, true)
		if refuse || tier == "" {
			return verdict{}
		}
		tiers[name] = tier
	}
	return verdict{arm: armNeverInit, tiers: tiers}
}

// newWitness builds the witness of a passing verdict from the status read it judged.
func newWitness(runID uuid.UUID, pe *podEvidence, pod *corev1.Pod, v verdict, post *nodeHolder, rec *runner.NodeRecord, now time.Time) *witness {
	w := &witness{RunID: runID, PodUID: pe.UID, NodeName: pe.NodeName, Arm: v.arm, At: now, CausalUID: pe.causalUID()}
	if pod != nil {
		w.ResourceVersion = pod.ResourceVersion
		w.Phase = string(pod.Status.Phase)
		for _, cs := range containerStatuses(pod) {
			if t := cs.State.Terminated; t != nil {
				w.Containers = append(w.Containers, witnessContainer{Name: cs.Name, ExitCode: t.ExitCode, Reason: t.Reason,
					FinishedAt: t.FinishedAt.Time.UTC(), ContainerID: t.ContainerID, Tier: v.tiers[cs.Name]})
			}
		}
	}
	switch v.arm {
	case armTerminal, armNeverInit:
		w.PostStatus = post
	case armTaint:
		w.Taint = post
	case armRecord:
		if rec != nil {
			w.RecordID, w.Attestation = rec.ID, rec.Attestation
			t := rec.Tuple
			w.Tuple = &t
		}
	}
	return w
}

// tuple is the run's current node tuple (Codex R9-L1, R14-L1): what an operator record must name.
func (ev *runEvidence) tuple(runID uuid.UUID) *runner.NodeTuple {
	t := &runner.NodeTuple{RunID: runID, Reuses: slices.Clone(ev.Reuse)}
	for _, comp := range []string{componentProxy, componentAgent} {
		pe := ev.Pods[comp]
		if pe == nil || pe.UID == "" {
			continue
		}
		tp := runner.NodeTuplePod{Component: comp, UID: pe.UID, NodeName: pe.NodeName, CausalUID: pe.causalUID(), BeforeHolderUID: pe.BeforeHolderUID}
		if a := pe.anchor(); a != nil {
			s := *a
			tp.PostBind = &s
		}
		if pe.Closing != nil && !pe.Closing.None && (pe.anchor() == nil || pe.Closing.Node.UID != pe.anchor().UID) {
			tp.ClosingHolderUID = pe.Closing.Node.UID
		}
		t.Pods = append(t.Pods, tp)
	}
	return t
}

// noCausalAssociation reports a run with a bound pod that has no causal node UID.
func (ev *runEvidence) noCausalAssociation() bool {
	for _, pe := range ev.Pods {
		if pe.UID != "" && pe.NodeName != "" && pe.causalUID() == "" {
			return true
		}
	}
	return false
}
