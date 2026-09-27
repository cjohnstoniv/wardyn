// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package api

// TestProviderSubscriptionDocker_PerPersonCaptureAndRunIsolation is the hermetic
// Docker+Postgres slice of #677 (T-17): the per-person Claude subscription
// (MP-8, provider_subscription.go) proven against REAL infrastructure rather
// than the in-memory fakes provider_subscription_test.go uses.
//
// What is real here, and why each piece has to be:
//   - Postgres (secretstore/pg, WARDYN_TEST_PG): the per-owner namespace
//     isolation this lane depends on (ownSecret's List-then-Get) is a Postgres
//     contract (Store.For(owner).Get FALLS BACK to the operator's row — see
//     ownSecret's own comment), which the in-memory memSecrets fake does not
//     necessarily reproduce. A member with no capture of her own (carol) must
//     never read the operator's row that genuinely exists under the same
//     wardyn-provider-<uid>-oauth name in the SAME real backend.
//   - Docker (WARDYN_TEST_DOCKER=1): the REAL wardyn-proxy image, in its own
//     container, is what actually performs the proxy-side substitution this
//     lane's whole design rests on — "the sandbox holds only an inert
//     sentinel; the run owner's own sign-in is injected proxy-side" is a claim
//     about that binary, not about resolveLLMInjections in isolation.
//   - A TLS Anthropic-compatible fake (test/subfake, a purpose-built stand-in
//     in the same spirit as test/conformance/hopfake) stands in for both the
//     control plane's credential resolve and the vendor: never a real
//     sign-in, spend or vendor endpoint (out of scope for this issue).
//
// subfake (test/subfake) runs as TWO SEPARATE container instances of the
// same binary, because the real proxy's own SSRF guard (egress_target.go's
// onOwnSubnetOrControlPlane) refuses to lift ANY host on the proxy's own
// attached subnet, or equal to its control-plane's address, NO MATTER what
// InternalHosts declares — by design, so a sandboxed run can never pivot to a
// sibling container or to the control plane's own address. That was measured
// while building this test (a same-network vendor fake was denied
// builtin:private-ip even with an InternalHosts lift for it):
//
//   - the CONTROL-PLANE instance is reached at a bridge-network alias
//     (wardyn-cl-677-cp-<run>, on wardyn-cl-677-net-<run> — suffixed per test
//     run so two concurrent runs never share a network or race its teardown),
//     which the proxy's own direct outbound resolve call reaches directly —
//     reliable, and never subject to vetHost since it is the proxy's OWN
//     call, not sandboxed traffic;
//   - the VENDOR instance is a SEPARATE container (a different address, off
//     that subnet), its port published to the host and reached back from the
//     proxy via host.docker.internal + an InternalHosts lift for it, which
//     DOES apply here because this address is neither the control plane's nor
//     on the proxy's own subnet. A bare (non-containerized) process on this
//     go-test process's own loopback was unreliably reachable via
//     host.docker.internal on this box (Docker Desktop on WSL2) — measured
//     while building this test — but a container's PUBLISHED port, reached
//     the same way, was reliable (Docker Desktop's own port-forwarding path,
//     not the separate WSL2-interop one). --network host does not share the
//     WSL distro's network namespace at all (also measured), so it is used
//     for neither.
//
// Wire, per owner: dispatchSub (the real resolveLLMInjections) authors the
// real per-owner grant, sourced from a REAL Postgres row -> the test's own Go
// process calls the REAL resolveProviderSubscriptionInjection handler
// in-process on that SAME grant (resolveSubAs) and reads back its actual
// "value" -> runner.BuildProxyConfig seals a plan exactly as dispatchRun
// would -> the REAL wardyn-proxy container resolves the grant against the
// control-plane subfake, which answers with THAT value (never a Go
// constant) -> a raw HTTP client (published proxy port on the host) stands
// in for the sandbox, trusting only the run's own MITM CA
// (plan.mitmCACertPEM, the same one dispatch generates in production) -> the
// vendor subfake logs the Authorization header the proxy actually forwarded,
// which the test asserts equals the SAME value the real handler served,
// never another owner's or the operator's.
//
// Three real members share ONE provider row and ONE Postgres-backed store:
// alice and bob each captured their own token; carol captured nothing (the
// third-member-refused-before-dispatch case). The operator's row is ALSO
// seeded under the same name, as bait for a namespace-fallback regression.
//
//   - namespace fallback is pinned by carol: mutate ownSecret to read
//     Store.For(owner).Get directly (dropping the List-then-Get) and her
//     dispatch wrongly succeeds with the operator's token instead of being
//     refused — this test goes red on that mutation alone.
//   - owner matching is pinned separately by the "owner matching" subtest,
//     against the SAME real Postgres-backed harness, calling the REAL
//     resolveProviderSubscriptionInjection handler for a grant whose own
//     record was tampered to claim another owner: mutate away its
//     rec.OwnerSubject != claims.Sub check and this refusal turns into a 200
//     (verified: the docker-container "refused by subfake" subtest below does
//     NOT pin this — subfake's own refusal there is a fixed test double, so it
//     proves only that the real proxy container stops forwarding once ITS
//     control-plane call comes back refused, whoever refused it).
import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	dockerclient "github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	pgsecrets "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// subDockerNetworkPrefix/subDockerCPHostPrefix: the bridge network and alias
// the control-plane subfake instance is reachable at from the proxy
// container, each made unique per test run (a random suffix) so two
// concurrent runs never share a network or race its teardown (F7).
// subDockerVendorHost is Docker Desktop's built-in host-reachable alias, used
// for the SEPARATE vendor subfake instance — see the file doc comment for why
// these must be two different addresses. On a native-Linux Docker engine
// (no built-in host.docker.internal DNS entry) runSubDockerProxy maps it
// explicitly via ExtraHosts/host-gateway.
const (
	subDockerNetworkPrefix = "wardyn-cl-677-net-"
	subDockerCPHostPrefix  = "wardyn-cl-677-cp-"
	subDockerVendorHost    = "host.docker.internal"
)

// buildProxyImage builds the REAL wardyn-proxy image from THIS worktree's own
// source, under a uniquely tagged name derived from the checked-out commit —
// never the pre-existing, shared wardyn/wardyn-proxy:local tag other sessions
// may be relying on. Removes the tag by this exact name in cleanup.
func buildProxyImage(t *testing.T) string {
	t.Helper()
	sha, err := exec.Command("git", "rev-parse", "--short=9", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	tag := fmt.Sprintf("wardyn/wardyn-proxy:cl-677-%s-%s", strings.TrimSpace(string(sha)), uuid.NewString()[:8])
	cmd := exec.Command("docker", "build", "-f", "../../deploy/compose/Dockerfile.proxy", "-t", tag, "../..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", tag, err, out)
	}
	t.Cleanup(func() {
		rmCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(rmCtx, "docker", "rmi", tag).CombinedOutput(); err != nil {
			t.Logf("remove image %s: %v\n%s", tag, err, out)
		}
	})
	return tag
}

// requireDockerAndPGSecrets is the ONE guard for this file: real Docker and a
// real Postgres both configured, or skip cleanly (never a false pass). Returns
// a real Postgres-backed secretstore.Store — the per-owner isolation under
// test is a property of THIS backend, not of memSecrets.
func requireDockerAndPGSecrets(t *testing.T) *pgsecrets.Store {
	t.Helper()
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping the hermetic Docker+Postgres per-person subscription test")
	}
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping the hermetic Docker+Postgres per-person subscription test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to WARDYN_TEST_PG: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	st, err := pgsecrets.New(pool, id)
	if err != nil {
		t.Fatalf("secretstore/pg.New: %v", err)
	}
	return st
}

// subHarnessPGDocker mirrors provider_subscription_test.go's subHarness but
// wires a REAL secret store instead of memSecrets — everything else (the
// fake Store for run/site-config, the shared-subscription posture off, the
// resolvable Claude sign-in image) is identical, since only the secret
// backend is this file's reason to exist.
func subHarnessPGDocker(t *testing.T, sec *pgsecrets.Store) *harness {
	t.Helper()
	h := newHarness(t)
	h.srv.cfg.Secrets = sec
	h.srv.cfg.AgentImages = map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
	h.srv.cfg.SubscriptionPostureOK = false
	h.srv.cfg.SubscriptionPostureReason = "OIDC/SSO is configured"
	h.srv.router = h.srv.routes()
	return h
}

// subDockerDispatch runs the real dispatch LLM phase for a fresh run owned by
// owner, on p, against h's currently-wired store/secrets.
func subDockerDispatch(h *harness, p types.ModelProvider, owner string) (*subStore, dispatchLLMPlan, bool) {
	st := &subStore{
		run:  types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: owner, ModelProviderID: p.ID},
		site: types.SiteConfig{ModelProviders: providerBlock(p)},
	}
	h.srv.cfg.Store = st
	plan, ok := dispatchSub(h, st, &types.RunPolicySpec{}, map[string]string{}, nil)
	return st, plan, ok
}

// buildSubfakeBinary builds test/subfake for the daemon's platform once and
// returns the built binary's path.
func buildSubfakeBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "subfake")
	cmd := exec.Command("go", "build", "-o", bin, "../../test/subfake")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build test/subfake: %v\n%s", err, out)
	}
	return bin
}

// ensureSubDockerNetwork creates the dedicated bridge network (per-run name,
// netName) the control-plane subfake instance and the proxy container share.
func ensureSubDockerNetwork(t *testing.T, cli *dockerclient.Client, netName string) {
	t.Helper()
	ctx := context.Background()
	_, err := cli.NetworkCreate(ctx, netName, dockerclient.NetworkCreateOptions{Driver: "bridge"})
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		t.Fatalf("create network %q: %v", netName, err)
	}
	t.Cleanup(func() {
		rmCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = cli.NetworkRemove(rmCtx, netName, dockerclient.NetworkRemoveOptions{})
	})
}

// subfakeCert mints an hoptls CA and a leaf for host, PEM-encoded for the
// container env subfake reads (SUBFAKE_CERT_PEM/KEY_PEM) and for
// ProxyConfig's ControlPlaneCAPEM/TrustedCAPEM.
func subfakeCert(t *testing.T, host string) (certPEM, keyPEM []byte, caPEM string) {
	t.Helper()
	now := time.Now()
	caBlob, err := hoptls.NewCA(now)
	if err != nil {
		t.Fatalf("hoptls.NewCA: %v", err)
	}
	ca, err := hoptls.ParseCA(caBlob)
	if err != nil {
		t.Fatalf("hoptls.ParseCA: %v", err)
	}
	cert, err := ca.ServingCert(host, now)
	if err != nil {
		t.Fatalf("ca.ServingCert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatalf("marshal subfake key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, string(ca.CertPEM)
}

// startSubfakeCP runs the control-plane subfake instance on netName under
// cpHost, serving TLS for that name. It answers grantID's resolve with
// {header, value} (or a 403 when refuse). Reached by the real proxy
// container's OWN direct outbound call — never sandboxed traffic, so this
// never needs (and, on the proxy's own subnet, could never get) an
// InternalHosts lift. Returns the container id and the CA PEM for
// ProxyConfig.ControlPlaneCAPEM.
func startSubfakeCP(t *testing.T, cli *dockerclient.Client, bin, netName, cpHost string, grantID uuid.UUID, header, value string, refuse bool) (id, caPEM string) {
	t.Helper()
	certPEM, keyPEM, caPEM := subfakeCert(t, cpHost)
	name := "wardyn-cl-677-subfake-cp-" + uuid.NewString()[:8]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	env := []string{
		"SUBFAKE_CERT_PEM=" + string(certPEM),
		"SUBFAKE_KEY_PEM=" + string(keyPEM),
		"SUBFAKE_GRANT=" + grantID.String(),
		"SUBFAKE_HEADER=" + header,
		"SUBFAKE_VALUE=" + value,
	}
	if refuse {
		env = append(env, "SUBFAKE_REFUSE=1")
	}
	created, err := cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image: "busybox:latest",
			Cmd:   []string{"/subfake"},
			Env:   env,
		},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(netName)},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {Aliases: []string{cpHost}},
		}},
	})
	if err != nil {
		t.Fatalf("create control-plane subfake: %v", err)
	}
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer rmCancel()
		_, _ = cli.ContainerRemove(rmCtx, created.ID, dockerclient.ContainerRemoveOptions{Force: true})
	})
	if err := copyBinaryToContainer(ctx, cli, created.ID, bin, "subfake"); err != nil {
		t.Fatalf("copy subfake binary: %v", err)
	}
	if _, err := cli.ContainerStart(ctx, created.ID, dockerclient.ContainerStartOptions{}); err != nil {
		t.Fatalf("start control-plane subfake: %v", err)
	}
	return created.ID, caPEM
}

// startSubfakeVendor runs a SEPARATE subfake instance (a different container,
// off the proxy's own subnet) with its :8443 published to 127.0.0.1:port on
// the host, serving TLS for subDockerVendorHost — the real proxy container
// reaches it back at that name, landing on the published port. Its grant id
// is a random one no proxy ever asks it to resolve; its only real job is
// recording the Authorization header of whatever the proxy forwards to it
// (the vendor leg). Returns the container id and the CA PEM for
// ProxyConfig.TrustedCAPEM.
func startSubfakeVendor(t *testing.T, cli *dockerclient.Client, bin string, port int) (id, caPEM string) {
	t.Helper()
	certPEM, keyPEM, caPEM := subfakeCert(t, subDockerVendorHost)
	name := "wardyn-cl-677-subfake-vendor-" + uuid.NewString()[:8]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	env := []string{
		"SUBFAKE_CERT_PEM=" + string(certPEM),
		"SUBFAKE_KEY_PEM=" + string(keyPEM),
		"SUBFAKE_GRANT=" + uuid.NewString(), // never asked for
		"SUBFAKE_HEADER=", "SUBFAKE_VALUE=",
	}
	pp, err := network.ParsePort("8443/tcp")
	if err != nil {
		t.Fatalf("parse subfake vendor port: %v", err)
	}
	created, err := cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:        "busybox:latest",
			Cmd:          []string{"/subfake"},
			Env:          env,
			ExposedPorts: network.PortSet{pp: struct{}{}},
		},
		HostConfig: &container.HostConfig{
			PortBindings: network.PortMap{pp: []network.PortBinding{
				{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: strconv.Itoa(port)},
			}},
		},
	})
	if err != nil {
		t.Fatalf("create vendor subfake: %v", err)
	}
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer rmCancel()
		_, _ = cli.ContainerRemove(rmCtx, created.ID, dockerclient.ContainerRemoveOptions{Force: true})
	})
	if err := copyBinaryToContainer(ctx, cli, created.ID, bin, "subfake"); err != nil {
		t.Fatalf("copy subfake binary: %v", err)
	}
	if _, err := cli.ContainerStart(ctx, created.ID, dockerclient.ContainerStartOptions{}); err != nil {
		t.Fatalf("start vendor subfake: %v", err)
	}
	return created.ID, caPEM
}

// runSubDockerProxy starts the REAL wardyn-proxy image (this worktree's own
// uniquely tagged build, image) as a bare container on netName (so it reaches
// the control-plane subfake by its alias), publishing its listen port to
// 127.0.0.1:port on the host so this test's own "sandbox" client can dial it
// directly. Named wardyn-cl-677-* per the lane's fixture-naming rule.
// ExtraHosts maps subDockerVendorHost to the host gateway explicitly: Docker
// Desktop provides that DNS entry built in, but a native-Linux Docker engine
// does not, and the vendor leg (a different, non-aliased address — see the
// file doc comment) needs it either way.
func runSubDockerProxy(t *testing.T, cli *dockerclient.Client, netName, image string, runID uuid.UUID, cfgJSON []byte, port int) string {
	t.Helper()
	name := "wardyn-cl-677-proxy-" + runID.String()[:8]
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pp, err := network.ParsePort(strconv.Itoa(port) + "/tcp")
	if err != nil {
		t.Fatalf("parse proxy port: %v", err)
	}
	created, err := cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:        image,
			Env:          []string{"WARDYN_PROXY_CONFIG_JSON=" + string(cfgJSON)},
			ExposedPorts: network.PortSet{pp: struct{}{}},
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(netName), // reaches cpHost by alias
			PortBindings: network.PortMap{pp: []network.PortBinding{
				{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: strconv.Itoa(port)},
			}},
			ExtraHosts: []string{subDockerVendorHost + ":host-gateway"},
		},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {},
		}},
	})
	if err != nil {
		t.Fatalf("create proxy container: %v", err)
	}
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer rmCancel()
		_, _ = cli.ContainerRemove(rmCtx, created.ID, dockerclient.ContainerRemoveOptions{Force: true})
	})
	if _, err := cli.ContainerStart(ctx, created.ID, dockerclient.ContainerStartOptions{}); err != nil {
		t.Fatalf("start proxy container: %v", err)
	}
	return created.ID
}

// containerLogs returns id's combined stdout+stderr, for assertions and
// failure diagnostics.
func containerLogs(cli *dockerclient.Client, id string) string {
	rc, err := cli.ContainerLogs(context.Background(), id, dockerclient.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "logs: " + err.Error()
	}
	defer rc.Close()
	var buf strings.Builder
	_, _ = stdcopy.StdCopy(&buf, &buf, rc)
	return buf.String()
}

// waitForLog polls id's logs until they contain want or within elapses.
func waitForLog(t *testing.T, cli *dockerclient.Client, id, want string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		logs := containerLogs(cli, id)
		if strings.Contains(logs, want) {
			return logs
		}
		if time.Now().After(deadline) {
			return logs // caller decides what an absent line means
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// copyBinaryToContainer tars bin (as destName, mode 0755) into the container's
// root, mirroring test/conformance/proxy_hop_tls_docker_test.go's startHopFake.
func copyBinaryToContainer(ctx context.Context, cli *dockerclient.Client, containerID, bin, destName string) error {
	b, err := os.ReadFile(bin)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: destName, Mode: 0o755, Size: int64(len(b))}); err != nil {
		return err
	}
	if _, err := tw.Write(b); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	_, err = cli.CopyToContainer(ctx, containerID, dockerclient.CopyToContainerOptions{
		DestinationPath: "/", Content: &buf,
	})
	return err
}

// sendThroughDockerProxy sends one POST through the real containerized proxy
// at 127.0.0.1:port to https://subDockerVendorHost:vendorPort/v1/messages,
// retrying while the container is still coming up (or, for the refused case,
// until the retry budget is spent because the container already exited).
// caPEM is the run's own MITM CA (plan.mitmCACertPEM) — the ONLY root the
// sandbox trusts, exactly as the real agent env would be configured.
func sendThroughDockerProxy(t *testing.T, port int, caPEM string, vendorPort int) (*http.Response, error) {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		t.Fatal("append run MITM CA to pool")
	}
	proxyURL, err := url.Parse("http://127.0.0.1:" + strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
	target := fmt.Sprintf("https://%s:%d/v1/messages", subDockerVendorHost, vendorPort)
	var resp *http.Response
	for i := 0; i < 80; i++ {
		req, rerr := http.NewRequest(http.MethodPost, target, strings.NewReader(`{}`))
		if rerr != nil {
			t.Fatal(rerr)
		}
		req.Header.Set("Authorization", "Bearer wardyn-managed-sentinel")
		req.Header.Set("Content-Type", "application/json")
		resp, err = client.Do(req)
		if err == nil {
			return resp, nil
		}
		time.Sleep(75 * time.Millisecond)
	}
	return nil, err
}

// resolveSubAs calls the REAL resolveProviderSubscriptionInjection handler,
// in-process, for owner's own grant on st — generalizing provider_subscription_test.go's
// resolveSub (which always mints the run token as alice) to mint as owner
// instead, so this also works for bob's subtest. This is what makes the
// Docker legs non-circular: the value fed onward to the control-plane subfake
// below is READ from this call, sourced from the real Postgres-backed store
// wired into h, rather than a Go constant the test already knows. A sink
// mutation that serves the wrong owner's (or the operator's) row surfaces
// right here, before any container starts.
func resolveSubAs(t *testing.T, h *harness, st *subStore, host, owner string) (int, string) {
	t.Helper()
	g := st.grants[0]
	var scope struct {
		SecretName string `json:"secret_name"`
	}
	_ = json.Unmarshal(g.Spec.Scope, &scope)
	h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "jti-sub-docker", Injection: &egress.InjectionRule{
		Host: host, Header: "Authorization", SecretName: scope.SecretName, Format: "Bearer %s",
	}}
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+g.ID.String(), mintRunTokenAs(t, h, st.run.ID, owner), "")
	return rr.Code, rr.Body.String()
}

func TestProviderSubscriptionDocker_PerPersonCaptureAndRunIsolation(t *testing.T) {
	sec := requireDockerAndPGSecrets(t)
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	runSuffix := uuid.NewString()[:8] // per-run network/alias suffix (F7): never shared across concurrent runs
	netName := subDockerNetworkPrefix + runSuffix
	cpHost := subDockerCPHostPrefix + runSuffix
	ensureSubDockerNetwork(t, cli, netName)
	subfakeBin := buildSubfakeBinary(t)
	proxyImage := buildProxyImage(t)

	const dockerBob = "bob@example.com"
	const dockerCarol = "carol@example.com"

	p := subProvider("claude") // one fixed UID for the whole test; BaseURL set per-subtest (each starts its own subfake, on its own port)

	ctx := context.Background()
	name := providerSecretName(p.UID, providerOAuthPart)
	if err := sec.Put(ctx, name, subBlob(subOperatorToken)); err != nil {
		t.Fatalf("seed operator row: %v", err)
	}
	if err := sec.For(subOwner).Put(ctx, name, subBlob(subOwnerToken)); err != nil {
		t.Fatalf("seed alice's row: %v", err)
	}
	if err := sec.For(dockerBob).Put(ctx, name, subBlob(subOtherToken)); err != nil {
		t.Fatalf("seed bob's row: %v", err)
	}
	// dockerCarol: nothing stored. The operator's row above, under the SAME
	// name, is the namespace-fallback bait.

	h := subHarnessPGDocker(t, sec)

	t.Run("a member with no capture of her own is refused before any grant is authored", func(t *testing.T) {
		st, _, ok := subDockerDispatch(h, p, dockerCarol)
		if ok || len(st.grants) != 0 {
			t.Fatalf("carol's dispatch ok=%v grants=%d, want refused with no grant authored", ok, len(st.grants))
		}
		want := fmt.Sprintf(mpRunRefusal, p.ID, mpSubNotSignedIn, mpRunRemedySignIn)
		if st.failed != want {
			t.Fatalf("failure hint = %q, want %q — carol must never resolve the operator's real Postgres row", st.failed, want)
		}
	})

	for _, tc := range []struct {
		name, owner, wantToken string
	}{
		{"alice's own real Postgres-stored token reaches the fake vendor, never the operator's", subOwner, subOwnerToken},
		{"bob's own real Postgres-stored token reaches the fake vendor, never alice's or the operator's", dockerBob, subOtherToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vendorPort := freeLoopbackPort(t)
			pp := p
			pp.BaseURL = fmt.Sprintf("https://%s:%d", subDockerVendorHost, vendorPort)
			st, plan, ok := subDockerDispatch(h, pp, tc.owner)
			if !ok || len(plan.injections) != 1 || len(st.grants) != 1 {
				t.Fatalf("dispatch: ok=%v injections=%d grants=%d (failed: %q)", ok, len(plan.injections), len(st.grants), st.failed)
			}
			if plan.mitmCACertPEM == "" || plan.mitmCAKeyPEM == "" || !plan.mitmLLM {
				t.Fatalf("dispatch did not provision a per-run MITM CA: mitmLLM=%v", plan.mitmLLM)
			}
			// Call the REAL resolveProviderSubscriptionInjection handler,
			// in-process, on this SAME grant — the one thing that makes the
			// docker leg below non-circular. resolved.Value is sourced from
			// real Postgres (via h's pg-backed secret store), never a Go
			// constant; a sink mutation that serves the wrong owner's row (or
			// the operator's) diverges from tc.wantToken right here, before
			// any container starts.
			code, body := resolveSubAs(t, h, st, plan.injections[0].Rule.Host, tc.owner)
			if code != http.StatusOK {
				t.Fatalf("real resolve handler (Postgres-backed) = %d %s, want 200", code, body)
			}
			var resolved types.ResolvedInjection
			if err := json.Unmarshal([]byte(body), &resolved); err != nil {
				t.Fatalf("unmarshal resolve response: %v\n%s", err, body)
			}
			wantValue := "Bearer " + tc.wantToken
			if resolved.Value != wantValue {
				t.Fatalf("the real handler served %q, want %q — it must come from %s's own real Postgres row, never another owner's or the operator's", resolved.Value, wantValue, tc.owner)
			}

			runToken := mintRunTokenAs(t, h, st.run.ID, tc.owner)
			h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: uuid.NewString(), Injection: &egress.InjectionRule{
				Host: plan.injections[0].Rule.Host, Header: "Authorization",
				SecretName: plan.injections[0].Rule.SecretName, Format: "Bearer %s",
			}}
			// The control-plane subfake answers THIS run's grant with
			// resolved.Value — literally what the REAL resolve handler just
			// served from real Postgres above, not a Go constant. The vendor
			// subfake is a SEPARATE instance/address (see the file doc
			// comment for why) whose only job is recording what the proxy
			// forwards to it.
			cpID, cpCAPEM := startSubfakeCP(t, cli, subfakeBin, netName, cpHost, st.grants[0].ID, "Authorization", resolved.Value, false)
			vendorID, vendorCAPEM := startSubfakeVendor(t, cli, subfakeBin, vendorPort)

			port := freeLoopbackPort(t)
			raw, err := runner.BuildProxyConfig(st.run.ID, runner.ProxyConfig{
				RunToken:          runToken,
				ControlPlaneURL:   fmt.Sprintf("https://%s:8443", cpHost),
				ControlPlaneCAPEM: cpCAPEM,
				Policy:            types.RunPolicySpec{AllowedDomains: []string{subDockerVendorHost}},
				InternalHosts:     []types.InternalHost{{HostSuffix: subDockerVendorHost}},
				Injection:         []runner.InjectionGrant{plan.injections[0]},
				MITMCACertPEM:     plan.mitmCACertPEM,
				MITMCAKeyPEM:      plan.mitmCAKeyPEM,
				MITMHosts:         plan.bedrockMITMHosts,
				MITMLLM:           true,
				TrustedCAPEM:      vendorCAPEM,
			}, port)
			if err != nil {
				t.Fatalf("BuildProxyConfig: %v", err)
			}
			proxyID := runSubDockerProxy(t, cli, netName, proxyImage, st.run.ID, raw, port)

			resp, err := sendThroughDockerProxy(t, port, plan.mitmCACertPEM, vendorPort)
			if err != nil {
				t.Fatalf("request through the real proxy container: %v\n--- proxy logs ---\n%s\n--- cp subfake logs ---\n%s\n--- vendor subfake logs ---\n%s",
					err, containerLogs(cli, proxyID), containerLogs(cli, cpID), containerLogs(cli, vendorID))
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (the real proxy container forwarded to the vendor subfake)", resp.StatusCode)
			}

			want := fmt.Sprintf("subfake: vendor request POST /v1/messages auth=%q", wantValue)
			logs := waitForLog(t, cli, vendorID, want, 5*time.Second)
			if !strings.Contains(logs, want) {
				t.Fatalf("the vendor subfake did not see %q; logs:\n%s", want, logs)
			}
			for _, leak := range []string{subOwnerToken, subOtherToken, subOperatorToken} {
				if leak != tc.wantToken && strings.Contains(logs, "auth=\"Bearer "+leak+"\"") {
					t.Fatalf("the wrong owner's token reached the fake vendor:\n%s", logs)
				}
			}
		})
	}

	t.Run("owner matching: a grant recording the wrong owner is refused, against real Postgres", func(t *testing.T) {
		st, plan, ok := subDockerDispatch(h, p, subOwner)
		if !ok || len(plan.injections) != 1 || len(st.grants) != 1 {
			t.Fatalf("dispatch: ok=%v injections=%d grants=%d", ok, len(plan.injections), len(st.grants))
		}
		// Tamper: the grant's own record now claims BOB authored it, though this
		// run belongs to (and is token-authenticated as) alice.
		st.grants[0].Spec.Scope = subScope(p.UID, dockerBob)
		code, body := resolveSub(t, h, st, plan.injections[0].Rule.Host)
		if code != http.StatusForbidden {
			t.Fatalf("resolve = %d %s, want 403 (owner mismatch)", code, body)
		}
		for _, leak := range []string{subOwnerToken, subOtherToken, subOperatorToken} {
			if strings.Contains(body, leak) {
				t.Fatalf("refusal carries a token: %s", body)
			}
		}
	})

	t.Run("a grant recording the wrong owner is refused by subfake, and the real proxy container never forwards it", func(t *testing.T) {
		vendorPort := freeLoopbackPort(t)
		pp := p
		pp.BaseURL = fmt.Sprintf("https://%s:%d", subDockerVendorHost, vendorPort)
		st, plan, ok := subDockerDispatch(h, pp, subOwner)
		if !ok || len(plan.injections) != 1 || len(st.grants) != 1 {
			t.Fatalf("dispatch: ok=%v injections=%d grants=%d", ok, len(plan.injections), len(st.grants))
		}
		runToken := mintRunTokenAs(t, h, st.run.ID, subOwner)
		h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: uuid.NewString(), Injection: &egress.InjectionRule{
			Host: plan.injections[0].Rule.Host, Header: "Authorization",
			SecretName: plan.injections[0].Rule.SecretName, Format: "Bearer %s",
		}}
		cpID, cpCAPEM := startSubfakeCP(t, cli, subfakeBin, netName, cpHost, st.grants[0].ID, "Authorization", "Bearer "+subOwnerToken, true /* refuse */)
		vendorID, vendorCAPEM := startSubfakeVendor(t, cli, subfakeBin, vendorPort)

		port := freeLoopbackPort(t)
		raw, err := runner.BuildProxyConfig(st.run.ID, runner.ProxyConfig{
			RunToken:          runToken,
			ControlPlaneURL:   fmt.Sprintf("https://%s:8443", cpHost),
			ControlPlaneCAPEM: cpCAPEM,
			Policy:            types.RunPolicySpec{AllowedDomains: []string{subDockerVendorHost}},
			InternalHosts:     []types.InternalHost{{HostSuffix: subDockerVendorHost}},
			Injection:         []runner.InjectionGrant{plan.injections[0]},
			MITMCACertPEM:     plan.mitmCACertPEM,
			MITMCAKeyPEM:      plan.mitmCAKeyPEM,
			MITMHosts:         plan.bedrockMITMHosts,
			MITMLLM:           true,
			TrustedCAPEM:      vendorCAPEM,
		}, port)
		if err != nil {
			t.Fatalf("BuildProxyConfig: %v", err)
		}
		proxyID := runSubDockerProxy(t, cli, netName, proxyImage, st.run.ID, raw, port)

		// A refused resolve fails the proxy's startup (it exits), so the
		// container may already be gone by the time we can dial it — either
		// shape (a refused response, or a dead listener) is the pass; what must
		// NEVER happen is a request reaching the vendor subfake.
		if resp, rerr := sendThroughDockerProxy(t, port, plan.mitmCACertPEM, vendorPort); rerr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("status = 200 through a refused resolve, want the tunnel refused")
			}
		}
		// F6: "the vendor never saw a request" alone would pass vacuously for
		// ANY proxy failure (bad config, a crash, an unreachable control
		// plane) — none of that pins the refusal this subtest is named for.
		// Require the real proxy container's OWN startup log to name the
		// control plane's 403 specifically, not just any absence.
		proxyLogs := waitForLog(t, cli, proxyID, "injection status 403", 5*time.Second)
		if !strings.Contains(proxyLogs, "injection status 403") {
			t.Fatalf("the real proxy container never logged the control-plane's 403 refusal (only a vacuous absence of a vendor request would otherwise pass this subtest):\n%s", proxyLogs)
		}
		if logs := containerLogs(cli, vendorID); strings.Contains(logs, "vendor request") {
			t.Fatalf("the vendor subfake saw a request despite the refused resolve:\n--- proxy logs ---\n%s\n--- cp subfake logs ---\n%s\n--- vendor subfake logs ---\n%s",
				proxyLogs, containerLogs(cli, cpID), logs)
		}
	})
}
