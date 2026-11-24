// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// POST /runs/{id}/attach/ticket with a purpose (attach_ticket_purpose.go): an SSH ticket for GET /runs/{id}/ssh, or
// an attach-forward ticket for the forward door, each bound to the one door it names.

const (
	purposeOwner   = "sub-pm"
	purposeSponsor = "sub-sponsor"
)

// purposeRig is a person's running run on a server whose SSH gateway and tickets are on (the gateway's config only:
// the mint reads it, nothing here serves SSH), with capability reads from cs.
type purposeRig struct {
	srv *Server
	ast *authzStore
	cs  *capStore
	run types.AgentRun
}

// purposeSponsorStore is the rig's store with the service identity reads: sponsor sponsors every service identity.
type purposeSponsorStore struct {
	*capStore
	store.ServiceIdentityStore
	sponsor string
}

func (s purposeSponsorStore) GetServiceIdentity(_ context.Context, name string) (types.ServiceIdentity, error) {
	return types.ServiceIdentity{Name: name, Sponsor: s.sponsor}, nil
}

func (s purposeSponsorStore) ServiceStillSponsored(_ context.Context, _, sponsor string, _ time.Time) (bool, error) {
	return sponsor == s.sponsor, nil
}

func newPurposeRig(t *testing.T, cs *capStore, configure func(*Config)) *purposeRig {
	t.Helper()
	if cs == nil {
		cs = &capStore{}
	}
	ast := newAuthzStore()
	cs.Store = ast
	if cs.userTypes == nil {
		cs.userTypes = utKnown
	}
	cfg := baseTestConfig(newHarness(t), purposeSponsorStore{capStore: cs, sponsor: purposeSponsor})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Audit = &recRecorder{}
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SSHListenAddr, cfg.SSHHostKey, cfg.SSHTickets = "127.0.0.1:0", hostKey, true
	if configure != nil {
		configure(&cfg)
	}
	pr := &purposeRig{srv: New(cfg), ast: ast, cs: cs}
	pr.run = pr.addRun(purposeOwner, nil)
	return pr
}

func (pr *purposeRig) addRun(createdBy string, mutate func(*types.AgentRun)) types.AgentRun {
	run := types.AgentRun{ID: uuid.New(), CreatedBy: createdBy, State: types.RunRunning, SandboxRef: "sbx-1"}
	if mutate != nil {
		mutate(&run)
	}
	pr.ast.mu.Lock()
	pr.ast.runs[run.ID] = run
	pr.ast.mu.Unlock()
	return run
}

func purposeCookie(t *testing.T, sub, role string) *http.Cookie {
	t.Helper()
	return ssoSessionOfType(t, sub, sub+"@corp.example", role, utPM)
}

// mint asks for a ticket on run as cookie's person, with query.
func (pr *purposeRig) mint(t *testing.T, cookie *http.Cookie, run uuid.UUID, query string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, pr.srv, http.MethodPost, "/api/v1/runs/"+run.String()+"/attach/ticket?"+query, cookie, "")
}

func (pr *purposeRig) tickets() map[string]store.AttachTicket {
	pr.ast.mu.Lock()
	defer pr.ast.mu.Unlock()
	out := map[string]store.AttachTicket{}
	for k, v := range pr.ast.tickets {
		out[k] = v
	}
	return out
}

// answer decodes a mint's body: the ticket and the echoed purpose on a 200, the reason and feature on a refusal.
func answer(t *testing.T, w *httptest.ResponseRecorder) (ticket, purpose, reason, feature string) {
	t.Helper()
	var b struct {
		Ticket  string `json:"ticket"`
		Purpose string `json:"purpose"`
		Reason  string `json:"reason"`
		Feature string `json:"feature"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	return b.Ticket, b.Purpose, b.Reason, b.Feature
}

// purposeMintRow is one value row of the mint: the request and what it answers, and the ticket stored on a 200.
type purposeMintRow struct {
	name      string
	cookie    func(*testing.T) *http.Cookie
	run       func(*purposeRig) uuid.UUID
	query     string
	configure func(*Config)
	forwardOn bool
	status    int
	reason    string
	purpose   string
	ports     []int
}

func ownerCookie(t *testing.T) *http.Cookie { return purposeCookie(t, purposeOwner, oidc.RoleUser) }

func (pr *purposeRig) ownRun() uuid.UUID { return pr.run.ID }

func TestAttachTicketPurpose_Mint(t *testing.T) {
	serviceRun := func(pr *purposeRig) uuid.UUID { return pr.addRun(types.ServicePrincipalPrefix+"bot", nil).ID }
	backgroundRun := func(pr *purposeRig) uuid.UUID {
		return pr.addRun(purposeOwner, func(r *types.AgentRun) { r.Experience = types.ExperienceBackground }).ID
	}
	sponsorCookie := func(t *testing.T) *http.Cookie { return purposeCookie(t, purposeSponsor, oidc.RoleUser) }
	ticketsOff := func(c *Config) { c.SSHTickets = false }
	gatewayOff := func(c *Config) { c.SSHListenAddr = "" }
	sftpOff := func(c *Config) { c.SSHChannels = map[string]bool{"shell": true, "exec": true, "forward": true} }
	forwardOff := func(c *Config) { c.SSHChannels = map[string]bool{"shell": true, "exec": true, "sftp": true} }
	shellOff := func(c *Config) { c.SSHChannels = map[string]bool{"exec": true, "sftp": true, "forward": true} }
	seventeen := make([]string, 17)
	for i := range seventeen {
		seventeen[i] = fmt.Sprint(3000 + i)
	}
	for _, row := range []purposeMintRow{
		// The SSH purposes and the mint's own refusals.
		{name: "tickets off", query: "purpose=ssh-exec", configure: ticketsOff, status: 409, reason: reasonSSHTicketsOff},
		{name: "ssh-exec", query: "purpose=ssh-exec", status: 200, purpose: ticketPurposeSSHExec},
		{name: "ssh-editor", query: "purpose=ssh-editor", status: 200, purpose: ticketPurposeSSHEditor},
		{name: "ssh-editor with sftp off", query: "purpose=ssh-editor", configure: sftpOff, status: 409, reason: reasonSSHTicketChannelOff},
		{name: "ssh-shell", query: "purpose=ssh-shell", status: 200, purpose: ticketPurposeSSHShell},
		{name: "ssh-shell with shell off", query: "purpose=ssh-shell", configure: shellOff, status: 409, reason: reasonSSHTicketChannelOff},
		{name: "ssh-shell with ports", query: "purpose=ssh-shell&ports=22", status: 400, reason: reasonAttachTicketPortsInvalid},
		{name: "ssh-sync is not a purpose yet", query: "purpose=ssh-sync", status: 400, reason: reasonAttachTicketPurposeInvalid},
		{name: "ssh-sync-git is not a purpose yet", query: "purpose=ssh-sync-git", status: 400, reason: reasonAttachTicketPurposeInvalid},
		{name: "ssh-forward with forward off", query: "purpose=ssh-forward&ports=3000", configure: forwardOff, status: 409, reason: reasonSSHTicketChannelOff},
		{name: "ssh-forward without ports", query: "purpose=ssh-forward", status: 400, reason: reasonAttachTicketPortsInvalid},
		{name: "ssh-forward with ports", query: "purpose=ssh-forward&ports=3000,5000", status: 200, purpose: ticketPurposeSSHForward, ports: []int{3000, 5000}},
		{name: "ssh-forward with a repeated port", query: "purpose=ssh-forward&ports=3000,3000", status: 400, reason: reasonAttachTicketPortsInvalid},
		{name: "ssh-forward with port 0", query: "purpose=ssh-forward&ports=0", status: 400, reason: reasonAttachTicketPortsInvalid},
		{name: "ssh-forward with a signed port", query: "purpose=ssh-forward&ports=%2B80", status: 400, reason: reasonAttachTicketPortsInvalid},
		{name: "ssh-forward with 17 ports", query: "purpose=ssh-forward&ports=" + strings.Join(seventeen, ","), status: 400, reason: reasonAttachTicketPortsInvalid},
		{name: "ssh-exec with ports", query: "purpose=ssh-exec&ports=22", status: 400, reason: reasonAttachTicketPortsInvalid},
		{name: "ssh-bogus", query: "purpose=ssh-bogus", status: 400, reason: reasonAttachTicketPurposeInvalid},
		{name: "gateway off", query: "purpose=ssh-exec", configure: gatewayOff, status: 409, reason: reasonSSHGatewayOff},
		{name: "sponsor ssh-exec", cookie: sponsorCookie, run: serviceRun, query: "purpose=ssh-exec", status: 403, reason: runOwnerOnlyReason},
		{name: "member on a foreign run", cookie: func(t *testing.T) *http.Cookie { return purposeCookie(t, "sub-other", oidc.RoleUser) },
			query: "purpose=ssh-exec", status: 404, reason: reasonRunNotFound},
		{name: "admin on a person's run", cookie: func(t *testing.T) *http.Cookie { return purposeCookie(t, "sub-admin", oidc.RoleAdmin) },
			query: "purpose=ssh-exec", status: 403, reason: runOwnerOnlyReason},
		{name: "background run", run: backgroundRun, query: "purpose=ssh-exec", status: 409, reason: reasonRunBackgroundOnly},
		// The forward purpose.
		{name: "attach-forward", query: "purpose=attach-forward&ports=3000", forwardOn: true, status: 200, purpose: ticketPurposeAttachForward, ports: []int{3000}},
		{name: "attach-forward with the switch off", query: "purpose=attach-forward&ports=3000", status: 409, reason: reasonRunForwardOff},
		{name: "attach-forward with forward left out", query: "purpose=attach-forward&ports=3000", configure: forwardOff, forwardOn: true, status: 409, reason: reasonRunForwardOff},
		{name: "attach-forward without ports", query: "purpose=attach-forward", forwardOn: true, status: 400, reason: reasonAttachTicketPortsInvalid},
		{name: "ports on ui", query: "purpose=ui&ports=3000", status: 400, reason: reasonAttachTicketPortsInvalid},
		{name: "ports on a terminal ticket", query: "ports=3000", status: 400, reason: reasonAttachTicketPortsInvalid},
		{name: "sponsor attach-forward", cookie: sponsorCookie, run: serviceRun, query: "purpose=attach-forward&ports=3000", forwardOn: true, status: 403, reason: runOwnerOnlyReason},
		// The unchanged paths: a terminal, a UI and any other purpose mint as before.
		{name: "terminal ticket", query: "", status: 200},
		{name: "ui ticket", query: "purpose=ui", status: 200},
		{name: "another purpose is a terminal ticket", query: "purpose=notebook", status: 200},
		{name: "sponsor terminal ticket", cookie: sponsorCookie, run: serviceRun, query: "", status: 200},
	} {
		t.Run(row.name, func(t *testing.T) {
			if row.forwardOn {
				t.Setenv(envAttachForward, "on")
			} else {
				t.Setenv(envAttachForward, "")
			}
			sponsorAllow := grant(types.CapabilitySubjectUser, purposeSponsor, capFeature, featureServiceAttach, types.CapabilityAllow)
			pr := newPurposeRig(t, &capStore{grants: []types.CapabilityGrant{sponsorAllow}}, row.configure)
			cookie, run := ownerCookie, (*purposeRig).ownRun
			if row.cookie != nil {
				cookie = row.cookie
			}
			if row.run != nil {
				run = row.run
			}
			w := pr.mint(t, cookie(t), run(pr), row.query)
			tok, echo, reason, _ := answer(t, w)
			if w.Code != row.status || reason != row.reason {
				t.Fatalf("mint ?%s = %d %s, want %d %q", row.query, w.Code, w.Body, row.status, row.reason)
			}
			stored := pr.tickets()
			if row.status != 200 {
				if len(stored) != 0 {
					t.Fatalf("a refused mint stored %d ticket(s)", len(stored))
				}
				return
			}
			got, ok := stored[tok]
			if !ok || len(stored) != 1 {
				t.Fatalf("stored %v, want the one ticket answered", stored)
			}
			if got.Purpose != row.purpose || !slices.Equal(got.Ports, row.ports) || echo != row.purpose {
				t.Errorf("stored purpose %q ports %v, echo %q; want %q %v and the same echo", got.Purpose, got.Ports, echo, row.purpose, row.ports)
			}
		})
	}
}

// TestAttachTicketPurpose_UnsupportedCallers: a person through a portal
// gets no SSH or forward ticket, and a device (or any actor that is not a person) no SSH ticket.
func TestAttachTicketPurpose_UnsupportedCallers(t *testing.T) {
	t.Setenv(envAttachForward, "on")
	pr := newPurposeRig(t, nil, nil)
	via := &types.DelegationVia{Grant: uuid.New(), Delegate: uuid.New()}
	person := ticketActor{actorType: types.ActorHuman, principal: purposeOwner}
	portal := person
	portal.via = via
	for _, tc := range []struct {
		name, query string
		ctx         func(context.Context) context.Context
		status      int
		reason      string
	}{
		{"portal ssh-exec", "purpose=ssh-exec", func(c context.Context) context.Context { return withTicketActor(c, portal) }, 403, reasonSSHTicketUnsupportedCaller},
		{"portal attach-forward", "purpose=attach-forward&ports=3000", func(c context.Context) context.Context { return withTicketActor(c, portal) }, 403, reasonAttachTicketNotYourRun},
		{"device ssh-exec", "purpose=ssh-exec", func(c context.Context) context.Context {
			return context.WithValue(c, deviceCtxKey{}, types.Device{ID: uuid.New()})
		}, 403, reasonSSHTicketUnsupportedCaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+pr.run.ID.String()+"/attach/ticket?"+tc.query, nil)
			req = req.WithContext(tc.ctx(req.Context()))
			w := httptest.NewRecorder()
			_, ok := pr.srv.mintScope(w, req, req.URL.Query().Get("purpose"))
			if ok || w.Code != tc.status || wireReason(t, w) != tc.reason {
				t.Fatalf("mintScope = ok %v %d %s, want %d %s", ok, w.Code, w.Body, tc.status, tc.reason)
			}
		})
	}
}

// TestAttachTicketPurpose_HelperMints: a helper under a live grant gets no SSH or forward
// ticket, with every switch on: the support arm knows no such door.
func TestAttachTicketPurpose_HelperMints(t *testing.T) {
	for _, query := range []string{"?purpose=ssh-exec", "?purpose=ssh-editor", "?purpose=attach-forward&ports=3000"} {
		t.Run(query, func(t *testing.T) {
			rig := newSupportTermRig(t, "terminal")
			t.Setenv(envAttachForward, "on")
			_, hostKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			rig.srv.cfg.SSHListenAddr, rig.srv.cfg.SSHHostKey, rig.srv.cfg.SSHTickets = "127.0.0.1:0", hostKey, true
			w := rig.mint(rig.helper, query)
			if w.Code != http.StatusForbidden || wireReason(t, w) != string(authz.ReasonSupportDoorNotGranted) || rig.ticketCount() != 0 {
				t.Fatalf("helper mint %s = %d %s with %d tickets, want 403 support_door_not_granted and none", query, w.Code, w.Body, rig.ticketCount())
			}
		})
	}
}

// TestAttachTicketPurpose_SSHKeyCapability: with tickets on, an SSH mint asks ssh_key as a key's
// registration does, and a person a deny covers gets the key door's 403 and row; an attach-forward mint is not asked;
// with tickets off the switch answers first.
func TestAttachTicketPurpose_SSHKeyCapability(t *testing.T) {
	t.Setenv(envAttachForward, "on")
	denied := func() *capStore {
		return &capStore{enf: map[string]bool{capFeature: true}, grants: []types.CapabilityGrant{
			grant(types.CapabilitySubjectUser, purposeOwner, capFeature, featureSSHKey, types.CapabilityDeny),
		}}
	}
	t.Run("ssh-exec is refused", func(t *testing.T) {
		pr := newPurposeRig(t, denied(), nil)
		w := pr.mint(t, ownerCookie(t), pr.run.ID, "purpose=ssh-exec")
		_, _, reason, feature := answer(t, w)
		if w.Code != http.StatusForbidden || reason != "capability_feature" || feature != featureSSHKey {
			t.Fatalf("mint = %d %s, want 403 capability_feature for ssh_key", w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), sshTicketFeatureRefusal) {
			t.Errorf("body = %s, want %q", w.Body, sshTicketFeatureRefusal)
		}
		var targets []string
		for _, ev := range pr.srv.cfg.Audit.(*recRecorder).snapshot() {
			if ev.Action == "authz.denied" {
				targets = append(targets, ev.Target)
			}
		}
		if want := []string{"runs.ssh_ticket"}; !slices.Equal(targets, want) || !slices.Equal(auditReasons(t, pr.srv, "authz.denied"), []string{"capability_feature"}) {
			t.Errorf("authz.denied targets %v reasons %v, want one capability_feature row at runs.ssh_ticket", targets, auditReasons(t, pr.srv, "authz.denied"))
		}
		if n := len(pr.tickets()); n != 0 {
			t.Errorf("a refused mint stored %d ticket(s)", n)
		}
	})
	t.Run("attach-forward is not asked", func(t *testing.T) {
		pr := newPurposeRig(t, denied(), nil)
		if w := pr.mint(t, ownerCookie(t), pr.run.ID, "purpose=attach-forward&ports=3000"); w.Code != http.StatusOK {
			t.Fatalf("attach-forward mint = %d %s, want 200", w.Code, w.Body)
		}
	})
	t.Run("tickets off answers first", func(t *testing.T) {
		pr := newPurposeRig(t, denied(), func(c *Config) { c.SSHTickets = false })
		w := pr.mint(t, ownerCookie(t), pr.run.ID, "purpose=ssh-exec")
		if w.Code != http.StatusConflict || wireReason(t, w) != reasonSSHTicketsOff {
			t.Fatalf("mint = %d %s, want 409 ssh_tickets_off", w.Code, w.Body)
		}
	})
}

// TestAttachTicketPurpose_SkipsTerminalCap: with every terminal slot held, an attach-forward and an
// SSH ticket still mint (neither holds a terminal slot), and a terminal ticket is refused as before.
func TestAttachTicketPurpose_SkipsTerminalCap(t *testing.T) {
	t.Setenv(envAttachForward, "on")
	pr := newPurposeRig(t, nil, nil)
	for i := range maxAttachSessionsPerRun {
		if _, ok := pr.srv.reserveAttachSlot(pr.run, fmt.Sprintf("p%d@example.com", i)); !ok {
			t.Fatal("a slot below the run cap was refused")
		}
	}
	for _, q := range []string{"purpose=attach-forward&ports=3000", "purpose=ssh-exec"} {
		if w := pr.mint(t, ownerCookie(t), pr.run.ID, q); w.Code != http.StatusOK {
			t.Errorf("mint ?%s on a full run = %d %s, want 200", q, w.Code, w.Body)
		}
	}
	w := pr.mint(t, ownerCookie(t), pr.run.ID, "")
	if w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonAttachSessionCapReached {
		t.Fatalf("terminal mint on a full run = %d %s, want 503 attach_session_cap_reached", w.Code, w.Body)
	}
}

// ticketWith stores a ticket for the rig's owner on its run, minted for purpose (and ports).
func (pr *purposeRig) ticketWith(t *testing.T, purpose string, ports []int) string {
	t.Helper()
	tok, err := storeAttachTicket(context.Background(), pr.srv.cfg.Store, store.AttachTicket{RunID: pr.run.ID, ActorType: types.ActorHuman,
		Principal: purposeOwner, Role: oidc.RoleUser, Purpose: purpose, Ports: ports}, oidc.Issue{At: time.Now()})
	if err != nil {
		t.Fatalf("store a ticket: %v", err)
	}
	return tok
}

func (pr *purposeRig) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	panicFails(t, pr.srv.Handler()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// TestAttachTicketPurpose_WrongDoorBurns: a ticket minted for one door, spent at another,
// gets the bad-ticket answer byte for byte with the bad-ticket row, and is burned. The server has no runner, so the
// terminal's own handler would answer 503 attach_no_runner: the 403 is the route's redemption, before the handler.
func TestAttachTicketPurpose_WrongDoorBurns(t *testing.T) {
	t.Setenv(envAttachForward, "on")
	pr := newPurposeRig(t, nil, nil)
	base := "/api/v1/runs/" + pr.run.ID.String()
	invalid := pr.get(t, base+"/attach?ticket=not-a-ticket")
	if invalid.Code != http.StatusForbidden || wireReason(t, invalid) != reasonUIGatewayTicketInvalid {
		t.Fatalf("an unknown ticket at /attach = %d %s, want the bad-ticket 403", invalid.Code, invalid.Body)
	}
	invalidRows := func() int {
		n := 0
		for _, ev := range pr.srv.cfg.Audit.(*recRecorder).snapshot() {
			if ev.Action == "session.attach" && ev.Outcome == "failure" && ev.Actor == "unknown" &&
				strings.Contains(string(ev.Data), "invalid, expired, or already-used attach ticket") {
				n++
			}
		}
		return n
	}
	for _, tc := range []struct {
		name, purpose, door string
		ports               []int
	}{
		{"ssh-exec at the terminal", ticketPurposeSSHExec, "/attach?ticket=", nil},
		{"ssh-forward at the forward door", ticketPurposeSSHForward, "/attach/forward?port=3000&ticket=", []int{3000}},
		{"ssh-editor at the terminal", ticketPurposeSSHEditor, "/attach?ticket=", nil},
		{"attach-forward at the terminal", ticketPurposeAttachForward, "/attach?ticket=", []int{3000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := invalidRows()
			tok := pr.ticketWith(t, tc.purpose, tc.ports)
			w := pr.get(t, base+tc.door+tok)
			if w.Code != invalid.Code || w.Body.String() != invalid.Body.String() {
				t.Fatalf("%s = %d %s, want the bad-ticket answer %d %s", tc.door, w.Code, w.Body, invalid.Code, invalid.Body)
			}
			if got := invalidRows(); got != rows+1 {
				t.Errorf("bad-ticket rows = %d, want %d", got, rows+1)
			}
			if _, live := pr.tickets()[tok]; live {
				t.Fatal("the ticket survived a refused redemption")
			}
		})
	}
	t.Run("the UI gateway's redemption", func(t *testing.T) {
		for _, p := range []string{ticketPurposeSSHExec, ticketPurposeAttachForward} {
			tok := pr.ticketWith(t, p, []int{3000})
			if _, v, err := pr.srv.redeemAttachTicket(context.Background(), tok, pr.run.ID); err != nil || v != verdictInvalid {
				t.Fatalf("%s ticket at the UI gateway = %v %v, want invalid", p, v, err)
			}
			if _, v, _ := pr.srv.redeemAttachTicket(context.Background(), tok, pr.run.ID); v != verdictInvalid {
				t.Fatalf("%s ticket redeemed a second time: %v", p, v)
			}
		}
	})
	t.Run("control: a terminal ticket reaches the terminal's handler", func(t *testing.T) {
		w := pr.get(t, base+"/attach?ticket="+pr.ticketWith(t, "", nil))
		if w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonAttachNoRunner {
			t.Fatalf("a terminal ticket at /attach = %d %s, want the handler's 503 attach_no_runner", w.Code, w.Body)
		}
	})
}
