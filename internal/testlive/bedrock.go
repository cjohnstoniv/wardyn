// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package testlive

// The live Bedrock suite's spend guard. Every credential comes from IAM
// Identity Center role credentials for ONE member account, the capped one, and
// that account is proven with STS before any model call: a service control
// policy does not bind a management account, so a management-account
// credential would spend outside the cap. There is deliberately no bearer-key
// path. The model allow-list, the max_tokens ceiling and the per-process call
// budget are enforced here, not left to the caller.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// MaxTokens is the ceiling on one call's max_tokens.
	MaxTokens = 32
	// DefaultMaxCalls and HardMaxCalls bound WARDYN_LIVE_BEDROCK_MAX_CALLS.
	DefaultMaxCalls = 5
	HardMaxCalls    = 20
	// DefaultModel is Claude Haiku 4.5 through the US cross-region profile.
	DefaultModel = "us.anthropic.claude-haiku-4-5-20251001-v1:0"
)

// allowedModels are the only base models the suite may call: Claude Haiku 4.5
// and Amazon Nova Micro, bare or behind a geographic inference profile.
var (
	allowedModels   = map[string]bool{"anthropic.claude-haiku-4-5-20251001-v1:0": true, "amazon.nova-micro-v1:0": true}
	profilePrefixes = []string{"", "us.", "eu.", "apac.", "global."}
	accountIDShape  = regexp.MustCompile(`^\d{12}$`)
)

// ModelAllowed reports whether id is on the allow-list.
func ModelAllowed(id string) bool {
	for _, p := range profilePrefixes {
		if base, ok := strings.CutPrefix(id, p); ok && allowedModels[base] {
			return true
		}
	}
	return false
}

// BedrockConfig is the validated WARDYN_LIVE_BEDROCK_* environment.
type BedrockConfig struct {
	AccountID, RoleName, SSORegion, Region, Model string
	MaxCalls                                      int
}

// LoadBedrockConfig validates the suite's environment. Anything outside the
// limits is an error, never clamped into range.
func LoadBedrockConfig(getenv func(string) string) (BedrockConfig, error) {
	c := BedrockConfig{
		AccountID: getenv(EnvBedrockAccount),
		RoleName:  getenv(EnvBedrockRole),
		SSORegion: getenv(EnvSSORegion),
		Region:    getenv(EnvBedrockRegion),
		Model:     getenv(EnvBedrockModel),
		MaxCalls:  DefaultMaxCalls,
	}
	if !accountIDShape.MatchString(c.AccountID) {
		return c, fmt.Errorf("%s must be the capped member account's 12-digit id", EnvBedrockAccount)
	}
	if c.RoleName == "" || c.SSORegion == "" || c.Region == "" {
		return c, fmt.Errorf("%s, %s and %s are required", EnvBedrockRole, EnvSSORegion, EnvBedrockRegion)
	}
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if !ModelAllowed(c.Model) {
		return c, fmt.Errorf("%s is not on the allow-list (Claude Haiku 4.5, Amazon Nova Micro)", EnvBedrockModel)
	}
	if v := getenv(EnvBedrockMaxCalls); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > HardMaxCalls {
			return c, fmt.Errorf("%s must be 1..%d", EnvBedrockMaxCalls, HardMaxCalls)
		}
		c.MaxCalls = n
	}
	return c, nil
}

// SSOToken is the part of an AWS CLI `sso login` cache file the suite uses.
type SSOToken struct {
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// ErrTokenExpired means the owner has to sign in again; the suite skips on it.
var ErrTokenExpired = errors.New("the Identity Center sign-in has expired")

// LoadSSOToken reads WARDYN_LIVE_AWS_SSO_TOKEN_FILE.
func LoadSSOToken(now time.Time) (string, error) {
	b, err := ReadSecretFile(EnvSSOTokenFile)
	if err != nil {
		return "", err
	}
	var tok SSOToken
	if err := json.Unmarshal(b, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("%s: not an AWS CLI SSO cache file", EnvSSOTokenFile)
	}
	if !tok.ExpiresAt.After(now) {
		return "", ErrTokenExpired
	}
	return tok.AccessToken, nil
}

// Endpoints are the three AWS base URLs the suite talks to; tests point them
// at local fakes.
type Endpoints struct{ Portal, STS, Bedrock string }

// DefaultEndpoints returns the real regional endpoints.
func DefaultEndpoints(c BedrockConfig) Endpoints {
	return Endpoints{
		Portal:  "https://portal.sso." + c.SSORegion + ".amazonaws.com",
		STS:     "https://sts." + c.Region + ".amazonaws.com",
		Bedrock: "https://bedrock-runtime." + c.Region + ".amazonaws.com",
	}
}

// Creds are temporary role credentials.
type Creds struct{ AccessKeyID, SecretAccessKey, SessionToken string }

// Bedrock is a client that has already proven it holds member-account
// credentials. The only way to get one is NewMemberBedrock.
type Bedrock struct {
	cfg   BedrockConfig
	ep    Endpoints
	creds Creds
	hc    *http.Client
}

// spent counts model calls across the whole test process.
var spent atomic.Int64

// NewMemberBedrock exchanges the Identity Center token for role credentials on
// cfg.AccountID and then refuses unless STS says those credentials belong to
// exactly that account.
func NewMemberBedrock(ctx context.Context, cfg BedrockConfig, ssoToken string, ep Endpoints, hc *http.Client) (*Bedrock, error) {
	creds, err := roleCredentials(ctx, hc, ep.Portal, ssoToken, cfg.AccountID, cfg.RoleName)
	if err != nil {
		return nil, err
	}
	b := &Bedrock{cfg: cfg, ep: ep, creds: creds, hc: hc}
	got, err := b.callerAccount(ctx)
	if err != nil {
		return nil, err
	}
	if err := RequireAccount(cfg.AccountID, got); err != nil {
		return nil, err
	}
	return b, nil
}

// RequireAccount refuses unless the credential's account is the capped one.
func RequireAccount(want, got string) error {
	if !accountIDShape.MatchString(want) || got != want {
		return fmt.Errorf("refusing to call Bedrock: the credential's account is not %s (spend is allowed only in the capped member account)", EnvBedrockAccount)
	}
	return nil
}

func roleCredentials(ctx context.Context, hc *http.Client, portal, token, account, role string) (Creds, error) {
	q := url.Values{"account_id": {account}, "role_name": {role}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, portal+"/federation/credentials?"+q.Encode(), nil)
	if err != nil {
		return Creds{}, err
	}
	req.Header.Set("x-amz-sso_bearer_token", token)
	body, err := send(hc, req, "GetRoleCredentials")
	if err != nil {
		return Creds{}, err
	}
	var out struct {
		RoleCredentials struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
		} `json:"roleCredentials"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.RoleCredentials.AccessKeyID == "" {
		return Creds{}, errors.New("GetRoleCredentials: no role credentials in the answer")
	}
	rc := out.RoleCredentials
	return Creds{rc.AccessKeyID, rc.SecretAccessKey, rc.SessionToken}, nil
}

func (b *Bedrock) callerAccount(ctx context.Context) (string, error) {
	form := []byte("Action=GetCallerIdentity&Version=2011-06-15")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.ep.STS+"/", bytes.NewReader(form))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	SignV4(req, form, b.creds, b.cfg.Region, "sts", time.Now())
	body, err := send(b.hc, req, "GetCallerIdentity")
	if err != nil {
		return "", err
	}
	var out struct {
		Account string `xml:"GetCallerIdentityResult>Account"`
	}
	if err := xml.Unmarshal(body, &out); err != nil || out.Account == "" {
		return "", errors.New("GetCallerIdentity: no account in the answer")
	}
	return out.Account, nil
}

// Converse makes one model call. It refuses a max_tokens above MaxTokens and
// any call past the process's budget, before anything is sent.
func (b *Bedrock) Converse(ctx context.Context, prompt string, maxTokens int) (string, error) {
	if maxTokens < 1 || maxTokens > MaxTokens {
		return "", fmt.Errorf("refusing: max_tokens must be 1..%d", MaxTokens)
	}
	if n := spent.Add(1); n > int64(b.cfg.MaxCalls) {
		return "", fmt.Errorf("refusing: call %d is past %s=%d", n, EnvBedrockMaxCalls, b.cfg.MaxCalls)
	}
	payload, _ := json.Marshal(map[string]any{
		"messages":        []any{map[string]any{"role": "user", "content": []any{map[string]string{"text": prompt}}}},
		"inferenceConfig": map[string]any{"maxTokens": maxTokens, "temperature": 0},
	})
	u, err := url.Parse(b.ep.Bedrock + "/model/" + awsEscape(b.cfg.Model) + "/converse")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	SignV4(req, payload, b.creds, b.cfg.Region, "bedrock", time.Now())
	body, err := send(b.hc, req, "Converse")
	if err != nil {
		return "", err
	}
	var out struct {
		Output struct {
			Message struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"message"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", errors.New("Converse: unreadable answer")
	}
	var text strings.Builder
	for _, c := range out.Output.Message.Content {
		text.WriteString(c.Text)
	}
	return text.String(), nil
}

// send performs req and returns the body of a 2xx answer. A failure carries
// the status and a short, redacted slice of the body.
func send(hc *http.Client, req *http.Request, op string) ([]byte, error) {
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", op, Redact(err.Error()))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: reading the answer: %w", op, err)
	}
	if resp.StatusCode/100 != 2 {
		snippet := string(body)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, fmt.Errorf("%s: HTTP %d: %s", op, resp.StatusCode, Redact(snippet))
	}
	return body, nil
}

// SignV4 signs req with AWS Signature Version 4, over host, x-amz-date and,
// when present, content-type and the session token.
func SignV4(req *http.Request, body []byte, c Creds, region, service string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	day := amzDate[:8]
	req.Header.Set("X-Amz-Date", amzDate)
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	headers := map[string]string{"host": req.URL.Host}
	for _, h := range []string{"Content-Type", "X-Amz-Date", "X-Amz-Security-Token"} {
		if v := req.Header.Get(h); v != "" {
			headers[strings.ToLower(h)] = strings.TrimSpace(v)
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	// Every service but S3 signs the path URI-encoded twice: once as sent,
	// once more for the canonical form.
	segs := strings.Split(req.URL.EscapedPath(), "/")
	for i, s := range segs {
		segs[i] = awsEscape(s)
	}
	canonPath := strings.Join(segs, "/")
	if canonPath == "" {
		canonPath = "/"
	}
	q := req.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var pairs []string
	for _, k := range keys {
		for _, v := range q[k] {
			pairs = append(pairs, awsEscape(k)+"="+awsEscape(v))
		}
	}
	canonical := strings.Join([]string{req.Method, canonPath, strings.Join(pairs, "&"),
		canonHeaders.String(), signed, sha256Hex(body)}, "\n")

	scope := day + "/" + region + "/" + service + "/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	key := hmacSHA256([]byte("AWS4"+c.SecretAccessKey), day)
	for _, part := range []string{region, service, "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKeyID+"/"+scope+
		", SignedHeaders="+signed+", Signature="+hex.EncodeToString(hmacSHA256(key, toSign)))
}

// awsEscape is RFC 3986 percent-encoding: unreserved characters stay, every
// other byte becomes %XX.
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
