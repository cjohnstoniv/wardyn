// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// maxExport caps what one share carries (the 0.10.0 ruling: 512 KiB).
const maxExport = 512 << 10

// cookie is the DevTools Network.Cookie fields a share carries. Cookies only:
// no local storage, no saved passwords, no other site's cookies.
type cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite,omitempty"`
}

// siteRe is a lower-case host name; wardynd has already reduced it to a
// registrable domain, this only refuses what could escape a file name.
var siteRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func siteArg(args []string) (string, error) {
	if len(args) != 2 || args[0] != "--site" || !siteRe.MatchString(args[1]) {
		return "", errors.New("usage: --site <registrable domain>")
	}
	return args[1], nil
}

// forSite reports whether a cookie's domain is site or one of its subdomains.
func forSite(domain, site string) bool {
	d := strings.ToLower(strings.TrimPrefix(domain, "."))
	return d == site || strings.HasSuffix(d, "."+site)
}

// filterSite keeps the cookies for site, in a canonical shape.
func filterSite(in []cookie, site string) []cookie {
	out := []cookie{}
	for _, c := range in {
		if !forSite(c.Domain, site) {
			continue
		}
		switch c.SameSite {
		case "Strict", "Lax", "None":
		default:
			c.SameSite = ""
		}
		out = append(out, c)
	}
	return out
}

// cmdExport prints the person's cookies for one site, as the person.
func cmdExport(args []string, out io.Writer) error {
	if err := checkPerson(); err != nil {
		return err
	}
	site, err := siteArg(args)
	if err != nil {
		return err
	}
	res, err := ctlCall(ctlSock, "Storage.getCookies", nil)
	if err != nil {
		return err
	}
	var got struct {
		Cookies []cookie `json:"cookies"`
	}
	if err := json.Unmarshal(res, &got); err != nil {
		return fmt.Errorf("read the browser's cookies: %w", err)
	}
	b, err := json.Marshal(filterSite(got.Cookies, site))
	if err != nil {
		return err
	}
	if len(b) > maxExport {
		return fmt.Errorf("the cookies for %s are larger than a share carries (%d KiB)", site, maxExport>>10)
	}
	_, err = out.Write(append(b, '\n'))
	return err
}

// storageState is the file the agent's browser reads: the Playwright
// storageState shape, so any browser tooling can load it.
type storageState struct {
	Cookies []stateCookie `json:"cookies"`
	Origins []any         `json:"origins"`
}

type stateCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite"`
}

func agentLoginsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".wardyn", "logins")
}

// cmdImport writes one shared site's cookies (stdin, from wardynd) into the
// agent's logins directory, as the agent. A cookie for any other site refuses
// the whole import.
func cmdImport(args []string, in io.Reader) error {
	if err := checkAgentUID(); err != nil {
		return err
	}
	site, err := siteArg(args)
	if err != nil {
		return err
	}
	var cs []cookie
	if err := json.NewDecoder(io.LimitReader(in, maxExport+1)).Decode(&cs); err != nil {
		return fmt.Errorf("read the shared cookies: %w", err)
	}
	st := storageState{Cookies: []stateCookie{}, Origins: []any{}}
	for _, c := range cs {
		if !forSite(c.Domain, site) {
			return fmt.Errorf("a shared cookie is not for %s", site)
		}
		ss := c.SameSite
		if ss != "Strict" && ss != "Lax" && ss != "None" {
			ss = "Lax"
		}
		st.Cookies = append(st.Cookies, stateCookie{c.Name, c.Value, c.Domain, c.Path, c.Expires, c.HTTPOnly, c.Secure, ss})
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	dir := agentLoginsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".import-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, site+".json")); err != nil {
		return err
	}
	fmt.Printf("%d\n", len(st.Cookies))
	return nil
}
