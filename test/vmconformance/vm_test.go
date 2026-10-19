// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build vm && linux

// Package vmconformance boots a real QEMU guest behind wardyn-vmnet and the
// real wardyn-proxy and runs the negative egress probe inside it, then runs
// the same probe on QEMU's own user networking, which must go red. Everything
// is hermetic: the origin sits on an internal network only the proxy joins.
//
// Run: WARDYN_TEST_VM=1 go test -tags vm -count=1 -v ./test/vmconformance/
// (Docker on DOCKER_HOST; /dev/kvm is used when present, TCG otherwise).
package vmconformance

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Rows the probe must fail when nothing pins the guest: each is a way out that
// QEMU's user networking leaves open and the relay closes.
var mustGoRed = []string{
	"dns-synthetic", "https-not-allowlisted", "literal-ip-443", "sni-fronting", "sni-split-mismatch",
	"sni-delayed", "tls-granted-8443-mismatch", "bare-host-22", "http-host-mismatch", "http-method-rule",
	"udp-dns-external", "udp-quic", "udp-ntp",
}

const (
	runSubnet = "10.231.0.0/24"
	upSubnet  = "10.231.1.0/24"
	originIP  = "10.231.1.10"
	proxyIP   = "10.231.0.2"
)

type env struct {
	id, sockDir, caB64 string
	images             map[string]string
}

func docker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (e *env) must(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	out, err := docker(ctx, args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, tail(out))
	}
	return out
}

func tail(s string) string {
	if l := strings.Split(s, "\n"); len(l) > 40 {
		return strings.Join(l[len(l)-40:], "\n")
	}
	return s
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func setup(t *testing.T) *env {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	e := &env{id: "vmconf-" + hex.EncodeToString(b), images: map[string]string{}}
	root := repoRoot()
	cleanup := func(args ...string) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = docker(ctx, args...)
	}
	t.Cleanup(func() {
		cleanup("rm", "-f", e.id+"-vmnet", e.id+"-proxy", e.id+"-origin", e.id+"-vm", e.id+"-canary")
		cleanup("network", "rm", e.id+"-run", e.id+"-up")
		for _, img := range e.images {
			cleanup("rmi", "-f", img)
		}
	})
	for name, args := range map[string][]string{
		"proxy":  {"-f", "deploy/compose/Dockerfile.proxy"},
		"vmnet":  {"-f", "deploy/compose/Dockerfile.vmnet"},
		"vm":     {"-f", "test/vmconformance/testdata/Dockerfile", "--target", "vm"},
		"origin": {"-f", "test/vmconformance/testdata/Dockerfile", "--target", "origin"},
	} {
		e.images[name] = "wardyn-vmconf/" + name + ":" + e.id
		e.must(t, append(append([]string{"build", "-q", "-t", e.images[name]}, args...), root)...)
	}
	e.must(t, "network", "create", "--internal", "--subnet", runSubnet, e.id+"-run")
	e.must(t, "network", "create", "--internal", "--subnet", upSubnet, e.id+"-up")

	tlsDir := t.TempDir()
	e.caB64 = originCert(t, tlsDir)
	e.must(t, "run", "-d", "--name", e.id+"-origin", "-v", tlsDir+":/tls:ro", "--network", e.id+"-up", "--ip", originIP,
		"--network-alias", "origin.test", "--network-alias", "other.test",
		"--sysctl", "net.ipv4.ip_unprivileged_port_start=0", e.images["origin"], "-cert", "/tls/cert.pem", "-key", "/tls/key.pem")

	cfgDir := t.TempDir()
	cfg := map[string]any{
		"run_id":            "5f1d0c2e-0000-4000-8000-0000000005a1",
		"control_plane_url": "http://127.0.0.1:9",
		"run_token":         "vm-conformance-placeholder",
		"vm":                true,
		"policy": map[string]any{
			"allowed_domains":    []string{"origin.test", "origin.test:8443", "origin.test:2222"},
			"allowed_methods":    []string{"GET", "HEAD", "CONNECT"},
			"first_use_approval": "always_deny",
		},
		"internal_hosts": []map[string]any{{"host_suffix": "origin.test", "cidrs": []string{upSubnet}}},
	}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(cfgDir, "proxy.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e.must(t, "create", "--name", e.id+"-proxy", "--network", e.id+"-run", "--ip", proxyIP, "--network-alias", "wardyn-proxy",
		"-v", cfgDir+":/cfg:ro", e.images["proxy"], "-config", "/cfg/proxy.json")
	e.must(t, "network", "connect", e.id+"-up", e.id+"-proxy")
	e.must(t, "start", e.id+"-proxy")

	// The run volume: vmnet (uid 65532) makes net.sock in it and QEMU, the
	// same uid, connects.
	e.sockDir = t.TempDir()
	if err := os.Chmod(e.sockDir, 0o777); err != nil {
		t.Fatal(err)
	}
	e.must(t, "run", "-d", "--name", e.id+"-vmnet", "--network", e.id+"-run",
		"--cap-drop", "ALL", "--read-only", "--security-opt", "no-new-privileges", "--user", "65532:65532",
		"-v", e.sockDir+":/run/vm", e.images["vmnet"], "-listen", "/run/vm/net.sock", "-proxy", "wardyn-proxy:3128")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(e.sockDir, "net.sock")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("vmnet never made its socket:\n%s", e.must(t, "logs", e.id+"-vmnet"))
		}
	}
	return e
}

// originCert writes the origin's self-signed certificate (origin.test and
// other.test) into dir and returns it as base64url DER for the guest.
func originCert(t *testing.T, dir string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "origin.test"},
		DNSNames: []string{"origin.test", "other.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]*pem.Block{"cert.pem": {Type: "CERTIFICATE", Bytes: der}, "key.pem": {Type: "EC PRIVATE KEY", Bytes: kder}} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(der)
}

type probeRow struct {
	Row    string `json:"row"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// boot runs the guest once and returns its probe rows. pinned attaches the
// guest's NIC to vmnet's socket; otherwise QEMU's user networking (slirp) on
// the origin's network is the control.
func (e *env) boot(t *testing.T, pinned bool) map[string]probeRow {
	t.Helper()
	args := []string{"run", "--rm", "--name", e.id + "-vm", "--cap-drop", "ALL", "--security-opt", "no-new-privileges"}
	accel := "tcg"
	if fi, err := os.Stat("/dev/kvm"); err == nil && fi.Mode()&os.ModeDevice != 0 {
		accel = "kvm:tcg"
		args = append(args, "--device", "/dev/kvm", "--group-add", fmt.Sprint(kvmGroup(fi)))
	}
	netdev, dns := "user,id=n0", "10.0.2.3"
	if pinned {
		args = append(args, "--network", "none", "-v", e.sockDir+":/run/vm")
		netdev, dns = "stream,id=n0,server=off,addr.type=unix,addr.path=/run/vm/net.sock", "10.0.2.2"
	} else {
		args = append(args, "--network", e.id+"-up")
	}
	args = append(args, e.images["vm"],
		"-machine", "q35,accel="+accel, "-cpu", "max", "-m", "512", "-smp", "2",
		"-nographic", "-no-reboot", "-kernel", "/vm/vmlinuz", "-initrd", "/vm/initrd.gz",
		"-append", fmt.Sprintf("console=ttyS0 quiet panic=-1 vmprobe.dns=%s vmprobe.origin=%s vmprobe.proxy=%s vmprobe.ca=%s", dns, originIP, proxyIP, e.caB64),
		"-netdev", netdev, "-device", "virtio-net-pci,netdev=n0,romfile=")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	out, err := docker(ctx, args...)
	rows := map[string]probeRow{}
	done := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "VMPROBE-DONE" {
			done = true
		}
		if j, ok := strings.CutPrefix(line, "VMPROBE "); ok {
			var r probeRow
			if json.Unmarshal([]byte(j), &r) == nil {
				rows[r.Row] = r
			}
		}
	}
	if !done {
		t.Fatalf("the guest probe did not finish (%v):\n%s", err, tail(out))
	}
	for _, r := range rows {
		t.Logf("%-26s pass=%-5v %s", r.Row, r.Pass, r.Detail)
	}
	return rows
}

func kvmGroup(fi os.FileInfo) uint32 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Gid
	}
	return 0
}

// decisions reads the proxy's decision rows (its stdout).
func (e *env) decisions(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(e.must(t, "logs", e.id+"-proxy"), "\n") {
		var d map[string]any
		if json.Unmarshal([]byte(l), &d) == nil && d["rule_source"] != nil {
			out = append(out, d)
		}
	}
	return out
}

func TestVMNetConformance(t *testing.T) {
	if os.Getenv("WARDYN_TEST_VM") != "1" {
		t.Skip("set WARDYN_TEST_VM=1 (needs Docker, and builds the proxy, relay and guest images)")
	}
	e := setup(t)

	t.Run("relay isolation", func(t *testing.T) {
		var c []struct {
			Config     struct{ User string }
			HostConfig struct {
				CapDrop        []string
				ReadonlyRootfs bool
				SecurityOpt    []string
			}
			NetworkSettings struct{ Networks map[string]any }
		}
		if err := json.Unmarshal([]byte(e.must(t, "inspect", e.id+"-vmnet")), &c); err != nil || len(c) != 1 {
			t.Fatalf("inspect: %v", err)
		}
		v := c[0]
		if len(v.HostConfig.CapDrop) != 1 || strings.ToUpper(v.HostConfig.CapDrop[0]) != "ALL" || !v.HostConfig.ReadonlyRootfs ||
			v.Config.User == "" || strings.HasPrefix(v.Config.User, "0") || strings.HasPrefix(v.Config.User, "root") {
			t.Fatalf("vmnet runs with caps %v, read-only %v, user %q; want cap-drop ALL, read-only, non-root",
				v.HostConfig.CapDrop, v.HostConfig.ReadonlyRootfs, v.Config.User)
		}
		if len(v.NetworkSettings.Networks) != 1 || v.NetworkSettings.Networks[e.id+"-run"] == nil {
			t.Fatalf("vmnet joins %v, want only the run network", v.NetworkSettings.Networks)
		}
		if internal := e.must(t, "network", "inspect", "-f", "{{.Internal}}", e.id+"-run"); internal != "true" {
			t.Fatalf("the run network is not internal (%s)", internal)
		}
		// From vmnet's own network namespace: the proxy answers, the origin and
		// the outside do not.
		canary := func(target string) error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, err := docker(ctx, "run", "--rm", "--name", e.id+"-canary", "--network", "container:"+e.id+"-vmnet",
				e.images["proxy"], "-egress-canary", target)
			return err
		}
		if err := canary(proxyIP + ":3128"); err != nil {
			t.Fatalf("the proxy is unreachable from vmnet's namespace: %v", err)
		}
		for _, target := range []string{originIP + ":443", "1.1.1.1:443"} {
			if canary(target) == nil {
				t.Fatalf("vmnet's namespace reaches %s", target)
			}
		}
	})

	t.Run("pinned guest keeps the contract", func(t *testing.T) {
		rows := e.boot(t, true)
		if len(rows) == 0 {
			t.Fatal("no probe rows")
		}
		for _, r := range rows {
			if !r.Pass {
				t.Errorf("row %s failed: %s", r.Row, r.Detail)
			}
		}
		// The proxy's log: each vmnet refusal is a deny tied to a flow, and no
		// flow that was refused also has an allow row.
		want := map[string]bool{"vmnet:sni-mismatch": false, "vmnet:sni-missing": false, "vmnet:literal-ip": false,
			"vmnet:udp": false, "vmnet:dropped-frame": false, "vmnet:host-mismatch": false, "policy:port": false, "policy:method": false}
		var ds []map[string]any
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			ds = e.decisions(t)
			for k := range want {
				want[k] = false
			}
			for _, d := range ds {
				if src, _ := d["rule_source"].(string); d["decision"] == "deny" {
					if _, ok := want[src]; ok {
						want[src] = true
					}
				}
			}
			if !strings.Contains(fmt.Sprint(want), "false") {
				break
			}
		}
		for k, seen := range want {
			if !seen {
				t.Errorf("no %s deny row in the proxy's log", k)
			}
		}
		allowed := map[string]bool{}
		for _, d := range ds {
			if f, _ := d["flow"].(string); f != "" && d["decision"] == "allow" {
				allowed[f] = true
			}
		}
		for _, d := range ds {
			f, _ := d["flow"].(string)
			if src, _ := d["rule_source"].(string); strings.HasPrefix(src, "vmnet:sni") && (f == "" || allowed[f]) {
				t.Errorf("refusal %v: want a flow with no allow row", d)
			}
		}
	})

	t.Run("unpinned control goes red", func(t *testing.T) {
		rows := e.boot(t, false)
		for _, name := range mustGoRed {
			r, ok := rows[name]
			if !ok {
				t.Errorf("row %s missing", name)
			} else if r.Pass {
				t.Errorf("row %s passed on unpinned networking (%s): the probe would not catch a missing pin", name, r.Detail)
			}
		}
	})
}
