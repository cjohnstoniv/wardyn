// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/base64"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPeerUIDFromProcNetTCP(t *testing.T) {
	// 127.0.0.1:41234 -> 127.0.0.1:33000, owned by uid 1001; the accepted
	// side of the same connection is uid 1001 too but swapped.
	table := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:80E8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 1 1\n" +
		"   1: 0100007F:A0F2 0100007F:80E8 01 00000000:00000000 00:00000000 00000000  1000        0 2 1\n" +
		"   2: 0100007F:80E8 0100007F:A0F2 01 00000000:00000000 00:00000000 00000000  1001        0 3 1\n"
	peer := netip.MustParseAddrPort("127.0.0.1:41202")
	local := netip.MustParseAddrPort("127.0.0.1:33000")
	if got := procAddr(peer); got != "0100007F:A0F2" {
		t.Fatalf("procAddr = %s", got)
	}
	uid, err := peerUIDFrom(table, peer, local)
	if err != nil || uid != 1000 {
		t.Fatalf("peer uid = %d, %v; want the CLIENT socket's uid 1000, not the accepted side's", uid, err)
	}
	if _, err := peerUIDFrom(table, netip.MustParseAddrPort("127.0.0.1:1"), local); err == nil {
		t.Fatal("an unknown peer must not resolve")
	}
}

// fakeUpstream records the first request head it is sent.
func fakeUpstream(t *testing.T) (string, chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			br := bufio.NewReader(c)
			var head strings.Builder
			for {
				line, err := br.ReadString('\n')
				head.WriteString(line)
				if err != nil || line == "\r\n" {
					break
				}
			}
			got <- head.String()
			_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), got
}

func TestForwarderServesOnlyThePersonsUID(t *testing.T) {
	up, got := fakeUpstream(t)
	cred := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	f, err := newForwarder(up, cred)
	if err != nil {
		t.Fatal(err)
	}
	defer f.ln.Close()
	uid := 1001
	f.peerUID = func(_, _ netip.AddrPort) (int, error) { return uid, nil }
	go f.serve(1001)
	addr := f.ln.Addr().String()

	send := func(req string) string {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.WriteString(c, req)
		b, _ := io.ReadAll(c)
		return string(b)
	}

	if resp := send("CONNECT site.test:443 HTTP/1.1\r\nHost: site.test:443\r\n\r\n"); !strings.HasPrefix(resp, "HTTP/1.1 200") {
		t.Fatalf("person CONNECT answered %q", resp)
	}
	head := <-got
	want := "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("wardyn-signin:"+cred))
	if !strings.HasPrefix(head, "CONNECT site.test:443 HTTP/1.1\r\n") || !strings.Contains(head, want) {
		t.Fatalf("upstream saw %q", head)
	}

	if resp := send("GET http://site.test/cb?code=SECRET HTTP/1.1\r\nHost: site.test\r\n\r\n"); !strings.HasPrefix(resp, "HTTP/1.1 405") {
		t.Fatalf("plain HTTP answered %q, want 405", resp)
	}

	uid = 1000
	if resp := send("CONNECT site.test:443 HTTP/1.1\r\nHost: site.test:443\r\n\r\n"); resp != "" {
		t.Fatalf("the agent's uid got %q, want the connection closed unanswered", resp)
	}
	select {
	case h := <-got:
		t.Fatalf("a refused connection reached the upstream: %q", h)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSiteFilter(t *testing.T) {
	in := []cookie{
		{Name: "a", Domain: ".site.test"}, {Name: "b", Domain: "accounts.site.test"},
		{Name: "c", Domain: "othersite.test"}, {Name: "d", Domain: "site.test.evil"}, {Name: "e", Domain: "other.test"},
	}
	var names []string
	for _, c := range filterSite(in, "site.test") {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{"a", "b"}) {
		t.Fatalf("filterSite kept %v", names)
	}
	for _, bad := range [][]string{{"--site", "../x"}, {"--site", "site"}, {"--site", "Site.test"}, {"site.test"}} {
		if _, err := siteArg(bad); err == nil {
			t.Errorf("siteArg(%v) accepted", bad)
		}
	}
}

func TestImportRefusesAnotherSitesCookie(t *testing.T) {
	if os.Getuid() == personUID || os.Getuid() == 0 {
		t.Skip("runs as the agent's uid")
	}
	t.Setenv("HOME", t.TempDir())
	err := cmdImport([]string{"--site", "site.test"}, strings.NewReader(`[{"name":"x","value":"v","domain":"other.test","path":"/"}]`))
	if err == nil {
		t.Fatal("an import carrying another site's cookie must refuse")
	}
	if _, serr := os.Stat(filepath.Join(agentLoginsDir(), "site.test.json")); serr == nil {
		t.Fatal("a refused import wrote a logins file")
	}
	if err := cmdImport([]string{"--site", "site.test"}, strings.NewReader(`[{"name":"x","value":"v","domain":".site.test","path":"/","sameSite":"Weird"}]`)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(agentLoginsDir(), "site.test.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("logins file: %v %v", fi, err)
	}
	b, _ := os.ReadFile(filepath.Join(agentLoginsDir(), "site.test.json"))
	if !strings.Contains(string(b), `"sameSite":"Lax"`) || !strings.Contains(string(b), `"origins":[]`) {
		t.Fatalf("storageState = %s", b)
	}
}

func TestPauseMath(t *testing.T) {
	tasks := []task{
		{pid: 1, tid: 1, ruid: 1000, suid: 1000, state: 'S'},
		{pid: 7, tid: 7, ruid: 1000, suid: 1000, state: 'T'},
		{pid: 8, tid: 8, ruid: 1000, suid: 1000, state: 'R'},
		{pid: 8, tid: 9, ruid: 1000, suid: 1000, state: 'D'},
		{pid: 10, tid: 10, ruid: 1001, suid: 1001, state: 'S'},
		{pid: 11, tid: 11, ruid: 0, suid: 1000, state: 'S'},
		{pid: 12, tid: 12, ruid: 1000, suid: 1000, state: 'Z'},
		{pid: 99, tid: 99, ruid: 1000, suid: 1000, state: 'R'},
	}
	if got := running(tasks, 1000, 99); !slices.Equal(got, []int{1, 8, 11}) {
		t.Fatalf("running = %v", got)
	}
	if got := stoppedPIDs(tasks, 1000, 99); !slices.Equal(got, []int{7}) {
		t.Fatalf("stoppedPIDs = %v", got)
	}
	if got := alive(tasks, 1001, 99); !slices.Equal(got, []int{10}) {
		t.Fatalf("alive = %v", got)
	}
}

func TestReadState(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stat")
	_ = os.WriteFile(p, []byte("42 (a) b) T 1 42 42 0"), 0o600)
	if st, ok := readState(p); !ok || st != 'T' {
		t.Fatalf("readState = %c %v", st, ok)
	}
}

func TestStartConfigValidate(t *testing.T) {
	good := startConfig{Credential: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), Proxy: "172.18.0.2:3129"}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []startConfig{
		{Credential: "x", Proxy: good.Proxy},
		{Credential: good.Credential, Proxy: "172.18.0.2"},
		{Credential: good.Credential, Proxy: good.Proxy, URL: "http://site.test/"},
	} {
		if err := c.validate(); err == nil {
			t.Errorf("validate(%+v) accepted", c)
		}
	}
}

func TestCheckStore(t *testing.T) {
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("no /dev/shm tmpfs")
	}
	dir, err := os.MkdirTemp("/dev/shm", "signin-store-")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(dir)
	_ = os.Chmod(dir, 0o700)
	if err := checkStore(dir, os.Getuid()); err != nil {
		t.Fatalf("a 0700 tmpfs dir the uid owns must pass: %v", err)
	}
	if err := checkStore(dir, os.Getuid()+1); err == nil {
		t.Fatal("another uid's store must refuse")
	}
	_ = os.Chmod(dir, 0o750)
	if err := checkStore(dir, os.Getuid()); err == nil {
		t.Fatal("a group-readable store must refuse")
	}
	disk := t.TempDir()
	_ = os.Chmod(disk, 0o700)
	if err := checkStore(disk, os.Getuid()); err == nil && !onTmpfs(disk) {
		t.Fatal("a store on disk must refuse")
	}
	link := filepath.Join(t.TempDir(), "link")
	_ = os.Symlink(dir, link)
	if err := checkStore(link, os.Getuid()); err == nil {
		t.Fatal("a symlinked store must refuse")
	}
}

func onTmpfs(dir string) bool {
	var fs syscall.Statfs_t
	return syscall.Statfs(dir, &fs) == nil && uint32(fs.Type) == tmpfsMagic
}
