// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

// Package k8s implements the runner/substrate.Substrate contract against the
// Kubernetes API: an L1 (packet-filter) confinement substrate alongside the
// docker package's L0 (structural, no-default-route) substrate. This
// substrate proves confinement with a NetworkPolicy default-denying the
// agent's egress except the wardyn-proxy sidecar — and since a NetworkPolicy
// object existing is not proof it's enforced, a boot-time two-phase canary
// (canary.go) confirms the deny actually takes effect before advertising any
// Confinement Class. See internal/runner/substrate's package doc for the full
// L0-vs-L1 contract.
//
// Sandbox shape: one Secret (proxy config JSON — secretKeyRef is the only
// safe place for it, since pod specs are API-readable), two NetworkPolicies
// (agent and proxy, created before any pod exists), a proxy pod, and an agent
// pod whose main container idles until Exec adds an ephemeral container to
// run the real task — ephemeral containers are add-only, so unlike docker's
// re-execable `docker exec`, Exec here is one-shot per sandbox (see exec.go).
package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// handlerRunscPrefix is the gVisor OCI-runtime-handler family name, the same
// floor guard the docker driver applies to its CC2 probe. A RuntimeClass's
// .Handler — not its arbitrary operator-chosen object Name — says which
// low-level runtime it invokes.
const handlerRunscPrefix = "runsc"

// Config configures the k8s Driver. Mirrors the docker driver's Config where
// concepts overlap; Namespace/ImagePullSecret/AllowUnenforcedNetPol are new.
type Config struct {
	// Namespace is the k8s namespace every sandbox is created in. Resolved by
	// register.go's resolveNamespace before reaching here; New defaults an
	// empty value to "default" defensively.
	Namespace string
	// ProxyImage is the wardyn-proxy sidecar image — ALSO the image the
	// boot-time egress canary launches (with -egress-canary).
	ProxyImage string
	// ImagePullSecret optionally names a pre-existing Secret (imagePullSecrets)
	// threaded onto every pod this substrate creates.
	ImagePullSecret string
	// Record mirrors the docker driver's Config.Record: when true, Exec wraps
	// the agent argv with wardyn-rec, delivering via the masked brokered
	// proxy upload; when false, Exec runs argv unwrapped. Images always ship
	// wardyn-rec; this only gates whether Exec invokes it.
	Record bool
	// ConfinementRuntimes maps a Confinement Class to the RuntimeClass NAME
	// that enforces it — WARDYN_CONFINEMENT_MAP's k8s form. Unlike docker's
	// stable runtime family names, a RuntimeClass object name carries no
	// platform convention Wardyn can safely guess, so CC2/CC3 are advertised
	// ONLY when explicitly pinned here. May be nil.
	ConfinementRuntimes map[types.ConfinementClass]string
	// AllowUnenforcedNetPol is WARDYN_K8S_ALLOW_UNENFORCED_NETPOL: downgrades
	// a canary-proven unenforced NetworkPolicy from a fail-closed boot refusal
	// to a loud warning. ClassSupport.NetworkPolicy/StructuralEgress still
	// stay false — an opted-out substrate must never read as confined.
	AllowUnenforcedNetPol bool
	// AckAmbientDefaultDeny is WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY: on a
	// managed cluster where a platform team already applies a baseline
	// default-deny NetworkPolicy, the canary's phase A (which applies no
	// policy of its own) fails in exactly that shape and construction
	// otherwise refuses to boot. This flag acknowledges that shape as
	// expected rather than a Wardyn misconfiguration. Never proof of
	// enforcement: ClassSupport.NetworkPolicy stays false either way (see
	// NetworkPolicyAcknowledged instead).
	AckAmbientDefaultDeny bool
	// SandboxPlacement is WARDYN_K8S_SANDBOX_PLACEMENT, the JSON form of Placement (placement.go):
	// node selector, tolerations, affinity, PriorityClass, annotations and labels for the agent,
	// proxy and canary pods. Empty places nothing. An invalid value refuses to boot.
	SandboxPlacement string
	// StartTimeout is WARDYN_SANDBOX_START_TIMEOUT: the absolute budget for a sandbox's proxy and agent
	// pods to start. Zero means runner.DefaultSandboxStartTimeout.
	StartTimeout time.Duration
	// CapacityWait is WARDYN_SANDBOX_CAPACITY_WAIT: how long a pod the scheduler has no room for waits,
	// on top of StartTimeout. Zero turns the wait off: such a pod fails at StartTimeout.
	CapacityWait time.Duration
	// RunMaxAge is WARDYN_RUN_MAX_AGE. When positive, both run pods get
	// activeDeadlineSeconds = RunMaxAge + podDeadlineGrace, so a run whose control plane has
	// gone away still fails its pods on a bounded schedule. Zero (the default) sets no deadline.
	RunMaxAge time.Duration
	// ReadNodes is WARDYN_K8S_READ_NODES: list the cluster's nodes (a ClusterRole the chart grants only
	// under k8s.readNodes) so preflight and create can warn when no node a run may be placed on is large
	// enough. Off by default: nodes are cluster-scoped and a shared-cluster tenant may not be granted them.
	ReadNodes bool
}

func (c *Config) withDefaults() {
	if c.StartTimeout <= 0 {
		c.StartTimeout = runner.DefaultSandboxStartTimeout
	}
	if c.Namespace == "" {
		c.Namespace = "default"
	}
}

// Driver implements substrate.Substrate against the Kubernetes API.
type Driver struct {
	clientset  kubernetes.Interface
	restConfig *rest.Config
	cfg        Config
	// placement is cfg.SandboxPlacement, parsed and validated once at construction.
	placement Placement
	// nodeCache is the last node list, shared by every fit check (fit_nodes.go).
	nodeCache nodeCache

	// apiserverHostPort is resolved once at construction from restConfig.Host
	// — the egress canary's dial target.
	apiserverHostPort string

	// netPolEnforced is the boot-time canary's verdict, fixed at construction
	// (re-running per Classes()/healthz call would create pods on every poll).
	// netPolOptedOut records WHY it may be false despite success
	// (AllowUnenforcedNetPol), diagnostics only.
	netPolEnforced bool
	netPolOptedOut bool
	// netPolAcked: the operator acknowledged an ambient-default-deny-shaped
	// phase-A failure (AckAmbientDefaultDeny) rather than the canary proving
	// enforcement. Deliberately SEPARATE from netPolEnforced — an acknowledged
	// risk is not proof, and ClassSupport.NetworkPolicy must never overclaim.
	// Surfaced as ClassSupport.NetworkPolicyAcknowledged instead.
	netPolAcked bool

	// execFactory is the test seam newExecutor (session.go) defers to when
	// set: replaces the real SPDY/WebSocket-fallback executor (which dials
	// the apiserver over HTTP and can't run against a fake clientset) with a
	// caller-supplied remotecommand.Executor. Nil in production.
	execFactory func(podName, container string, cmd []string, stdin, tty bool) (remotecommand.Executor, error)

	// metricsRead is the test seam readPodMetrics defers to when set: the real
	// read is a raw GET that a fake clientset cannot serve. Nil in production.
	metricsRead func(ctx context.Context, namespace, labelSelector string) ([]byte, error)

	// execOutputs maps a sandbox ref to its SandboxSpec.ExecOutput (an
	// io.Writer), which Exec streams the agent container's log into. The one
	// in-memory state here: the buffer it feeds is in memory too, so a
	// restarted wardynd has neither.
	execOutputs sync.Map
}

var _ substrate.Substrate = (*Driver)(nil)
var _ runner.ActivitySampler = (*Driver)(nil)

// New constructs a Driver against the cluster client-go's standard config
// loading resolves (in-cluster, else kubeconfig), running the boot-time
// egress canary before returning. Fails closed: an indeterminate canary, or
// one proving NetworkPolicy unenforced without AllowUnenforcedNetPol, refuses
// construction entirely (see canary.go).
func New(cfg Config) (*Driver, error) {
	restCfg, err := loadRestConfig()
	if err != nil {
		return nil, err
	}
	cs, err := newClientset(restCfg)
	if err != nil {
		return nil, err
	}
	return newWithClient(context.Background(), cs, restCfg, cfg)
}

// clientQPS and clientBurst replace client-go's 5/10 default, which one
// readiness poll consumes on its own: two overlapping starts would starve
// every other pod and Secret call. Fixed, not configurable — nothing sets a knob.
// ponytail: a shared pod informer replaces the Get polls if ~45 concurrent
// task runs per replica ever saturate this.
const (
	clientQPS   = 50
	clientBurst = 100
)

// newClientset builds the clientset New uses, with the rate limit above.
func newClientset(restCfg *rest.Config) (*kubernetes.Clientset, error) {
	restCfg.QPS, restCfg.Burst = clientQPS, clientBurst
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: build clientset: %w", err)
	}
	return cs, nil
}

// newWithClient is the seam unit tests use to inject a fake clientset (and a
// synthetic rest.Config — only its Host field matters, for the canary's
// dial target).
func newWithClient(ctx context.Context, cs kubernetes.Interface, restCfg *rest.Config, cfg Config) (*Driver, error) {
	cfg.withDefaults()
	if cfg.ProxyImage == "" {
		return nil, errProxyImageUnset
	}
	hostPort, err := apiserverHostPort(restCfg)
	if err != nil {
		return nil, err
	}
	placement, err := parsePlacement(cfg.SandboxPlacement)
	if err != nil {
		return nil, err
	}
	d := &Driver{clientset: cs, restConfig: restCfg, cfg: cfg, placement: placement, apiserverHostPort: hostPort}

	verdict, cerr := d.runEgressCanary(ctx)
	switch verdict {
	case canaryIndeterminate:
		return nil, fmt.Errorf("k8s: refusing to boot: %w", cerr)
	case canaryUnenforced:
		if !cfg.AllowUnenforcedNetPol {
			return nil, fmt.Errorf("k8s: refusing to boot: %w (set WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1 to override on a TRUSTED cluster)", errNetworkPolicyUnenforced)
		}
		logWarnUnenforcedNetPolOptOut()
		d.netPolOptedOut = true
	case canaryEnforced:
		d.netPolEnforced = true
	case canaryAcknowledged:
		logWarnAckAmbientDefaultDeny()
		d.netPolAcked = true
	}
	return d, nil
}

// logWarnUnenforcedNetPolOptOut is the loud, unmissable warning the
// WARDYN_K8S_ALLOW_UNENFORCED_NETPOL opt-out must emit at construction: an
// operator who set the override cannot miss that every sandbox this Driver
// creates runs with UNCONFINED egress until it is unset.
func logWarnUnenforcedNetPolOptOut() {
	msg := "wardynd: k8s NetworkPolicy is NOT enforced on this cluster (the boot-time egress canary connected despite a deny-all policy) and WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1 — proceeding anyway: " +
		"every sandbox this substrate creates has UNCONFINED egress, not the L1 confinement Wardyn normally requires. Classes stay advertised, but NetworkPolicy and StructuralEgress both report false — " +
		"this substrate will never read as confined on /healthz. Unset WARDYN_K8S_ALLOW_UNENFORCED_NETPOL and fix the cluster's CNI/NetworkPolicy support to restore real confinement."
	slog.Warn(msg)
}

// logWarnAckAmbientDefaultDeny is the loud, unmissable warning when
// WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY=1 let boot proceed past a phase-A
// failure shaped like an ambient default-deny NetworkPolicy: the canary never
// proved Wardyn's own policy is enforced (phase B never ran), only that the
// operator accepted that as this cluster's expected baseline. Mirrors
// logWarnUnenforcedNetPolOptOut's contract — an acknowledgment, not proof.
func logWarnAckAmbientDefaultDeny() {
	msg := "wardynd: k8s egress canary phase A failed in exactly the shape an ambient (platform-applied) default-deny NetworkPolicy " +
		"produces, and WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY=1 — proceeding anyway: phase B (the deny-all test that would actually prove " +
		"Wardyn's own NetworkPolicy is enforced) was SKIPPED, because behind an existing ambient deny it could only ever also refuse, " +
		"proving nothing. Classes stay advertised, but NetworkPolicy still reports false and NetworkPolicyAcknowledged reports true — " +
		"this substrate reads as acknowledged-not-proven on the setup checklist's k8s egress-containment row (runner capabilities: network_policy=false, network_policy_acknowledged=true), never as confirmed confined. To get real " +
		"proof, exempt Wardyn's own pods (wardyn.managed=true) from the platform's ambient policy and unset this env."
	slog.Warn(msg)
}

func (d *Driver) Name() string { return driverName }

// Classes reports the Confinement Classes this substrate can enforce. CC1 is
// unconditional: reaching this point already proves the canary passed or the
// operator opted out (see New). CC2/CC3 each require an explicit
// WARDYN_CONFINEMENT_MAP pin resolving to a RuntimeClass that exists and
// whose .Handler passes the class's floor guard.
func (d *Driver) Classes(ctx context.Context) (substrate.ClassSupport, error) {
	classes := []types.ConfinementClass{types.CC1}
	resolved := map[types.ConfinementClass]string{types.CC1: "k8s/(default)"}

	if name := d.cfg.ConfinementRuntimes[types.CC2]; name != "" {
		handler, err := d.runtimeClassHandler(ctx, name)
		switch {
		case err != nil:
			return substrate.ClassSupport{}, fmt.Errorf("k8s: CC2 RuntimeClass %q: %w", name, err)
		case handler != "" && strings.HasPrefix(handler, handlerRunscPrefix):
			classes = append(classes, types.CC2)
			resolved[types.CC2] = "k8s/" + handler
		}
	}
	if name := d.cfg.ConfinementRuntimes[types.CC3]; name != "" {
		handler, err := d.runtimeClassHandler(ctx, name)
		switch {
		case err != nil:
			return substrate.ClassSupport{}, fmt.Errorf("k8s: CC3 RuntimeClass %q: %w", name, err)
		case handler != "" && !runner.IsKnownNonVaultRuntime(handler):
			classes = append(classes, types.CC3)
			resolved[types.CC3] = "k8s/" + handler
		}
	}

	return substrate.ClassSupport{
		Classes:          classes,
		Resolved:         resolved,
		StructuralEgress: false, // this substrate proves L1, never L0 (see package doc)
		NetworkPolicy:    d.netPolEnforced,
		// The operator accepted an ambient-default-deny-shaped canary failure
		// rather than the canary proving enforcement (netPolAcked). Never true
		// at the same time as NetworkPolicy — mutually exclusive canary outcomes.
		NetworkPolicyAcknowledged: d.netPolAcked,
		// Exec (exec.go's recordCmd) wraps the ephemeral-container argv with
		// wardyn-rec via the masked brokered proxy upload — the only path
		// this substrate supports, since mounts (and shared-volume delivery)
		// are impossible on k8s — but only when Config.Record is on, so a
		// Record=false substrate never advertises an upload it won't perform.
		SessionRecording: d.cfg.Record,
		// Under the recorder the exec's container log is empty: wardyn-rec runs
		// asciinema, which writes nothing to a stdout that is not a TTY, so
		// followExecOutput would copy nothing and the run would read as a
		// complete, empty capture. The output is in the run's recording.
		ExecOutputUncaptured: d.cfg.Record,
		// This substrate binds a member's drive: ensureDrivePVC creates/adopts
		// the claim and applyDriveToPod attaches it. True only while that path
		// exists — TestCreateSandbox_MountsAUserDrive pins the two together,
		// since declaring it without the mount previews green and fails at dispatch.
		UserDrives: true,
		// Delivers a root-owned managed file: managedFileVolumes projects each
		// one out of the per-run Secret as a read-only kubelet tmpfs mount
		// point, in place and unreplaceable before any container starts. True
		// only while that path exists, for UserDrives' reason.
		ManagedFiles: true,
		// A run's disk_mib becomes the agent container's
		// resources.limits[ephemeral-storage]; the kubelet enforces it by
		// EVICTING the pod (measured periodically; the write is never
		// refused). Not `filesystem` — nothing here binds a byte.
		//
		// That limit alone binds the wrong container: the agent runs in an
		// ephemeral container the kubelet doesn't meter, so the budget sits on
		// an idle main container nothing writes in. ephemeralScratchVolumes
		// NARROWS that with two emptyDirs (/tmp, workdir) carrying disk_mib as
		// sizeLimit. A narrowing, not a close: what the agent writes elsewhere
		// (rest of $HOME, toolchain caches, any authored target outside the
		// workdir) is still on the unmetered layer. `eviction` here means
		// those two paths, not every byte the agent writes.
		EphemeralDiskEnforcement: types.StorageEnforcementEviction,
	}, nil
}

// runtimeClassHandler resolves a RuntimeClass's .Handler by name, treating
// "not found" as "" (no error) so Classes can fail-closed-omit that class
// rather than fail the whole call.
func (d *Driver) runtimeClassHandler(ctx context.Context, name string) (string, error) {
	rc, err := d.clientset.NodeV1().RuntimeClasses().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return rc.Handler, nil
}
