// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAWSSigV4Sign_PublishedVectors signs each published SigV4 case (testdata/sigv4, see its
// NOTICE) through the proxy's signer path and wants the published Authorization header.
func TestAWSSigV4Sign_PublishedVectors(t *testing.T) {
	dirs, err := filepath.Glob("testdata/sigv4/*/context.json")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no vectors: %v", err)
	}
	for _, ctxPath := range dirs {
		dir := filepath.Dir(ctxPath)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			var c struct {
				Credentials struct {
					AccessKeyID     string `json:"access_key_id"`
					SecretAccessKey string `json:"secret_access_key"`
					Token           string `json:"token"`
				} `json:"credentials"`
				Region    string    `json:"region"`
				Service   string    `json:"service"`
				SignBody  bool      `json:"sign_body"`
				Timestamp time.Time `json:"timestamp"`
			}
			raw, _ := os.ReadFile(ctxPath)
			if err := json.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}
			req := readVectorRequest(t, filepath.Join(dir, "request.txt"))
			want := readVectorRequest(t, filepath.Join(dir, "header-signed-request.txt")).Header.Get("Authorization")
			body, _ := io.ReadAll(req.Body)
			sum := sha256.Sum256(body)
			payload := hex.EncodeToString(sum[:])
			signed := []string{"host"}
			for k := range req.Header {
				signed = append(signed, k)
			}
			if c.SignBody {
				req.Header.Set("X-Amz-Content-Sha256", payload)
				signed = append(signed, "x-amz-content-sha256")
			}
			creds := aws.Credentials{AccessKeyID: c.Credentials.AccessKeyID, SecretAccessKey: c.Credentials.SecretAccessKey, SessionToken: c.Credentials.Token}
			if err := awsSigV4Sign(v4.NewSigner(), req, signed, payload, awsSigV4HostScope{service: c.Service, region: c.Region}, creds, c.Timestamp); err != nil {
				t.Fatal(err)
			}
			if got := req.Header.Get("Authorization"); got != want {
				t.Fatalf("Authorization\n got %s\nwant %s", got, want)
			}
		})
	}
}

// readVectorRequest reads a vector's request as the terminated tunnel would, with the URL a
// forward carries.
func readVectorRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("\n\n")) {
		raw = append(bytes.TrimRight(raw, "\n"), "\n\n"...) // a bodiless case ends at its last header
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	req.URL.Scheme, req.URL.Host = "https", req.Host
	req.Header.Del("Host")
	return req
}

func TestAWSSigV4Scope(t *testing.T) {
	for host, want := range map[string]string{
		"bedrock-runtime.us-east-1.amazonaws.com":         "bedrock us-east-1",
		"bedrock-agent-runtime.eu-west-1.amazonaws.com":   "bedrock eu-west-1",
		"s3.us-west-2.amazonaws.com":                      "s3 us-west-2",
		"my-bucket.s3.us-west-2.amazonaws.com":            "s3 us-west-2",
		"my-bucket.s3.amazonaws.com":                      "s3 us-east-1",
		"*.s3.us-east-1.amazonaws.com":                    "",
		"*.amazonaws.com":                                 "",
		"sts.amazonaws.com":                               "sts us-east-1",
		"iam.amazonaws.com":                               "iam us-east-1",
		"iam-fips.amazonaws.com":                          "iam us-east-1",
		"sts-fips.us-east-1.amazonaws.com":                "sts us-east-1",
		"sts.us-east-1.api.aws":                           "",
		"vpce-1a2b-c3d4.sts.us-east-1.vpce.amazonaws.com": "",
		"dynamodb.ap-southeast-2.amazonaws.com":           "dynamodb ap-southeast-2",
		"Logs.US-East-1.amazonaws.com.":                   "logs us-east-1",
		"portal.sso.us-east-1.amazonaws.com":              "",
		"oidc.us-east-1.amazonaws.com":                    "",
		"signin.aws.amazon.com":                           "",
		"s3.dualstack.us-east-1.amazonaws.com":            "",
		"x.bedrock-runtime.us-east-1.vpce.amazonaws.com":  "",
		"123456789012.s3-control.us-east-1.amazonaws.com": "",
		"us-east-1.amazonaws.com":                         "",
		"amazonaws.com":                                   "",
		"bedrock-runtime.us-east-1.amazonaws.com.cn":      "",
		"example.com":                                     "",
	} {
		service, region, ok := AWSSigV4Scope(host)
		got := ""
		if ok {
			got = service + " " + region
		}
		if got != want {
			t.Errorf("AWSSigV4Scope(%q) = %q, want %q", host, got, want)
		}
	}
}

const (
	sigv4Bearer     = "sso-bearer-of-the-person"
	sigv4RoleKey    = "ROLEKEYID-OF-THE-PERSON"
	sigv4RoleSecret = "role-secret-of-the-person"
	sigv4RoleToken  = "role-session-token-of-the-person"
	sigv4Account    = "111122223333"
	sigv4Role       = "Developer"
	sigv4Portal     = "portal.sso.us-east-1.amazonaws.com"
	sigv4Bedrock    = "bedrock-runtime.us-east-1.amazonaws.com"
	sigv4S3         = "s3.us-east-1.amazonaws.com"
	sigv4STS        = "sts.us-east-1.amazonaws.com"
	sigv4STSLondon  = "sts.eu-west-2.amazonaws.com"
	sigv4STSFIPS    = "sts-fips.us-east-1.amazonaws.com"
	sigv4IAM        = "iam.amazonaws.com"
)

// sigv4Hosts are the hosts the harness's run re-signs on.
var sigv4Hosts = []string{sigv4Bedrock, sigv4S3, sigv4STS, sigv4STSLondon, sigv4STSFIPS, sigv4IAM}

// sigv4Arrival is one request as the fake AWS service read it.
type sigv4Arrival struct {
	method, uri, host string
	header            http.Header
	body              []byte
	contentLength     int64
}

// sigv4Harness is a proxy whose run re-signs on three AWS hosts, one TLS server standing in for
// every AWS host (the portal and the services) that records what arrives, and a fake control
// plane that counts every resolve of the run's sign-in.
type sigv4Harness struct {
	p           *Proxy
	mu          sync.Mutex
	seen        []sigv4Arrival
	redeemed    atomic.Int32
	portalCalls atomic.Int32
	cpStatus    atomic.Int32 // the control plane's answer; 0 is 200
	portalCode  atomic.Int32 // the portal's answer; 0 is 200
	lifetime    atomic.Int64 // role credential lifetime; 0 is an hour
	respond     func(w http.ResponseWriter, r *http.Request)
	log         func() string
}

func newSigV4Harness(t *testing.T, allowed ...string) *sigv4Harness {
	t.Helper()
	h := &sigv4Harness{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == awsSSOFederationPath {
			h.portalCalls.Add(1)
			q := r.URL.Query()
			if r.Host != sigv4Portal || r.Header.Get("x-amz-sso_bearer_token") != sigv4Bearer ||
				q.Get("account_id") != sigv4Account || q.Get("role_name") != sigv4Role {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if c := h.portalCode.Load(); c != 0 {
				w.WriteHeader(int(c))
				return
			}
			life := time.Duration(h.lifetime.Load())
			if life == 0 {
				life = time.Hour
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"roleCredentials": map[string]any{
				"accessKeyId": sigv4RoleKey, "secretAccessKey": sigv4RoleSecret, "sessionToken": sigv4RoleToken,
				"expiration": time.Now().Add(life).UnixMilli(),
			}})
			return
		}
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.seen = append(h.seen, sigv4Arrival{method: r.Method, uri: r.RequestURI, host: r.Host, header: r.Header.Clone(), body: body, contentLength: r.ContentLength})
		h.mu.Unlock()
		if h.respond != nil {
			h.respond(w, r)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(upstream.Close)
	grant := uuid.New()
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.redeemed.Add(1)
		if r.URL.Path != "/api/v1/internal/injection/"+grant.String() || r.Header.Get("Authorization") != "Bearer RUNTOK" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if s := h.cpStatus.Load(); s != 0 {
			w.WriteHeader(int(s))
			_, _ = io.WriteString(w, `{"error":"The AWS sign-in behind this run has ended"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{Header: "x-amz-sso_bearer_token", Value: sigv4Bearer, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	}))
	t.Cleanup(cp.Close)
	buf := &bytes.Buffer{}
	h.p = newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: append(sigv4HostPorts(), allowed...)}),
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Injector:        &injector{byHost: map[string]*injEntry{}, base: cp.URL, token: newTokenSource("RUNTOK"), client: cp.Client()},
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(upstream)),
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		AWSSigV4: &AWSSigV4Config{GrantID: grant, PortalHost: sigv4Portal, AccountID: sigv4Account, RoleName: sigv4Role,
			Hosts: sigv4Hosts},
	})
	h.log = func() string {
		_ = h.p.sink.close(context.Background())
		return buf.String()
	}
	return h
}

func sigv4HostPorts() []string {
	out := make([]string, 0, len(sigv4Hosts))
	for _, h := range sigv4Hosts {
		out = append(out, h+":443")
	}
	return out
}

func (h *sigv4Harness) arrivals() []sigv4Arrival {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]sigv4Arrival(nil), h.seen...)
}

// sandboxRequest is a request the sandbox's SDK signed with the placeholder (or with key, when
// set), as the terminated tunnel reads it.
func sandboxRequest(t *testing.T, method, host, target, body string, hdr map[string]string, key, service, region, payload string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, "https://"+host+target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body == "" {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if key == "" {
		key = AWSSigV4PlaceholderAccessKeyID
	}
	if payload == "" {
		sum := sha256.Sum256([]byte(body))
		payload = hex.EncodeToString(sum[:])
	}
	if service != "" {
		if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: key, SecretAccessKey: AWSSigV4PlaceholderSecretAccessKey},
			req, payload, service, region, time.Now(), func(o *v4.SignerOptions) { o.DisableURIPathEscaping = service == "s3" }); err != nil {
			t.Fatal(err)
		}
	}
	var wire bytes.Buffer
	if err := req.Write(&wire); err != nil {
		t.Fatal(err)
	}
	in, err := http.ReadRequest(bufio.NewReader(&wire))
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func (h *sigv4Harness) send(t *testing.T, req *http.Request, host string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.p.serveMITMRequest(rec, req, host, 443)
	return rec
}

// mustReSign asserts the request reached AWS once, signed with the role credentials over
// exactly what arrived, and that neither the placeholder nor a credential reached the sandbox.
func (h *sigv4Harness) mustReSign(t *testing.T, rec *httptest.ResponseRecorder, service string) sigv4Arrival {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s, want 200", rec.Code, rec.Body)
	}
	seen := h.arrivals()
	if len(seen) != 1 {
		t.Fatalf("AWS saw %d requests, want 1", len(seen))
	}
	a := seen[0]
	auth := a.header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential="+sigv4RoleKey+"/") || a.header.Get("X-Amz-Security-Token") != sigv4RoleToken {
		t.Fatalf("AWS saw Authorization %q token %q, want the role's", auth, a.header.Get("X-Amz-Security-Token"))
	}
	if strings.Contains(auth, AWSSigV4PlaceholderAccessKeyID) {
		t.Fatalf("the placeholder reached AWS: %q", auth)
	}
	verifyArrival(t, a, service)
	for _, secret := range []string{sigv4Bearer, sigv4RoleSecret, sigv4RoleToken} {
		if strings.Contains(rec.Body.String(), secret) || strings.Contains(h.log(), secret) {
			t.Fatalf("%q reached the sandbox or the decision log", secret)
		}
	}
	return a
}

// verifyArrival checks the signature AWS received against the request AWS received, as AWS
// would: the role's key over the listed headers, the path and the query that arrived.
func verifyArrival(t *testing.T, a sigv4Arrival, service string) {
	t.Helper()
	_, params, _ := strings.Cut(a.header.Get("Authorization"), " ")
	var signedHeaders, scope string
	for f := range strings.SplitSeq(params, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
		switch k {
		case "SignedHeaders":
			signedHeaders = v
		case "Credential":
			scope = v
		}
	}
	region := strings.Split(scope, "/")[2]
	u, err := url.ParseRequestURI(a.uri)
	if err != nil {
		t.Fatal(err)
	}
	u.Scheme, u.Host = "https", a.host
	r := &http.Request{Method: a.method, URL: u, Host: a.host, Header: http.Header{}, ContentLength: a.contentLength}
	for k := range strings.SplitSeq(signedHeaders, ";") {
		if k != "host" && k != "content-length" {
			r.Header[http.CanonicalHeaderKey(k)] = a.header.Values(k)
		}
	}
	at, err := time.Parse("20060102T150405Z", a.header.Get("X-Amz-Date"))
	if err != nil {
		t.Fatal(err)
	}
	payload := a.header.Get("X-Amz-Content-Sha256")
	if payload == "" {
		sum := sha256.Sum256(a.body)
		payload = hex.EncodeToString(sum[:])
	}
	r.Header.Del("X-Amz-Date")
	r.Header.Del("X-Amz-Security-Token")
	if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: sigv4RoleKey, SecretAccessKey: sigv4RoleSecret, SessionToken: sigv4RoleToken},
		r, payload, service, region, at, func(o *v4.SignerOptions) { o.DisableURIPathEscaping = service == "s3" }); err != nil {
		t.Fatal(err)
	}
	if got, want := a.header.Get("Authorization"), r.Header.Get("Authorization"); got != want {
		t.Fatalf("the signature AWS received does not verify over what arrived\n got %s\nwant %s", got, want)
	}
}

func TestAWSSigV4_ReSignsAllowedRequests(t *testing.T) {
	t.Run("bedrock body hashed by the proxy", func(t *testing.T) {
		h := newSigV4Harness(t)
		body := `{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`
		rec := h.send(t, sandboxRequest(t, "POST", sigv4Bedrock, "/model/anthropic.claude-x%3A0/converse", body,
			map[string]string{"Content-Type": "application/json"}, "", "bedrock", "us-east-1", ""), sigv4Bedrock)
		a := h.mustReSign(t, rec, "bedrock")
		if string(a.body) != body || a.uri != "/model/anthropic.claude-x%3A0/converse" {
			t.Fatalf("AWS saw %s %q, want the sandbox's request", a.uri, a.body)
		}
		if log := h.log(); !strings.Contains(log, `"`+ruleSourceAWSSigV4+`"`) {
			t.Fatalf("no %s allow row: %s", ruleSourceAWSSigV4, log)
		}
	})
	t.Run("bedrock response stream relayed", func(t *testing.T) {
		h := newSigV4Harness(t)
		h.respond = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			for _, chunk := range []string{"frame-one", "frame-two"} {
				_, _ = io.WriteString(w, chunk)
				w.(http.Flusher).Flush()
			}
		}
		rec := h.send(t, sandboxRequest(t, "POST", sigv4Bedrock, "/model/m/invoke-with-response-stream", `{"prompt":"hi"}`,
			map[string]string{"Content-Type": "application/json"}, "", "bedrock", "us-east-1", ""), sigv4Bedrock)
		h.mustReSign(t, rec, "bedrock")
		if rec.Body.String() != "frame-oneframe-two" {
			t.Fatalf("relayed %q", rec.Body)
		}
	})
	t.Run("s3 unsigned payload keeps the raw path", func(t *testing.T) {
		h := newSigV4Harness(t)
		rec := h.send(t, sandboxRequest(t, "PUT", sigv4S3, "/bucket/a%20b/c%2Bd.txt?x-id=PutObject", "object bytes",
			map[string]string{"X-Amz-Content-Sha256": "UNSIGNED-PAYLOAD"}, "", "s3", "us-east-1", "UNSIGNED-PAYLOAD"), sigv4S3)
		a := h.mustReSign(t, rec, "s3")
		if a.uri != "/bucket/a%20b/c%2Bd.txt?x-id=PutObject" || string(a.body) != "object bytes" {
			t.Fatalf("AWS saw %s %q", a.uri, a.body)
		}
	})
	t.Run("s3 unsigned streaming trailer upload", func(t *testing.T) {
		h := newSigV4Harness(t)
		chunked := "c\r\nobject bytes\r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n"
		rec := h.send(t, sandboxRequest(t, "PUT", sigv4S3, "/bucket/key", chunked, map[string]string{
			"X-Amz-Content-Sha256": "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "Content-Encoding": "aws-chunked",
			"X-Amz-Decoded-Content-Length": "12", "X-Amz-Trailer": "x-amz-checksum-crc32",
		}, "", "s3", "us-east-1", "STREAMING-UNSIGNED-PAYLOAD-TRAILER"), sigv4S3)
		a := h.mustReSign(t, rec, "s3")
		if string(a.body) != chunked || a.header.Get("Content-Encoding") != "aws-chunked" {
			t.Fatalf("AWS saw body %q encoding %q", a.body, a.header.Get("Content-Encoding"))
		}
	})
	t.Run("sts GetCallerIdentity", func(t *testing.T) {
		h := newSigV4Harness(t)
		rec := h.send(t, sandboxRequest(t, "POST", sigv4STS, "/", "Action=GetCallerIdentity&Version=2011-06-15",
			map[string]string{"Content-Type": "application/x-www-form-urlencoded; charset=utf-8"}, "", "sts", "us-east-1", ""), sigv4STS)
		h.mustReSign(t, rec, "sts")
	})
}

func TestAWSSigV4_RoleCredentialsCachedAndRenewed(t *testing.T) {
	h := newSigV4Harness(t)
	req := func() *http.Request {
		return sandboxRequest(t, "POST", sigv4Bedrock, "/model/m/converse", `{}`, nil, "", "bedrock", "us-east-1", "")
	}
	for range 2 {
		if rec := h.send(t, req(), sigv4Bedrock); rec.Code != http.StatusOK {
			t.Fatalf("status %d %s", rec.Code, rec.Body)
		}
	}
	if r, p := h.redeemed.Load(), h.portalCalls.Load(); r != 1 || p != 1 {
		t.Fatalf("two requests resolved the sign-in %d times and called the portal %d times, want 1 and 1", r, p)
	}
	h.p.sigv4.creds.Expires = time.Now().Add(awsSigV4CredentialMargin - time.Second)
	if rec := h.send(t, req(), sigv4Bedrock); rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	if r, p := h.redeemed.Load(), h.portalCalls.Load(); r != 2 || p != 2 {
		t.Fatalf("credentials inside the renewal margin were not renewed: %d resolves, %d portal calls", r, p)
	}
}

func TestAWSSigV4_RefusesWhatItCannotReSign(t *testing.T) {
	json := map[string]string{"Content-Type": "application/json"}
	for _, tc := range []struct {
		name, code string
		req        func(t *testing.T) *http.Request
		host       string
	}{
		{"presigned", awsSigV4Presigned, func(t *testing.T) *http.Request {
			return sandboxRequest(t, "GET", sigv4S3, "/bucket/key?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=x&X-Amz-Signature=y", "", nil, "", "", "", "")
		}, sigv4S3},
		{"unsigned", awsSigV4NotPlaceholder, func(t *testing.T) *http.Request {
			return sandboxRequest(t, "POST", sigv4Bedrock, "/model/m/converse", `{}`, json, "", "", "", "")
		}, sigv4Bedrock},
		{"another key", awsSigV4NotPlaceholder, func(t *testing.T) *http.Request {
			return sandboxRequest(t, "POST", sigv4Bedrock, "/model/m/converse", `{}`, json, "SOMEONEELSESKEY", "bedrock", "us-east-1", "")
		}, sigv4Bedrock},
		{"sigv4a", awsSigV4Algorithm, func(t *testing.T) *http.Request {
			return sandboxRequest(t, "POST", sigv4Bedrock, "/model/m/converse", `{}`, map[string]string{"Authorization": "AWS4-ECDSA-P256-SHA256 Credential=" +
				AWSSigV4PlaceholderAccessKeyID + "/20260101/bedrock/aws4_request, SignedHeaders=host, Signature=00"}, "", "", "", "")
		}, sigv4Bedrock},
		{"wrong region", awsSigV4Scope, func(t *testing.T) *http.Request {
			return sandboxRequest(t, "POST", sigv4Bedrock, "/model/m/converse", `{}`, json, "", "bedrock", "us-west-2", "")
		}, sigv4Bedrock},
		{"wrong service", awsSigV4Scope, func(t *testing.T) *http.Request {
			return sandboxRequest(t, "POST", sigv4Bedrock, "/model/m/converse", `{}`, json, "", "sagemaker", "us-east-1", "")
		}, sigv4Bedrock},
		{"signed chunks", awsSigV4SignedChunks, func(t *testing.T) *http.Request {
			return sandboxRequest(t, "PUT", sigv4S3, "/bucket/key", "chunks", map[string]string{"X-Amz-Content-Sha256": "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"},
				"", "s3", "us-east-1", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD")
		}, sigv4S3},
		{"event stream body", awsSigV4EventStream, func(t *testing.T) *http.Request {
			return sandboxRequest(t, "POST", sigv4Bedrock, "/model/m/invoke-with-bidirectional-stream", "frames",
				map[string]string{"Content-Type": "application/vnd.amazon.eventstream"}, "", "bedrock", "us-east-1", "")
		}, sigv4Bedrock},
		{"payload hash", awsSigV4Payload, func(t *testing.T) *http.Request {
			return sandboxRequest(t, "PUT", sigv4S3, "/bucket/key", "x", map[string]string{"X-Amz-Content-Sha256": "not-a-hash"}, "", "s3", "us-east-1", "not-a-hash")
		}, sigv4S3},
		{"body too large", awsSigV4BodyTooLarge, func(t *testing.T) *http.Request {
			return sandboxRequest(t, "POST", sigv4Bedrock, "/model/m/converse", strings.Repeat("x", awsSigV4MaxHashedBody+1), json, "", "bedrock", "us-east-1", "")
		}, sigv4Bedrock},
		{"sts AssumeRole", awsSigV4Action, func(t *testing.T) *http.Request {
			return sandboxRequest(t, "POST", sigv4STS, "/", "Action=AssumeRole&RoleArn=arn&RoleSessionName=s&Version=2011-06-15",
				map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, "", "sts", "us-east-1", "")
		}, sigv4STS},
		{"sts by query", awsSigV4Action, func(t *testing.T) *http.Request {
			return sandboxRequest(t, "GET", sigv4STS, "/?Action=GetSessionToken&Version=2011-06-15", "", nil, "", "sts", "us-east-1", "")
		}, sigv4STS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSigV4Harness(t)
			rec := h.send(t, tc.req(t), tc.host)
			if rec.Code != http.StatusForbidden || rec.Header().Get("X-Wardyn-Reason") != tc.code || !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("status %d reason %q body %s, want 403 %s", rec.Code, rec.Header().Get("X-Wardyn-Reason"), rec.Body, tc.code)
			}
			if seen := h.arrivals(); len(seen) != 0 {
				t.Fatalf("AWS saw %d requests; a refused request must not leave", len(seen))
			}
			if r := h.redeemed.Load(); r != 0 {
				t.Fatalf("a refused request resolved the sign-in %d times", r)
			}
			if log := h.log(); !strings.Contains(log, `"`+ruleSourceAWSSigV4Refused+`"`) {
				t.Fatalf("no %s row: %s", ruleSourceAWSSigV4Refused, log)
			}
		})
	}
}

func TestAWSSigV4_RefusesWhenCredentialsFail(t *testing.T) {
	for name, set := range map[string]func(h *sigv4Harness){
		"control plane refuses": func(h *sigv4Harness) { h.cpStatus.Store(http.StatusForbidden) },
		"portal refuses":        func(h *sigv4Harness) { h.portalCode.Store(http.StatusUnauthorized) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newSigV4Harness(t)
			set(h)
			rec := h.send(t, sandboxRequest(t, "POST", sigv4Bedrock, "/model/m/converse", `{}`, nil, "", "bedrock", "us-east-1", ""), sigv4Bedrock)
			if rec.Code != http.StatusForbidden || rec.Header().Get("X-Wardyn-Reason") != awsSigV4Credentials {
				t.Fatalf("status %d reason %q body %s, want 403 %s", rec.Code, rec.Header().Get("X-Wardyn-Reason"), rec.Body, awsSigV4Credentials)
			}
			if len(h.arrivals()) != 0 {
				t.Fatal("a request with no role credentials reached AWS")
			}
		})
	}
}

func TestAWSSigV4_NoUnterminatedPath(t *testing.T) {
	h := newSigV4Harness(t, sigv4Bedrock+":8443")
	rec := httptest.NewRecorder()
	connect := httptest.NewRequest(http.MethodConnect, "http://"+sigv4Bedrock+":8443", nil)
	connect.Host = sigv4Bedrock + ":8443"
	h.p.handleConnect(rec, connect)
	if rec.Code != http.StatusForbidden || rec.Header().Get("X-Wardyn-Reason") != awsSigV4Scope {
		t.Fatalf("CONNECT on another port: status %d reason %q, want 403 %s", rec.Code, rec.Header().Get("X-Wardyn-Reason"), awsSigV4Scope)
	}
	rec = httptest.NewRecorder()
	h.p.servePlain(rec, httptest.NewRequest(http.MethodGet, "http://"+sigv4Bedrock+"/model/m/converse", nil))
	if rec.Code != http.StatusForbidden || rec.Header().Get("X-Wardyn-Reason") != awsSigV4Scope {
		t.Fatalf("plain request: status %d reason %q, want 403 %s", rec.Code, rec.Header().Get("X-Wardyn-Reason"), awsSigV4Scope)
	}
	if len(h.arrivals()) != 0 {
		t.Fatal("an unterminated request reached AWS")
	}
}

func TestAWSSigV4_ConfigValidation(t *testing.T) {
	good := func() *Config {
		return &Config{MITMCACertPEM: "cert", MITMCAKeyPEM: "key", AWSSigV4: &AWSSigV4Config{
			GrantID: uuid.New(), PortalHost: sigv4Portal, AccountID: sigv4Account, RoleName: sigv4Role, Hosts: []string{sigv4Bedrock, sigv4S3},
		}}
	}
	if err := good().validateAWSSigV4(); err != nil {
		t.Fatalf("good config refused: %v", err)
	}
	for name, mut := range map[string]func(c *Config){
		"no CA":            func(c *Config) { c.MITMCAKeyPEM = "" },
		"no grant":         func(c *Config) { c.AWSSigV4.GrantID = uuid.Nil },
		"not a portal":     func(c *Config) { c.AWSSigV4.PortalHost = "evil.example.com" },
		"account":          func(c *Config) { c.AWSSigV4.AccountID = "1234" },
		"role":             func(c *Config) { c.AWSSigV4.RoleName = "a/b" },
		"no hosts":         func(c *Config) { c.AWSSigV4.Hosts = nil },
		"wildcard host":    func(c *Config) { c.AWSSigV4.Hosts = []string{"*.amazonaws.com"} },
		"portal as host":   func(c *Config) { c.AWSSigV4.Hosts = []string{sigv4Portal} },
		"uppercase host":   func(c *Config) { c.AWSSigV4.Hosts = []string{"S3.us-east-1.amazonaws.com"} },
		"duplicate host":   func(c *Config) { c.AWSSigV4.Hosts = []string{sigv4S3, sigv4S3} },
		"injection on one": func(c *Config) { c.Injection = []InjectionConfig{{InjectionRule: egress.InjectionRule{Host: sigv4S3}}} },
		"mitm entry":       func(c *Config) { c.MITMHosts = []string{sigv4Bedrock + ":443"} },
		"vpce sts host":    func(c *Config) { c.AWSSigV4.Hosts = []string{"vpce-1a2b-c3d4.sts.us-east-1.vpce.amazonaws.com"} },
		"dualstack sts":    func(c *Config) { c.AWSSigV4.Hosts = []string{"sts.us-east-1.api.aws"} },
	} {
		c := good()
		mut(c)
		if err := c.validateTerminatedGates(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// STS and IAM are judged on the one Action they run, read from the query and the plain form
// body together, never from a header, an encoded body or one of several Actions.
func TestAWSSigV4_GuardsQueryProtocolActions(t *testing.T) {
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	with := func(extra map[string]string) map[string]string {
		m := maps.Clone(form)
		maps.Copy(m, extra)
		return m
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = io.WriteString(zw, "Action=AssumeRole&RoleArn=arn&RoleSessionName=s&Version=2011-06-15")
	_ = zw.Close()
	type req struct {
		method, host, target, body string
		hdr                        map[string]string
		service, region            string
		chunked                    bool
		wireQuery                  string // the query as sent, when it differs from the signed (sorted) one
	}
	allowed := []struct {
		name    string
		r       req
		service string
	}{
		{"sts GetCallerIdentity in the query", req{"GET", sigv4STS, "/?Action=GetCallerIdentity&Version=2011-06-15", "", nil, "sts", "us-east-1", false, ""}, "sts"},
		{"sts GetCallerIdentity in the body", req{"POST", sigv4STSLondon, "/", "Action=GetCallerIdentity&Version=2011-06-15", form, "sts", "eu-west-2", false, ""}, "sts"},
		{"sts-fips GetCallerIdentity", req{"POST", sigv4STSFIPS, "/", "Action=GetCallerIdentity&Version=2011-06-15", form, "sts", "us-east-1", false, ""}, "sts"},
		{"iam ListUsers", req{"POST", sigv4IAM, "/", "Action=ListUsers&Version=2010-05-08", form, "iam", "us-east-1", false, ""}, "iam"},
		{"iam TagUser with member keys", req{"POST", sigv4IAM, "/", "Action=TagUser&UserName=u&Tags.member.1.Key=team&Tags.member.1.Value=a_b-c&Version=2010-05-08", form, "iam", "us-east-1", false, ""}, "iam"},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			h := newSigV4Harness(t)
			r := tc.r
			h.mustReSign(t, h.send(t, sandboxRequest(t, r.method, r.host, r.target, r.body, r.hdr, "", r.service, r.region, ""), r.host), tc.service)
		})
	}
	refused := []struct {
		name, code string
		r          req
	}{
		{"X-Amz-Target names GetCallerIdentity over an AssumeRole body", awsSigV4Action, req{"POST", sigv4STS, "/", "Action=AssumeRole&RoleArn=arn&RoleSessionName=s&Version=2011-06-15",
			with(map[string]string{"X-Amz-Target": "AWSSecurityTokenServiceV20110615.GetCallerIdentity"}), "sts", "us-east-1", false, ""}},
		{"X-Amz-Target alone", awsSigV4Action, req{"POST", sigv4STS, "/", "", map[string]string{"X-Amz-Target": "AWSSecurityTokenServiceV20110615.GetCallerIdentity"}, "sts", "us-east-1", false, ""}},
		{"a duplicate Action in the query", awsSigV4Action, req{"GET", sigv4STS, "/?Action=GetCallerIdentity&Action=AssumeRole&Version=2011-06-15", "", nil, "sts", "us-east-1", false,
			"Action=GetCallerIdentity&Action=AssumeRole&Version=2011-06-15"}},
		{"the same Action twice", awsSigV4Action, req{"GET", sigv4STS, "/?Action=GetCallerIdentity&Action=GetCallerIdentity", "", nil, "sts", "us-east-1", false, ""}},
		{"the query and the body disagree", awsSigV4Action, req{"POST", sigv4STS, "/?Action=GetCallerIdentity", "Action=AssumeRole&RoleArn=arn&RoleSessionName=s", form, "sts", "us-east-1", false, ""}},
		{"an Action under another case", awsSigV4Action, req{"POST", sigv4STS, "/?Action=GetCallerIdentity", "action=AssumeRole", form, "sts", "us-east-1", false, ""}},
		{"a body that does not parse", awsSigV4Action, req{"POST", sigv4STS, "/?Action=GetCallerIdentity", "Action=AssumeRole;x", form, "sts", "us-east-1", false, ""}},
		{"no Action", awsSigV4Action, req{"POST", sigv4STS, "/", "Version=2011-06-15", form, "sts", "us-east-1", false, ""}},
		{"a gzip body", awsSigV4Action, req{"POST", sigv4STS, "/?Action=GetCallerIdentity", gz.String(), with(map[string]string{"Content-Encoding": "gzip"}), "sts", "us-east-1", false, ""}},
		{"a chunked body", awsSigV4Action, req{"POST", sigv4STS, "/", "Action=GetCallerIdentity", form, "sts", "us-east-1", true, ""}},
		{"a JSON body", awsSigV4Action, req{"POST", sigv4STS, "/?Action=GetCallerIdentity", `{"Action":"AssumeRole"}`, map[string]string{"Content-Type": "application/x-amz-json-1.1"}, "sts", "us-east-1", false, ""}},
		{"a CBOR body", awsSigV4Action, req{"POST", sigv4STS, "/?Action=GetCallerIdentity", "\xa0", map[string]string{"Content-Type": "application/cbor", "Smithy-Protocol": "rpc-v2-cbor"}, "sts", "us-east-1", false, ""}},
		{"a regional host carrying AssumeRole", awsSigV4Action, req{"POST", sigv4STSLondon, "/", "Action=AssumeRole&RoleArn=arn&RoleSessionName=s", form, "sts", "eu-west-2", false, ""}},
		{"a FIPS host carrying AssumeRole", awsSigV4Action, req{"POST", sigv4STSFIPS, "/", "Action=AssumeRole&RoleArn=arn&RoleSessionName=s", form, "sts", "us-east-1", false, ""}},
		{"iam CreateAccessKey", awsSigV4Action, req{"POST", sigv4IAM, "/", "Action=CreateAccessKey&UserName=u", form, "iam", "us-east-1", false, ""}},
		{"iam CreateAccessKey in the query", awsSigV4Action, req{"GET", sigv4IAM, "/?Action=CreateAccessKey&UserName=u", "", nil, "iam", "us-east-1", false, ""}},
		{"iam CreateLoginProfile behind a ListUsers target", awsSigV4Action, req{"POST", sigv4IAM, "/", "Action=CreateLoginProfile&UserName=u&Password=p",
			with(map[string]string{"X-Amz-Target": "AWSIdentityManagementV20100508.ListUsers"}), "iam", "us-east-1", false, ""}},
		{"an sts host signed for another service", awsSigV4Scope, req{"POST", sigv4STS, "/", "Action=AssumeRole&RoleArn=arn&RoleSessionName=s", form, "iam", "us-east-1", false, ""}},
		{"another service's host signed for sts", awsSigV4Scope, req{"POST", sigv4Bedrock, "/", "Action=AssumeRole&RoleArn=arn&RoleSessionName=s", form, "sts", "us-east-1", false, ""}},
	}
	refused = append(refused,
		struct {
			name, code string
			r          req
		}{"two Content-Type headers", awsSigV4Action, req{"POST", sigv4STS, "/", "Action=GetCallerIdentity", form, "sts", "us-east-1", false, ""}},
		struct {
			name, code string
			r          req
		}{"iam CreateAccessKey with a trailing space", awsSigV4Action, req{"POST", sigv4IAM, "/", "Action=CreateAccessKey+&UserName=u", form, "iam", "us-east-1", false, ""}},
		struct {
			name, code string
			r          req
		}{"iam createaccesskey", awsSigV4Action, req{"POST", sigv4IAM, "/", "Action=createaccesskey&UserName=u", form, "iam", "us-east-1", false, ""}},
		struct {
			name, code string
			r          req
		}{"iam CreateAccessKey with a NUL", awsSigV4Action, req{"POST", sigv4IAM, "/", "Action=CreateAccessKey%00&UserName=u", form, "iam", "us-east-1", false, ""}},
		struct {
			name, code string
			r          req
		}{"an empty iam Action", awsSigV4Action, req{"POST", sigv4IAM, "/", "Action=&UserName=u", form, "iam", "us-east-1", false, ""}},
		struct {
			name, code string
			r          req
		}{"sts getcalleridentity under another case", awsSigV4Action, req{"POST", sigv4STS, "/", "Action=getcalleridentity", form, "sts", "us-east-1", false, ""}},
		struct {
			name, code string
			r          req
		}{"a NUL key beside a query Action", awsSigV4Action, req{"POST", sigv4STS, "/?Action=GetCallerIdentity", "Action%00=AssumeRole&RoleArn=arn&RoleSessionName=s", form, "sts", "us-east-1", false, ""}},
		struct {
			name, code string
			r          req
		}{"a key with a tab", awsSigV4Action, req{"POST", sigv4IAM, "/?Action=ListUsers", "Act%09ion=CreateAccessKey&UserName=u", form, "iam", "us-east-1", false, ""}})
	for _, action := range []string{"ResetServiceSpecificCredential", "UploadSSHPublicKey", "UploadSigningCertificate"} {
		refused = append(refused,
			struct {
				name, code string
				r          req
			}{"iam " + action + " in the query", awsSigV4Action, req{"GET", sigv4IAM, "/?Action=" + action + "&UserName=u", "", nil, "iam", "us-east-1", false, ""}},
			struct {
				name, code string
				r          req
			}{"iam " + action + " in the body", awsSigV4Action, req{"POST", sigv4IAM, "/", "Action=" + action + "&UserName=u", form, "iam", "us-east-1", false, ""}})
	}
	for _, action := range []string{"AssumeRole", "GetSessionToken", "GetFederationToken", "AssumeRoleWithWebIdentity"} {
		refused = append(refused,
			struct {
				name, code string
				r          req
			}{"sts " + action + " in the query", awsSigV4Action, req{"GET", sigv4STS, "/?Action=" + action + "&Version=2011-06-15", "", nil, "sts", "us-east-1", false, ""}},
			struct {
				name, code string
				r          req
			}{"sts " + action + " in the body", awsSigV4Action, req{"POST", sigv4STS, "/", "Action=" + action + "&Version=2011-06-15", form, "sts", "us-east-1", false, ""}})
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			h := newSigV4Harness(t)
			r := tc.r
			in := sandboxRequest(t, r.method, r.host, r.target, r.body, r.hdr, "", r.service, r.region, "")
			if r.chunked {
				in.TransferEncoding, in.ContentLength = []string{"chunked"}, -1
			}
			if r.wireQuery != "" {
				in.URL.RawQuery = r.wireQuery
			}
			if tc.name == "two Content-Type headers" {
				in.Header.Add("Content-Type", "application/x-amz-json-1.0")
			}
			rec := h.send(t, in, r.host)
			if rec.Code != http.StatusForbidden || rec.Header().Get("X-Wardyn-Reason") != tc.code {
				t.Fatalf("status %d reason %q body %s, want 403 %s", rec.Code, rec.Header().Get("X-Wardyn-Reason"), rec.Body, tc.code)
			}
			if seen := h.arrivals(); len(seen) != 0 {
				t.Fatalf("AWS saw %d requests (body %q); a refused request must not leave", len(seen), seen[0].body)
			}
		})
	}
}

// A streamed answer reaches the sandbox as it streams: over a real server and client, the first
// event arrives while the upstream is still holding the rest back.
func TestAWSSigV4_StreamedResponseIsFlushedPerChunk(t *testing.T) {
	h := newSigV4Harness(t)
	release := make(chan struct{})
	h.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = io.WriteString(w, "frame-one")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "frame-two")
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.p.serveMITMRequest(w, r, sigv4Bedrock, 443)
	}))
	defer front.Close()
	defer close(release)
	in := sandboxRequest(t, "POST", sigv4Bedrock, "/model/m/invoke-with-response-stream", `{"prompt":"hi"}`,
		map[string]string{"Content-Type": "application/json"}, "", "bedrock", "us-east-1", "")
	out, err := http.NewRequest(in.Method, front.URL+in.RequestURI, in.Body)
	if err != nil {
		t.Fatal(err)
	}
	out.Header, out.ContentLength = in.Header.Clone(), in.ContentLength
	first := make(chan string, 1)
	go func() { // headers and the first event, both while the upstream holds the rest back
		resp, err := front.Client().Do(out)
		if err != nil {
			first <- err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		buf := make([]byte, len("frame-one"))
		n, _ := io.ReadFull(resp.Body, buf)
		first <- string(buf[:n])
	}()
	select {
	case got := <-first:
		if got != "frame-one" {
			t.Fatalf("first event %q, want frame-one", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event did not arrive while the upstream was still streaming: the relay buffers")
	}
}
