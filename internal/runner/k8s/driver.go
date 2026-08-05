// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

// Package k8s implements the runner/substrate.Substrate contract against the
// Kubernetes API: an L1 (packet-filter) confinement substrate alongside the
// docker package's L0 (structural, no-default-route) substrate. Where docker
// proves confinement by never giving the agent a route off-host, this
// substrate proves it with a Kubernetes NetworkPolicy default-denying the
// agent's egress except the wardyn-proxy sidecar — and, because a
// NetworkPolicy object existing is not proof it is enforced, a boot-time
// two-phase canary (see canary.go) positively confirms the deny actually
// takes effect on THIS cluster before the substrate will advertise any
// Confinement Class. See internal/runner/substrate's package doc for the full
// L0-vs-L1 contract this substrate upholds.
//
// Sandbox shape: one Secret (the proxy's config JSON — API-readable pod specs
// make secretKeyRef the only safe place for it), two NetworkPolicies (agent
// and proxy, created before any pod exists), a proxy pod, and an agent pod
// whose main container idles until Exec adds an ephemeral container to run
// the real task — ephemeral containers are add-only, so unlike docker's
// re-execable `docker exec`, Exec here is one-shot per sandbox (see exec.go).
package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// handlerRunscPrefix is the gVisor OCI-runtime-handler family name, the same
// floor guard the docker driver applies to its CC2 runtime probe (there:
// runtimeRunsc). A RuntimeClass's .Handler — not its object Name, which is an
// arbitrary operator-chosen label — is what actually says which low-level
// runtime it invokes.
const handlerRunscPrefix = "runsc"

// Config configures the k8s Driver. Mirrors the docker driver's Config shape
// where the concepts overlap (ProxyImage, ConfinementRuntimes); k8s-specific
// knobs (Namespace, ImagePullSecret, AllowUnenforcedNetPol) are new.
type Config struct {
	// Namespace is the k8s namespace every sandbox is created in. Resolved by
	// register.go's resolveNamespace before reaching here; New defaults an
	// empty value to "default" defensively (the direct-driver-caller case
	// mirrors docker's Config.withDefaults).
	Namespace string
	// ProxyImage is the wardyn-proxy sidecar image — ALSO the image the
	// boot-time egress canary launches (with -egress-canary instead of its
	// normal entrypoint args).
	ProxyImage string
	// ImagePullSecret optionally names a pre-existing Secret (imagePullSecrets)
	// threaded onto every pod this substrate creates (agent, proxy, canary).
	ImagePullSecret string
	// ConfinementRuntimes maps a Confinement Class to the RuntimeClass NAME
	// (a k8s object name, operator-chosen) that enforces it — WARDYN_CONFINEMENT_MAP's
	// k8s form. Unlike docker (whose runtime family names are a stable,
	// well-known convention `docker info` reports), a k8s RuntimeClass's
	// object name carries no platform convention Wardyn could safely guess:
	// CC2/CC3 are therefore ONLY advertised when explicitly pinned here (see
	// Classes). May be nil.
	ConfinementRuntimes map[types.ConfinementClass]string
	// AllowUnenforcedNetPol is WARDYN_K8S_ALLOW_UNENFORCED_NETPOL: when the
	// boot-time canary proves the cluster's CNI does NOT enforce
	// NetworkPolicy, downgrade that from a fail-closed boot refusal to a loud
	// warning. Even then, ClassSupport.NetworkPolicy/StructuralEgress both
	// stay false — an opted-out substrate must never read as confined.
	AllowUnenforcedNetPol bool
}

func (c *Config) withDefaults() {
	if c.Namespace == "" {
		c.Namespace = "default"
	}
}

// Driver implements substrate.Substrate against the Kubernetes API.
type Driver struct {
	clientset  kubernetes.Interface
	restConfig *rest.Config
	cfg        Config

	// apiserverHostPort is resolved once at construction from restConfig.Host
	// — the egress canary's dial target.
	apiserverHostPort string

	// netPolEnforced is the boot-time canary's verdict, fixed at construction
	// (re-running the canary per Classes()/healthz call would create pods on
	// every poll). netPolOptedOut records WHY it may be false even though
	// construction succeeded (AllowUnenforcedNetPol), for diagnostics only —
	// ClassSupport.NetworkPolicy is netPolEnforced either way.
	netPolEnforced bool
	netPolOptedOut bool
}

var _ substrate.Substrate = (*Driver)(nil)

// New constructs a Driver against the cluster client-go's standard config
// loading resolves (in-cluster, else kubeconfig), running the boot-time
// egress canary before returning. Fails closed: a canary that cannot reach a
// verdict, or one that proves NetworkPolicy is unenforced without
// AllowUnenforcedNetPol, refuses construction entirely (see canary.go).
func New(cfg Config) (*Driver, error) {
	restCfg, err := loadRestConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: build clientset: %w", err)
	}
	return newWithClient(context.Background(), cs, restCfg, cfg)
}

// newWithClient is the seam unit tests use to inject a fake clientset (and a
// synthetic rest.Config — only its Host field matters here, for the canary's
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
	d := &Driver{clientset: cs, restConfig: restCfg, cfg: cfg, apiserverHostPort: hostPort}

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
	}
	return d, nil
}

// logWarnUnenforcedNetPolOptOut is the loud, unmissable warning the
// WARDYN_K8S_ALLOW_UNENFORCED_NETPOL opt-out must emit at construction (the
// A0 contract correction): naming exactly what is going unconfined, so an
// operator who set the override cannot miss that every sandbox this Driver
// creates runs with UNCONFINED egress until it is unset.
func logWarnUnenforcedNetPolOptOut() {
	slog.Warn("wardynd: k8s NetworkPolicy is NOT enforced on this cluster (the boot-time egress canary connected despite a deny-all policy) and WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1 — proceeding anyway: every sandbox this substrate creates has UNCONFINED egress, not the L1 confinement Wardyn normally requires. Classes stay advertised, but NetworkPolicy and StructuralEgress both report false — this substrate will never read as confined on /healthz. Unset WARDYN_K8S_ALLOW_UNENFORCED_NETPOL and fix the cluster's CNI/NetworkPolicy support to restore real confinement.")
}

func (d *Driver) Name() string { return driverName }

// Classes reports the Confinement Classes this substrate can enforce. CC1 is
// unconditional: reaching this point already proves the canary passed or the
// operator explicitly opted out (see New). CC2/CC3 each require an explicit
// WARDYN_CONFINEMENT_MAP pin (see Config.ConfinementRuntimes's doc) resolving
// to a RuntimeClass that exists and whose .Handler passes the class's floor
// guard — never overclaim, never silently downgrade.
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
		// Exec (see exec.go's recordCmd) wraps every ephemeral-container argv
		// with wardyn-rec, delivering via the masked brokered proxy upload —
		// the only path this substrate supports (mounts, and so any
		// shared-volume delivery, are impossible on k8s; see SandboxSpec's
		// Recording doc).
		SessionRecording: true,
	}, nil
}

// runtimeClassHandler resolves a RuntimeClass's .Handler by name, treating
// "not found" as "" (no error) so Classes can fail-closed-omit that class
// rather than fail the whole call — the same asymmetry docker's
// capabilitiesForWith uses between "absent" and "genuine probe error".
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
