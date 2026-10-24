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

// helperLogTime is the time of the first helper.log line whose msg is msgText.
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

// p4Path is where P4 leaves its result for P22, in the window's evidence dir.
func p4Path() string { return filepath.Join(cfg.evidence, "p4-result.json") }

// warmSeries starts n runs one after another, checks the budgets, and returns each guest's
// identity. want is the strategy the runs must have been configured with ("" takes the runner's).
func warmSeries(t *testing.T, n int, want macvm.IdentityStrategy) ([]guestIdentity, bool) {
	t.Helper()
	var clone, hello, running []time.Duration
	var ids []guestIdentity
	for i := range n {
		t0 := time.Now()
		id, err := create(t, spec{})
		if err != nil {
			t.Fatal(err)
		}
		helloAt, ok := helloAfter(t, id, 3*time.Minute)
		if !ok {
			keepHelperLog(t, id)
			t.Errorf("does not boot: run %d (%s) had no guest agent hello within 3 minutes; recorded, not retried", i, id)
			end(t, id)
			return ids, false
		}
		v := wait(t, id, 2*time.Minute)
		if v.State != "RUNNING" {
			t.Fatalf("run %s is %s after hello: %s", id, v.State, v.FailureHint)
		}
		pinned(t, v)
		running = append(running, time.Since(t0))
		st := helperStatus(t, id)
		if want != "" {
			checkIdentity(t, st, want)
		}
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
		ids = append(ids, g)
		t.Logf("RECORD run=%s identity=%s machine_id_sha256=%s guest_serial=%s guest_uuid=%s vz_rss_kib=%d",
			id, st.Identity, st.MachineIDSHA256, g.Serial, g.UUID, vzFootprintKiB(t))
		keepHelperLog(t, id)
		end(t, id)
	}
	record(t, "clone_upper_bound_max", percentile(clone, 100))
	record(t, "hello_p50", percentile(hello, 50))
	record(t, "hello_p95", percentile(hello, 95))
	record(t, "create_to_running_p95", percentile(running, 95))
	if c := percentile(clone, 100); c > 2*time.Second {
		t.Errorf("clone upper bound %s (run.json to \"identity configured\") is over the 2s budget; helper.log has the detail", c)
	}
	if p := percentile(hello, 50); p > 60*time.Second {
		t.Errorf("hello p50 %s, budget 60s", p)
	}
	if p := percentile(hello, 95); p > 90*time.Second {
		t.Errorf("hello p95 %s, budget 90s", p)
	}
	return ids, true
}

// TestP04_WarmStartAndIdentity: sequential warm starts for both identity strategies, with each
// run's configured-identifier hash and the guest's serial number and platform UUID.
func TestP04_WarmStartAndIdentity(t *testing.T) {
	live(t)
	res := p4Result{Booted: map[macvm.IdentityStrategy]bool{}}
	ids := map[macvm.IdentityStrategy][]guestIdentity{}
	record(t, "base_physical_du", strings.Fields(sh(t, 5*time.Minute, "du", "-sh", layout(t).BaseDir(0)).out + " ?")[0])
	defer func() {
		res.Tracking, res.Why = trackingCheck(ids[macvm.IdentityTravel], ids[macvm.IdentityFresh])
		t.Logf("RECORD tracking_check_holds=%v (%s); evidence only, never qualification", res.Tracking, res.Why)
		keepJSON(t, "identities.json", ids)
		b, _ := json.MarshalIndent(res, "", "  ")
		_ = os.MkdirAll(cfg.evidence, 0o700)
		if err := os.WriteFile(p4Path(), b, 0o600); err != nil {
			t.Errorf("p4 result: %v", err)
		}
	}()
	if cfg.identityCmd == "" {
		// No switch: the budgets are still measured under whatever strategy the runner uses.
		t.Run("runner_strategy", func(t *testing.T) { warmSeries(t, cfg.warmTravel, "") })
	}
	for _, series := range []struct {
		s macvm.IdentityStrategy
		n int
	}{{macvm.IdentityTravel, cfg.warmTravel}, {macvm.IdentityFresh, cfg.warmFresh}} {
		t.Run(string(series.s), func(t *testing.T) {
			setIdentity(t, series.s)
			g, booted := warmSeries(t, series.n, series.s)
			ids[series.s], res.Booted[series.s] = g, booted
			record(t, string(series.s)+"_boots", booted)
		})
	}
}

// TestP05_AgentSession: Init, exec and output follow over the guest's vsock. Pause and resume are
// reached through a desktop sign-in, so they are a person's step.
func TestP05_AgentSession(t *testing.T) {
	live(t)
	block(t)
	id := start(t, spec{}) // RUNNING means Init passed: the runner waits for it.
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
		// The console's terminal door: `wardyn attach` mints the same attach ticket the console's
		// terminal uses and, with piped input, sends it and exits once the shell does.
		probe := `echo "TTY=$(tty) SIZE=$(stty size)"`
		r := shIn(t, 2*time.Minute, []byte(probe+"\nexit\n"), cfg.wardyn, "attach", id)
		keep(t, "attach.txt", []byte(r.out+"\n--- stderr\n"+r.err))
		got := r.out
		if r.code != 0 || !ptyLine.MatchString(got) {
			t.Logf("RECORD attach_path=no pty line (exit %d): %s", r.code, clean(r.err))
			if !cfg.prompts {
				t.Fatalf("`wardyn attach` showed no pty (exit %d) and no person can open the console's terminal: %s", r.code, clean(r.err+" "+r.out))
			}
			got = ask(t, fmt.Sprintf("In the console, open run %s's terminal, run exactly: %s  and paste the line it prints:", id, probe))
		}
		m := ptyLine.FindStringSubmatch(got)
		if m == nil {
			t.Fatalf("the terminal is not a pty: %q", clean(got))
		}
		record(t, "pty", m[1])
		record(t, "pty_size", m[2])
		t.Log("RECORD pty_size is a record only: SIZE=0 0 (a piped client sends no window size) is accepted")
	})
	t.Run("pause_resume", func(t *testing.T) {
		needPerson(t, "the automated suite's run (pause and resume need a desktop sign-in)")
		keep(t, "ps-before.txt", []byte(mustGuest(t, id, "ps -axo pid=,stat=,comm=")))
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
	block(t)
	need(t, cfg.allowedURL, "WARDYN_MACVM_ALLOWED_URL")
	intent := map[string]any{}
	if cfg.gitURL != "" {
		intent["repo"] = cfg.gitURL
	}
	id := start(t, spec{intent: intent})
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
		code := strings.TrimSpace(mustGuest(t, id, "curl -sS -o /dev/null -w '%{http_code}' --max-time 30 '"+cfg.allowedURL+"'"))
		if !regexp.MustCompile(`^[23]\d\d$`).MatchString(code) {
			t.Fatalf("allowed host answered %q", code)
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
		if r.code != 0 {
			t.Fatalf("docker logs %s: exit %d: %s", name, r.code, r.err)
		}
		all.WriteString(r.out + r.err)
	}
	return all.String()
}

// probe is one negative probe from the guest. Its script runs inside a wrapper that prints the
// probe's own exit status, so a failed exec is never read as a refused probe.
type probe struct {
	name, script string
	reached      string // stdout that means the target answered
	refused      string // output or status text that shows the guest-side refusal or timeout
	logIP        string // the probe's own destination, which its log line must name...
	logPort      string // ...with its own port ("" for ICMP or a named host)
	udp          bool   // the relay logs no UDP: guest-side evidence only
}

// ptyLine is the terminal probe's answer: a real pty and its size.
var ptyLine = regexp.MustCompile(`TTY=(/dev/ttys?\d+) SIZE=(\d+ \d+)`)

var probeRC = regexp.MustCompile(`(?m)^PROBE_RC=(\d+)$`)

// logExplains reports whether one log line names the probe's own destination (and port).
func logExplains(logs string, p probe) bool {
	ip := regexp.MustCompile(`(^|[^0-9a-fA-F:.])` + regexp.QuoteMeta(p.logIP) + `($|[^0-9a-fA-F:.])`)
	port := regexp.MustCompile(`(^|\D)` + regexp.QuoteMeta(p.logPort) + `($|\D)`)
	for _, l := range strings.Split(logs, "\n") {
		if ip.MatchString(l) && (p.logPort == "" || port.MatchString(l)) {
			return true
		}
	}
	return false
}

// TestP07_NetworkNegative: controls first, then every probe from the guest fails on the guest's
// side and, for TCP, IPv6 and named hosts, a relay or proxy line names that probe's own flow.
func TestP07_NetworkNegative(t *testing.T) {
	live(t)
	block(t)
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
	id := start(t, spec{})
	t.Run("tools", func(t *testing.T) {
		mustGuest(t, id, "for c in nc dig curl ping6 route ifconfig; do command -v $c >/dev/null || { echo missing $c >&2; exit 1; }; done")
	})
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
	// The guest's own link-local address, so its own echo reply is not read as a neighbour's.
	own := strings.TrimSpace(guest(t, id, "ifconfig "+ifc+" | awk '/inet6 fe80/{print $2; exit}' | cut -d% -f1", time.Minute).out)
	if !regexp.MustCompile(`^fe80:[0-9a-f:]+$`).MatchString(own) {
		own = ""
	}
	curlRefused := `curl: \((6|7|28|35|52|56)\)|Failed to connect|timed out|Connection refused|CONNECT tunnel failed|reset by peer|Empty reply`
	ncRefused := `Connection refused|Operation timed out|timed out|failed|No route to host`
	probes := []probe{
		{"lan_tcp", fmt.Sprintf("nc -v -G 5 -w 5 %s %s </dev/null", tcpHost, tcpPort), m, ncRefused, tcpHost, tcpPort, false},
		{"lan_udp", fmt.Sprintf("printf ping | nc -u -w 3 %s %s; echo udp-done", tcpHost, udpPort), m, "udp-done", tcpHost, udpPort, true},
		{"host_loopback_via_gateway", fmt.Sprintf("nc -v -G 5 -w 5 10.0.2.2 %s </dev/null", loPort), m, ncRefused, "10.0.2.2", loPort, false},
		{"metadata", "curl -sS --max-time 5 -o /dev/null -w 'HTTP%{http_code}' http://169.254.169.254/", "HTTP[1-5]", curlRefused, "169.254.169.254", "80", false},
		{"dns_direct_udp", "dig @1.1.1.1 example.com +time=3 +tries=1", "ANSWER SECTION", "connection timed out|no servers could be reached|communications error", "1.1.1.1", "53", true},
		{"dns_direct_tcp", "dig +tcp @1.1.1.1 example.com +time=3 +tries=1", "ANSWER SECTION", "connection timed out|no servers could be reached|communications error|connection refused", "1.1.1.1", "53", false},
		{"https_direct_ip", "curl -sk --max-time 5 -o /dev/null -w 'HTTP%{http_code}' https://8.8.8.8/", "HTTP[1-5]", curlRefused, "8.8.8.8", "443", false},
		{"ipv6_global", "curl -6 -sSk --max-time 5 -o /dev/null -w 'HTTP%{http_code}' 'https://[2606:4700:4700::1111]/'", "HTTP[1-5]", curlRefused + `|Could not resolve|Network is unreachable|No route`, "2606:4700:4700::1111", "443", false},
		{"ipv6_link_local", "ping6 -c 2 -i 1 ff02::1%" + ifc + " | grep 'bytes from' | grep -v '" + own + "'; echo ll-done", "bytes from", "ll-done", "ff02::1", "", false},
		{"denied_host", "curl -sS --max-time 10 -o /dev/null -w 'HTTP%{http_code}' 'https://" + cfg.deniedHost + "/'", "HTTP[23]", curlRefused + `|HTTP403`, cfg.deniedHost, "", false},
	}
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			if p.name == "ipv6_link_local" && own == "" {
				notRun(t, "the guest shows no link-local address of its own, so a neighbour's reply cannot be told from its own")
			}
			r := guest(t, id, "out=$( ("+p.script+") 2>&1); rc=$?; printf 'PROBE_RC=%d\\n%s\\n' $rc \"$out\"", time.Minute)
			mm := probeRC.FindStringSubmatch(r.out)
			if r.code != 0 || mm == nil {
				t.Fatalf("the exec failed (exit %d), so the probe proves nothing: %s", r.code, strings.TrimSpace(r.err))
			}
			keep(t, "probe.txt", []byte(r.out))
			if regexp.MustCompile(p.reached).MatchString(probeRC.ReplaceAllString(r.out, "")) {
				t.Fatalf("probe reached its target: %q", strings.TrimSpace(r.out))
			}
			if !regexp.MustCompile(p.refused).MatchString(r.out) {
				t.Fatalf("probe exit %s with no refusal or timeout in its output: %q", mm[1], strings.TrimSpace(r.out))
			}
			if p.udp {
				t.Log("EVIDENCE: guest-side only (the relay logs no UDP)")
			}
		})
	}
	allowed(t, "after")
	t.Run("logged", func(t *testing.T) {
		logs := runLogs(t, id)
		keep(t, "relay-and-proxy.log", []byte(logs))
		for _, p := range probes {
			if !p.udp && !logExplains(logs, p) {
				t.Errorf("probe %s: no relay or proxy line names %s %s: unexplained, which fails P7 until explained", p.name, p.logIP, p.logPort)
			}
		}
	})
	t.Run("one_filehandle_nic", func(t *testing.T) {
		untested(t, "the NIC kind is not observable from outside the helper")
	})
}

// TestP08_Throughput: the same object from an allowed host in a VM run and in a container run, both
// on the window's runner.
func TestP08_Throughput(t *testing.T) {
	live(t)
	need(t, cfg.bigURL, "WARDYN_MACVM_1G_URL")
	speed := func(t *testing.T, kind string) float64 {
		id := start(t, spec{intent: map[string]any{"machine_kind": kind}})
		out := mustGuest(t, id, "curl -sS -o /dev/null -w '%{http_code} %{speed_download}' --max-time 1200 '"+cfg.bigURL+"'")
		end(t, id)
		var code int
		var bps float64
		if _, err := fmt.Sscan(out, &code, &bps); err != nil || code/100 != 2 {
			t.Fatalf("download %q: %v", out, err)
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
