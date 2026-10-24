// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build macvm_live

package macvmlive

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

func marker() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "wardyn-live-" + hex.EncodeToString(b)
}

// helperLogTime is the time of the first helper.log line whose msg is msg.
func helperLogTime(t *testing.T, id, msgText string) (time.Time, bool) {
	t.Helper()
	f, err := os.Open(filepath.Join(runDir(t, id), macvm.HelperLog))
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l struct {
			Time time.Time `json:"time"`
			Msg  string    `json:"msg"`
		}
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.Msg == msgText {
			return l.Time, true
		}
	}
	return time.Time{}, false
}

// keepHelperLog copies a live run's helper.log into the evidence before the run dir goes.
func keepHelperLog(t *testing.T, id string) {
	t.Helper()
	if b, err := os.ReadFile(filepath.Join(runDir(t, id), macvm.HelperLog)); err == nil {
		keep(t, "helper-"+id+".log", b)
	}
}

// vzFootprintKiB sums the resident size of the Virtualization processes of this login.
func vzFootprintKiB(t *testing.T) int {
	t.Helper()
	out := mustOK(t, sh(t, 30*time.Second, "ps", "-axo", "rss=,uid=,comm="), "ps")
	uid := strconv.Itoa(os.Getuid())
	total := 0
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 3 && f[1] == uid && strings.HasPrefix(filepath.Base(strings.Join(f[2:], " ")), "com.apple.Virtualization.VirtualMachine") {
			n, _ := strconv.Atoi(f[0])
			total += n
		}
	}
	return total
}

// p4Result is what P4 leaves for P22: which strategies booted, and the tracking check.
type p4Result struct {
	Booted   map[macvm.IdentityStrategy]bool `json:"booted"`
	Tracking bool                            `json:"tracking_check_holds"`
	Why      string                          `json:"tracking_check_why"`
}

func p4Path() string { return filepath.Join(cfg.out, "p4-result.json") }

// TestP04_WarmStartAndIdentity: sequential warm starts for both identity strategies, with each
// run's configured-identifier hash and the guest's serial number and platform UUID.
func TestP04_WarmStartAndIdentity(t *testing.T) {
	live(t)
	res := p4Result{Booted: map[macvm.IdentityStrategy]bool{}}
	ids := map[macvm.IdentityStrategy][]guestIdentity{}
	base := layout(t).BaseDir(0)
	record(t, "base_physical_du", strings.Fields(sh(t, 5*time.Minute, "du", "-sh", base).out + " ?")[0])
	for _, series := range []struct {
		s macvm.IdentityStrategy
		n int
	}{{macvm.IdentityTravel, cfg.warmTravel}, {macvm.IdentityFresh, cfg.warmFresh}} {
		t.Run(string(series.s), func(t *testing.T) {
			setIdentity(t, series.s)
			var clone, hello, running []time.Duration
			for i := range series.n {
				t0 := time.Now()
				id, err := create(t, nil)
				if err != nil {
					t.Fatal(err)
				}
				helloAt, ok := helloAfter(t, id, 3*time.Minute)
				if !ok {
					keepHelperLog(t, id)
					record(t, string(series.s)+"_boots", false)
					t.Errorf("strategy %s does not boot: run %d (%s) had no guest agent hello within 3 minutes; recorded, not retried", series.s, i, id)
					end(t, id)
					return
				}
				v := wait(t, id, 2*time.Minute)
				if v.State != "RUNNING" {
					t.Fatalf("run %s is %s after hello: %s", id, v.State, v.FailureHint)
				}
				running = append(running, time.Since(t0))
				st := helperStatus(t, id)
				checkIdentity(t, st, series.s)
				// Clone time is bounded above by run.json's write (runnerd, before it starts the
				// helper) to "identity configured", which the helper logs right after the clone.
				fi, err := os.Stat(filepath.Join(runDir(t, id), macvm.RunConfigFile))
				if err != nil {
					t.Fatalf("run %s: %v", id, err)
				}
				if cfgAt, ok := helperLogTime(t, id, "macvm: identity configured"); ok {
					clone = append(clone, cfgAt.Sub(fi.ModTime()))
					hello = append(hello, helloAt.Sub(cfgAt))
				} else {
					t.Errorf("run %s: no \"identity configured\" line in helper.log", id)
				}
				g := readGuestIdentity(t, id)
				ids[series.s] = append(ids[series.s], g)
				t.Logf("RECORD run=%s strategy=%s machine_id_sha256=%s guest_serial=%s guest_uuid=%s vz_rss_kib=%d",
					id, series.s, st.MachineIDSHA256, g.Serial, g.UUID, vzFootprintKiB(t))
				keepHelperLog(t, id)
				end(t, id)
			}
			res.Booted[series.s] = true
			record(t, string(series.s)+"_clone_max", percentile(clone, 100))
			record(t, string(series.s)+"_hello_p50", percentile(hello, 50))
			record(t, string(series.s)+"_hello_p95", percentile(hello, 95))
			record(t, string(series.s)+"_create_to_running_p95", percentile(running, 95))
			if c := percentile(clone, 100); c > 2*time.Second {
				t.Errorf("clone upper bound %s (run.json to \"identity configured\") is over the 2s budget; helper.log has the detail", c)
			}
			if p := percentile(hello, 50); p > 60*time.Second {
				t.Errorf("hello p50 %s, budget 60s", p)
			}
			if p := percentile(hello, 95); p > 90*time.Second {
				t.Errorf("hello p95 %s, budget 90s", p)
			}
		})
	}
	res.Tracking, res.Why = trackingCheck(ids[macvm.IdentityTravel], ids[macvm.IdentityFresh])
	t.Logf("RECORD tracking_check_holds=%v (%s); evidence only, never qualification", res.Tracking, res.Why)
	keepJSON(t, "identities.json", ids)
	b, _ := json.MarshalIndent(res, "", "  ")
	if err := os.MkdirAll(cfg.out, 0o700); err == nil {
		_ = os.WriteFile(p4Path(), b, 0o600)
	}
}

// TestP05_AgentSession: Init, exec, output follow and a PTY over the guest's vsock. Pause and resume
// are reached through a desktop sign-in, so they are a person's step.
func TestP05_AgentSession(t *testing.T) {
	live(t)
	id := start(t, nil) // RUNNING means Init passed: the runner waits for it.
	t.Run("exec", func(t *testing.T) {
		m := marker()
		if out := mustGuest(t, id, "echo "+m+"; echo err >&2; exit 0"); !strings.Contains(out, m) {
			t.Fatalf("exec output %q lacks the marker", out)
		}
		if r := guest(t, id, "exit 7", time.Minute); r.code != 7 {
			t.Fatalf("exec exit status %d, want 7", r.code)
		}
	})
	t.Run("follow", func(t *testing.T) {
		// Output must arrive while the command still runs, not only when it ends.
		c := exec.Command(cfg.wardyn, "run", "exec", id, "--", "sh", "-c", "echo first; sleep 10; echo last")
		out, _ := c.StdoutPipe()
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t0 := time.Now()
		line, _ := bufio.NewReader(out).ReadString('\n')
		first := time.Since(t0)
		_, _ = io.Copy(io.Discard, out)
		_ = c.Wait()
		record(t, "first_line_after", first)
		if strings.TrimSpace(line) != "first" || first > 8*time.Second {
			t.Fatalf("first line %q after %s: output is not followed", line, first)
		}
	})
	t.Run("pty", func(t *testing.T) {
		// script(1) gives the CLI's ssh a terminal; the guest must report one back.
		r := sh(t, 2*time.Minute, "script", "-q", "/dev/null", cfg.wardyn, "run", "ssh", id, "--", "tty")
		if !strings.Contains(r.out, "/dev/tty") {
			t.Fatalf("no PTY in the guest: %q", r.out)
		}
	})
	t.Run("pause_resume", func(t *testing.T) {
		needPerson(t, "the automated suite's run (pause and resume need a desktop sign-in)")
		before := mustGuest(t, id, "ps -axo pid=,stat=,comm= | grep -v ' ps$'")
		keep(t, "ps-before.txt", []byte(before))
		doStep(t, fmt.Sprintf("In the console, open a desktop sign-in for run %s (it pauses the agent). Leave it open.", id))
		keep(t, "ps-paused.txt", []byte(mustGuest(t, id, "ps -axo pid=,stat=,comm=")))
		doStep(t, "End the sign-in (it resumes the agent).")
		keep(t, "ps-resumed.txt", []byte(mustGuest(t, id, "ps -axo pid=,stat=,comm=")))
		if !confirm(t, "Did the run page show the agent paused during the sign-in and running after it?") {
			t.Fatal("pause or resume did not show on the run page")
		}
	})
}

// TestP06_NetworkPositive: the fixed DHCP lease, an allowed HTTPS host, git through the broker.
func TestP06_NetworkPositive(t *testing.T) {
	live(t)
	need(t, cfg.allowedURL, "WARDYN_MACVM_ALLOWED_URL")
	intent := map[string]any{}
	if cfg.gitURL != "" {
		intent["repo"] = cfg.gitURL
	}
	id := start(t, intent)
	t.Run("lease", func(t *testing.T) {
		pkt := mustGuest(t, id, `ipconfig getpacket "$(route -n get default | awk '/interface:/{print $2}')"`)
		keep(t, "packet.txt", []byte(pkt))
		for _, want := range []string{"yiaddr = 10.0.2.15", "subnet_mask (ip): 255.255.255.0", "router (ip_mult): {10.0.2.2}", "domain_name_server (ip_mult): {10.0.2.2}"} {
			if !strings.Contains(pkt, want) {
				t.Errorf("lease lacks %q", want)
			}
		}
	})
	t.Run("allowed_https", func(t *testing.T) {
		if code := strings.TrimSpace(mustGuest(t, id, "curl -sS -o /dev/null -w '%{http_code}' --max-time 30 '"+cfg.allowedURL+"'")); code[0] != '2' && code[0] != '3' {
			t.Fatalf("allowed host answered %s", code)
		}
	})
	t.Run("git_clone", func(t *testing.T) {
		need(t, cfg.gitURL, "WARDYN_MACVM_GIT_URL")
		mustGuest(t, id, "rm -rf /tmp/p6 && git clone --depth 1 '"+cfg.gitURL+"' /tmp/p6 && git -C /tmp/p6 rev-parse HEAD")
	})
}

// listen starts a TCP or UDP listener on host:0 that answers every peer with m.
func listen(t *testing.T, network, host, m string) string {
	t.Helper()
	if network == "udp" {
		pc, err := net.ListenPacket("udp", net.JoinHostPort(host, "0"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pc.Close() })
		go func() {
			buf := make([]byte, 512)
			for {
				_, a, err := pc.ReadFrom(buf)
				if err != nil {
					return
				}
				_, _ = pc.WriteTo([]byte(m+"\n"), a)
			}
		}()
		return pc.LocalAddr().String()
	}
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte(m + "\n"))
			c.Close()
		}
	}()
	return l.Addr().String()
}

func hostReads(t *testing.T, network, addr, m string) bool {
	t.Helper()
	c, err := net.DialTimeout(network, addr, 5*time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if network == "udp" {
		_, _ = c.Write([]byte("ping"))
	}
	b := make([]byte, 128)
	n, _ := c.Read(b)
	return strings.Contains(string(b[:n]), m)
}

func runLogs(t *testing.T, id string) string {
	t.Helper()
	var all strings.Builder
	for _, name := range []string{"wardyn-vmnet-" + id, "wardyn-proxy-" + id} {
		r := sh(t, time.Minute, "docker", "logs", name)
		all.WriteString(r.out + r.err)
	}
	return all.String()
}

// TestP07_NetworkNegative: controls first, then every probe from the guest fails and is logged.
func TestP07_NetworkNegative(t *testing.T) {
	live(t)
	need(t, cfg.allowedURL, "WARDYN_MACVM_ALLOWED_URL")
	need(t, cfg.deniedHost, "WARDYN_MACVM_DENIED_HOST")
	lan := env("WARDYN_MACVM_HOST_LAN_IP", strings.TrimSpace(sh(t, 10*time.Second, "ipconfig", "getifaddr", "en0").out))
	if net.ParseIP(lan) == nil {
		t.Fatalf("no LAN IP for en0 (%q); set WARDYN_MACVM_HOST_LAN_IP", lan)
	}
	m := marker()
	tcpLAN, tcpLo, udpLAN := listen(t, "tcp", lan, m), listen(t, "tcp", "127.0.0.1", m), listen(t, "udp", lan, m)
	t.Run("controls", func(t *testing.T) {
		for _, c := range []struct{ net, addr string }{{"tcp", tcpLAN}, {"tcp", tcpLo}, {"udp", udpLAN}} {
			if !hostReads(t, c.net, c.addr, m) {
				t.Errorf("control: the host cannot reach its own %s listener %s", c.net, c.addr)
			}
		}
		for _, a := range []string{"1.1.1.1:53", "8.8.8.8:443"} {
			if c, err := net.DialTimeout("tcp", a, 5*time.Second); err != nil {
				t.Errorf("control: the host cannot reach %s: %v", a, err)
			} else {
				c.Close()
			}
		}
		c, err := net.DialTimeout("tcp", "169.254.169.254:80", 5*time.Second)
		record(t, "host_metadata_answers", err == nil)
		if err == nil {
			c.Close()
		}
	})
	id := start(t, nil)
	allowed := func(t *testing.T, when string) {
		if r := guest(t, id, "curl -sS -o /dev/null --max-time 30 '"+cfg.allowedURL+"'", time.Minute); r.code != 0 {
			t.Errorf("allowed-path control %s the probes failed: %s", when, r.err)
		}
	}
	allowed(t, "before")
	_, loPort, _ := net.SplitHostPort(tcpLo)
	_, udpPort, _ := net.SplitHostPort(udpLAN)
	tcpHost, tcpPort, _ := net.SplitHostPort(tcpLAN)
	ifc := `"$(route -n get default | awk '/interface:/{print $2}')"`
	probes := []struct {
		name, script, reached, logKey string
	}{
		{"lan_tcp", fmt.Sprintf("nc -G 5 -w 5 %s %s </dev/null", tcpHost, tcpPort), m, tcpHost},
		{"lan_udp", fmt.Sprintf("printf ping | nc -u -w 3 %s %s", tcpHost, udpPort), m, tcpHost},
		{"host_loopback_via_gateway", fmt.Sprintf("nc -G 5 -w 5 10.0.2.2 %s </dev/null", loPort), m, "10.0.2.2"},
		{"metadata", "curl -sS --max-time 5 -o /dev/null -w 'HTTP%{http_code}' http://169.254.169.254/", "HTTP[1-5]", "169.254.169.254"},
		{"dns_direct_udp", "dig @1.1.1.1 example.com +time=3 +tries=1", "ANSWER SECTION", "1.1.1.1"},
		{"dns_direct_tcp", "dig +tcp @1.1.1.1 example.com +time=3 +tries=1", "ANSWER SECTION", "1.1.1.1"},
		{"https_direct_ip", "curl -sk --max-time 5 -o /dev/null -w 'HTTP%{http_code}' https://8.8.8.8/", "HTTP[1-5]", "8.8.8.8"},
		{"ipv6_global", "curl -6 -sk --max-time 5 -o /dev/null -w 'HTTP%{http_code}' 'https://[2606:4700:4700::1111]/'", "HTTP[1-5]", "ipv6"},
		{"ipv6_link_local", "ping6 -c 2 -i 1 ff02::1%" + ifc + " | grep 'bytes from' | grep -v \"$(ifconfig " + ifc + " | awk '/inet6 fe80/{print $2}' | cut -d% -f1)\"", "bytes from", "ipv6"},
		{"denied_host", "curl -sS --max-time 10 -o /dev/null -w 'HTTP%{http_code}' 'https://" + cfg.deniedHost + "/'", "HTTP[23]", cfg.deniedHost},
	}
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			r := guest(t, id, p.script, time.Minute)
			if regexp.MustCompile(p.reached).MatchString(r.out) {
				t.Fatalf("probe reached its target: %q", strings.TrimSpace(r.out))
			}
		})
	}
	allowed(t, "after")
	logs := runLogs(t, id)
	keep(t, "relay-and-proxy.log", []byte(logs))
	for _, p := range probes {
		if !strings.Contains(strings.ToLower(logs), strings.ToLower(p.logKey)) {
			t.Errorf("probe %s failed with no relay or proxy line naming %s: unexplained, which fails P7 until explained", p.name, p.logKey)
		}
	}
	t.Run("one_filehandle_nic", func(t *testing.T) {
		var d struct {
			NICs []struct {
				Type string `json:"type"`
			} `json:"nics"`
		}
		out := mustOK(t, sh(t, time.Minute, cfg.helper, assumedDescribe(runDir(t, id))...), "wardyn-macvm describe")
		keep(t, "describe.json", []byte(out))
		if err := json.Unmarshal([]byte(out), &d); err != nil {
			t.Fatalf("describe JSON: %v", err)
		}
		if len(d.NICs) != 1 || !strings.EqualFold(d.NICs[0].Type, "filehandle") {
			t.Fatalf("NICs %+v, want exactly one FileHandle", d.NICs)
		}
	})
}

// TestP08_Throughput: the same object from an allowed host in a VM run and in a container run.
func TestP08_Throughput(t *testing.T) {
	live(t)
	need(t, cfg.bigURL, "WARDYN_MACVM_1G_URL")
	speed := func(t *testing.T, kind string) float64 {
		id := start(t, map[string]any{"machine_kind": kind})
		out := strings.TrimSpace(mustGuest(t, id, "curl -sS -o /dev/null -w '%{speed_download}' --max-time 1200 '"+cfg.bigURL+"'"))
		end(t, id)
		bps, err := strconv.ParseFloat(out, 64)
		if err != nil {
			t.Fatalf("speed %q: %v", out, err)
		}
		return bps * 8 / 1e6
	}
	vm, ctr := speed(t, "macos_vm"), speed(t, "container")
	record(t, "vm_mbit_s", fmt.Sprintf("%.0f", vm))
	record(t, "container_mbit_s", fmt.Sprintf("%.0f", ctr))
	if want := min(200, 0.8*ctr); vm < want {
		t.Errorf("LIMIT: the VM run downloaded at %.0f Mbit/s, below %.0f (min of 200 and 80%% of the container's %.0f)", vm, want, ctr)
	}
}
