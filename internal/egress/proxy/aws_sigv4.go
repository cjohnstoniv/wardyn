// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// AWS SigV4 re-signing at the proxy (WARDYN_AWS_SIGV4_AT_PROXY, off by default).
//
// With it on, the sandbox's AWS clients hold a placeholder key pair and sign with it as they
// always do. The proxy terminates TLS for the run's AWS service hosts, checks each request is
// one it can re-sign for that host, drops the sandbox's signature and signs the request again
// with role credentials it gets itself from IAM Identity Center's GetRoleCredentials, using the
// captured sign-in behind the run's grant. Neither the sign-in nor the role credentials reach
// the sandbox. A request the proxy cannot re-sign is refused with a named reason and never
// forwarded with the placeholder.

const (
	ruleSourceAWSSigV4        = "brokered:aws-sigv4"
	ruleSourceAWSSigV4Refused = "brokered:aws-sigv4:refused"

	// AWSSigV4PlaceholderAccessKeyID and AWSSigV4PlaceholderSecretAccessKey are the key pair the
	// sandbox holds under re-signing. They authorise nothing; the proxy refuses a request signed
	// with any other key.
	AWSSigV4PlaceholderAccessKeyID     = "WARDYNRESIGNSATPROXY"
	AWSSigV4PlaceholderSecretAccessKey = "wardyn-proxy-re-signs-every-request-0000"

	// awsSigV4Port is the one port re-signing terminates.
	awsSigV4Port = 443
	// awsSigV4MaxHashedBody bounds a body the proxy must read whole to hash: one the client sent
	// without x-amz-content-sha256.
	awsSigV4MaxHashedBody = 16 << 20
	// awsSigV4CredentialMargin renews the role credentials this long before they expire.
	awsSigV4CredentialMargin = 5 * time.Minute
	// awsSSOFederationPath is the portal's GetRoleCredentials.
	awsSSOFederationPath = "/federation/credentials"
)

// The named reasons a re-signing refusal carries, in the X-Wardyn-Reason header and its message.
const (
	awsSigV4NotPlaceholder = "aws_sigv4_not_placeholder"
	awsSigV4Presigned      = "aws_sigv4_presigned"
	awsSigV4Algorithm      = "aws_sigv4_algorithm"
	awsSigV4Scope          = "aws_sigv4_scope"
	awsSigV4SignedChunks   = "aws_sigv4_signed_chunks"
	awsSigV4EventStream    = "aws_sigv4_event_stream"
	awsSigV4Payload        = "aws_sigv4_payload"
	awsSigV4BodyTooLarge   = "aws_sigv4_body_too_large"
	awsSigV4Credentials    = "aws_sigv4_credentials"
	awsSigV4Action         = "aws_sigv4_action"
)

// AWSSigV4Config is the run's re-signing grant: the captured sign-in (GrantID, which resolves
// to the portal's bearer header), the account and role whose credentials sign, the portal that
// issues them, and the exact service hosts the proxy re-signs on.
type AWSSigV4Config struct {
	GrantID    uuid.UUID `json:"grant_id"`
	PortalHost string    `json:"portal_host"`
	AccountID  string    `json:"account_id"`
	RoleName   string    `json:"role_name"`
	Hosts      []string  `json:"hosts"`
}

// awsSigV4Scope is the service and region a host signs for.
type awsSigV4HostScope struct{ service, region string }

// awsSigV4Door is the compiled grant and the role credentials it last obtained.
type awsSigV4Door struct {
	cfg    AWSSigV4Config
	scopes map[string]awsSigV4HostScope
	signer *v4.Signer

	mu    sync.Mutex
	creds aws.Credentials
}

// newAWSSigV4Door compiles the grant and adds its hosts to the MITM-eligible set on port 443.
// Nil without a grant.
func newAWSSigV4Door(g *AWSSigV4Config, mitmHosts map[string]bool, mitmPorts map[string]int) *awsSigV4Door {
	if g == nil {
		return nil
	}
	d := &awsSigV4Door{cfg: *g, scopes: map[string]awsSigV4HostScope{}, signer: v4.NewSigner()}
	for _, h := range g.Hosts {
		service, region, ok := AWSSigV4Scope(h)
		if !ok {
			continue // validate refused this config at boot
		}
		k := hostKey(h)
		d.scopes[k] = awsSigV4HostScope{service: service, region: region}
		mitmHosts[k], mitmPorts[k] = true, awsSigV4Port
	}
	return d
}

// awsServiceLabel is a label that can name an AWS service's endpoint.
var awsServiceLabel = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// awsBucketLabel is one label of an S3 bucket name in a virtual-hosted bucket host.
var awsBucketLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// awsAccountID is an AWS account id.
var awsAccountID = regexp.MustCompile(`^\d{12}$`)

// awsRoleName is an IAM role name (the permission set's role the portal names).
var awsRoleName = regexp.MustCompile(`^[\w+=,.@-]{1,64}$`)

// awsSigV4SigningNames maps an endpoint label to the signing name the SDKs sign it with, where
// the two differ.
var awsSigV4SigningNames = map[string]string{
	"bedrock-runtime":       "bedrock",
	"bedrock-agent-runtime": "bedrock",
	"bedrock-agent":         "bedrock",
}

// awsSigV4NeverResigned are the sign-in services: their requests carry no SigV4 signature, and
// the run's sign-in stays with the proxy.
var awsSigV4NeverResigned = map[string]bool{"sso": true, "oidc": true, "portal": true, "signin": true}

// AWSSigV4Scope reports the signing service and region of an AWS service host the proxy can
// re-sign, from the host alone: <service>.<region>.amazonaws.com, a global
// <service>.amazonaws.com (signed for us-east-1), or an S3 virtual-hosted bucket
// <bucket>.s3[.<region>].amazonaws.com. Any other shape (dual-stack, PrivateLink, account-
// prefixed control planes, other partitions) and the sign-in hosts are not re-signed: a run
// with one stays on the path it had before re-signing.
func AWSSigV4Scope(host string) (service, region string, ok bool) {
	rest, found := strings.CutSuffix(hostKey(host), ".amazonaws.com")
	if !found || rest == "" {
		return "", "", false
	}
	labels := strings.Split(rest, ".")
	region = "us-east-1"
	if n := len(labels); n > 1 && awsRegionLabel.MatchString(labels[n-1]) {
		region, labels = labels[n-1], labels[:n-1]
	}
	label := labels[len(labels)-1]
	if (len(labels) > 1 && label != "s3") || !awsServiceLabel.MatchString(label) ||
		awsRegionLabel.MatchString(label) || awsSigV4NeverResigned[label] {
		return "", "", false
	}
	for _, bucket := range labels[:len(labels)-1] {
		if !awsBucketLabel.MatchString(bucket) {
			return "", "", false // a wildcard or anything else that is not one exact bucket host
		}
	}
	service = cmp.Or(awsSigV4SigningNames[label], label)
	for guarded := range awsSigV4ActionGuards {
		if strings.HasPrefix(label, guarded+"-") {
			service = guarded // sts-fips, iam-fips: the family signs, and is guarded, as one service
		}
	}
	return service, region, true
}

// isAWSSigV4Host reports whether host is one the run re-signs on.
func (p *Proxy) isAWSSigV4Host(host string) bool {
	if p.sigv4 == nil {
		return false
	}
	_, ok := p.sigv4.scopes[hostKey(host)]
	return ok
}

// awsSigV4Plan is what admission decided: the payload hash to sign with ("" hashes the body)
// and the headers the sandbox signed, which are the ones the proxy signs again.
type awsSigV4Plan struct {
	payload string
	signed  []string
	service string // the service the sandbox signed for
}

// awsSigV4Refusal is a request the proxy will not re-sign.
type awsSigV4Refusal struct{ code, why string }

// admitAWSSigV4 checks a request is one the proxy can re-sign for scope: signed with the
// placeholder in a header (not a presigned URL), with SigV4 (not SigV4a), for the host's own
// service and region, over a payload the proxy can sign again — a declared hash, an unsigned
// payload, or a body it hashes itself. A chunk-signed upload and an event-stream body sign every
// chunk with a chain that starts at the sandbox's signature, so neither can be re-signed.
func admitAWSSigV4(r *http.Request, scope awsSigV4HostScope) (awsSigV4Plan, *awsSigV4Refusal) {
	for k := range r.URL.Query() {
		if strings.EqualFold(k, "X-Amz-Signature") || strings.EqualFold(k, "X-Amz-Credential") || strings.EqualFold(k, "X-Amz-Algorithm") {
			return awsSigV4Plan{}, &awsSigV4Refusal{awsSigV4Presigned,
				"it is a presigned URL; Wardyn re-signs only requests signed in the Authorization header"}
		}
	}
	auths := r.Header.Values("Authorization")
	if len(auths) != 1 {
		return awsSigV4Plan{}, &awsSigV4Refusal{awsSigV4NotPlaceholder,
			"it is not signed with this run's placeholder AWS key, and Wardyn re-signs only requests that are"}
	}
	algo, params, _ := strings.Cut(strings.TrimSpace(auths[0]), " ")
	switch algo {
	case "AWS4-HMAC-SHA256":
	case "AWS4-ECDSA-P256-SHA256":
		return awsSigV4Plan{}, &awsSigV4Refusal{awsSigV4Algorithm,
			"it is signed with SigV4a (multi-Region), which Wardyn does not re-sign; sign for one Region"}
	default:
		return awsSigV4Plan{}, &awsSigV4Refusal{awsSigV4NotPlaceholder,
			"it is not signed with this run's placeholder AWS key, and Wardyn re-signs only requests that are"}
	}
	var credential, signedHeaders string
	for f := range strings.SplitSeq(params, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
		switch k {
		case "Credential":
			credential = v
		case "SignedHeaders":
			signedHeaders = v
		}
	}
	parts := strings.Split(credential, "/")
	if len(parts) != 5 || parts[0] != AWSSigV4PlaceholderAccessKeyID || parts[4] != "aws4_request" || signedHeaders == "" {
		return awsSigV4Plan{}, &awsSigV4Refusal{awsSigV4NotPlaceholder,
			"it is not signed with this run's placeholder AWS key, and Wardyn re-signs only requests that are"}
	}
	if parts[2] != scope.region || parts[3] != scope.service {
		return awsSigV4Plan{}, &awsSigV4Refusal{awsSigV4Scope, fmt.Sprintf(
			"it is signed for service %q in %q, and this host takes service %q in %q", parts[3], parts[2], scope.service, scope.region)}
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/vnd.amazon.eventstream" {
		return awsSigV4Plan{}, &awsSigV4Refusal{awsSigV4EventStream,
			"its body is an event stream whose every message is signed in a chain from the request's signature, which Wardyn cannot re-sign"}
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	switch {
	case payload == "" || payload == "UNSIGNED-PAYLOAD" || payload == "STREAMING-UNSIGNED-PAYLOAD-TRAILER":
	case strings.HasPrefix(payload, "STREAMING-"):
		return awsSigV4Plan{}, &awsSigV4Refusal{awsSigV4SignedChunks,
			"it is a chunk-signed upload whose every chunk is signed in a chain from the request's signature, which Wardyn cannot re-sign; " +
				"turn off chunk signing (for the AWS CLI, `payload_signing_enabled = false`)"}
	case len(payload) != sha256.Size*2 || !isHex(payload):
		return awsSigV4Plan{}, &awsSigV4Refusal{awsSigV4Payload, "its x-amz-content-sha256 header is not a payload hash Wardyn can sign"}
	}
	return awsSigV4Plan{payload: payload, signed: strings.Split(signedHeaders, ";"), service: parts[3]}, nil
}

// awsSigV4ActionGuard is how the proxy judges a query-protocol service's action before it
// re-signs: STS re-signs only the actions in allow, since its others hand out credentials, and
// IAM only the actions that start with one of reads, since any IAM write can create a credential
// for some principal. Both compare exactly. A token API another service offers (ECR or
// CodeArtifact GetAuthorizationToken, say) is not guarded.
type awsSigV4ActionGuard struct {
	allow, reads []string
	why          string
}

// awsSigV4ActionGuards are the guarded services, by signing name. Every host of either family
// (FIPS included, see AWSSigV4Scope) signs under that name.
var awsSigV4ActionGuards = map[string]awsSigV4ActionGuard{
	"sts": {allow: []string{"GetCallerIdentity"},
		why: "Wardyn re-signs only GetCallerIdentity on AWS STS; its other actions hand out credentials, which would put them in the sandbox"},
	"iam": {reads: []string{"Get", "List", "Simulate", "Generate"},
		why: "Wardyn re-signs only IAM actions that read (Get, List, Simulate, Generate); any IAM write can create a credential for some principal"},
}

// awsActionName is the shape of an AWS action name; a value of any other shape (empty, padded,
// carrying a NUL or punctuation) is refused rather than judged, since the service may read it
// differently than a list compare does.
var awsActionName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)

// awsQueryKey is the shape of a query-protocol parameter name (Action, Version, Tags.member.1.Key).
// A key of any other shape (a NUL, a tab) could be an Action the service reads and a compare
// misses, so a guarded request carrying one is refused.
var awsQueryKey = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// awsSigV4ActionRefusal judges a guarded request on what the service runs: the one Action its
// query and plain form body name between them. A request the proxy cannot read that way is
// refused: an action named in a header (X-Amz-Target, Smithy-Protocol), any Content-Encoding, a
// chunked or streamed body, a body or content type that is not a form (or more than one Content-Type),
// a query or form that does not parse, and no Action, more than one, one not shaped like an action
// name, or one the guard refuses. "" admits it.
func awsSigV4ActionRefusal(r *http.Request, payload string, body []byte, g awsSigV4ActionGuard) string {
	ct := r.Header.Get("Content-Type")
	mt, _, err := mime.ParseMediaType(ct)
	switch {
	case len(r.Header.Values("X-Amz-Target")) > 0 || len(r.Header.Values("Smithy-Protocol")) > 0:
		return "it names its action in a header, and Wardyn judges this service's action only from the Action parameter"
	case len(r.Header.Values("Content-Type")) > 1:
		return "it carries more than one Content-Type, and Wardyn judges this service's action only from a plain form body"
	case len(r.Header.Values("Content-Encoding")) > 0 || len(r.TransferEncoding) > 0 || strings.HasPrefix(payload, "STREAMING-"):
		return "its body is encoded, chunked or streamed, and Wardyn judges this service's action only from a plain form body"
	case (ct != "" || len(body) > 0) && (err != nil || mt != "application/x-www-form-urlencoded"):
		return "its body is not a form, and Wardyn judges this service's action only from a plain form body"
	}
	inQuery, okQuery := awsQueryActions(r.URL.RawQuery)
	inBody, okBody := awsQueryActions(string(body))
	actions := slices.Concat(inQuery, inBody)
	switch {
	case !okQuery || !okBody:
		return "its query or form body does not parse, so Wardyn cannot tell which action it runs"
	case len(actions) != 1:
		return fmt.Sprintf("it names %d actions between its query and its body, and Wardyn re-signs only a request that names exactly one", len(actions))
	case !awsActionName.MatchString(actions[0]):
		return fmt.Sprintf("its Action %q is not an action name, and Wardyn re-signs only one it can read", actions[0])
	case (g.allow != nil && !slices.Contains(g.allow, actions[0])) || // exact: folding would widen an allow list
		(g.reads != nil && !slices.ContainsFunc(g.reads, func(p string) bool { return strings.HasPrefix(actions[0], p) })):
		return g.why
	}
	return ""
}

// awsQueryActions is every Action value a query string or form body names, under any case of
// the name; false when it does not parse or names a parameter not shaped like one (awsQueryKey).
func awsQueryActions(raw string) ([]string, bool) {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return nil, false
	}
	var out []string
	for k, v := range q {
		if !awsQueryKey.MatchString(k) {
			return nil, false
		}
		if strings.EqualFold(k, "Action") {
			out = append(out, v...)
		}
	}
	return out, true
}

// serveAWSSigV4 is the door: admit, hash, obtain the role credentials, forward re-signed.
func (p *Proxy) serveAWSSigV4(w http.ResponseWriter, r *http.Request, host string, port int) {
	scope := p.sigv4.scopes[hostKey(host)]
	plan, ref := admitAWSSigV4(r, scope)
	if ref != nil {
		p.refuseAWSSigV4(w, r, host, port, scope, ref.code, ref.why)
		return
	}
	// A guarded service is judged on the body it runs, so its body is always read. The host's
	// service and the one the sandbox signed for are the same here (admission refused any other);
	// either one being guarded guards the request.
	guard, guarded := awsSigV4ActionGuards[scope.service]
	if g, ok := awsSigV4ActionGuards[plan.service]; ok {
		guard, guarded = g, true
	}
	var body []byte
	if (plan.payload == "" || guarded) && r.Body != nil && r.Body != http.NoBody {
		// Charged to the engine's retained-bytes budget for as long as the request holds it.
		buf, release, err := p.scan.readBody(r.Context(), r.Body, r.ContentLength, awsSigV4MaxHashedBody)
		defer release()
		switch {
		case errors.Is(err, errScanBusy):
			p.refuseAWSSigV4(w, r, host, port, scope, awsSigV4BodyTooLarge, "Wardyn had no room to hold its body to sign it; retry")
			return
		case err != nil:
			p.refuseAWSSigV4(w, r, host, port, scope, awsSigV4Payload, "Wardyn could not read its body to sign it")
			return
		case len(buf) > awsSigV4MaxHashedBody:
			p.refuseAWSSigV4(w, r, host, port, scope, awsSigV4BodyTooLarge, fmt.Sprintf(
				"its body is over %d MiB and carries no x-amz-content-sha256 header, so Wardyn would have to hold all of it to sign it", awsSigV4MaxHashedBody>>20))
			return
		}
		body = buf
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		r.Header.Del("Transfer-Encoding")
	}
	payload := plan.payload
	if guarded {
		if why := awsSigV4ActionRefusal(r, payload, body, guard); why != "" {
			p.refuseAWSSigV4(w, r, host, port, scope, awsSigV4Action, why)
			return
		}
		payload = "" // signed over the body it was judged on
	}
	if payload == "" {
		sum := sha256.Sum256(body)
		payload = hex.EncodeToString(sum[:])
	}
	creds, err := p.awsSigV4Credentials(r.Context())
	switch {
	case err != nil && r.Context().Err() != nil:
		return // the client hung up; nothing was refused
	case err != nil:
		slog.Warn("proxy: the AWS role credentials for re-signing could not be obtained", "host", host, "err", err)
		p.refuseAWSSigV4(w, r, host, port, scope, awsSigV4Credentials, adoControlPlaneRefusal(err,
			"Wardyn could not get this run's AWS role credentials from the run owner's AWS sign-in; the owner may need to sign in to AWS again"))
		return
	}
	rearmBodyDeadline(w) // the credential fetch may have taken a while
	p.forwardAPIDoor(w, r, host, port, apiDoor{allow: ruleSourceAWSSigV4, denied: ruleSourceAWSSigV4Refused,
		mintFailed: "aws re-signing", upstream: "aws", stream: true,
		sign: func(out *http.Request) error {
			return awsSigV4Sign(p.sigv4.signer, out, plan.signed, payload, scope, creds, time.Now())
		}})
}

// awsSigV4Sign signs out for scope with creds, over the headers the sandbox signed and no
// others. It signs a shadow of out, so the query and path go upstream exactly as the sandbox
// sent them, and copies back the three headers a signature sets.
func awsSigV4Sign(s *v4.Signer, out *http.Request, signed []string, payload string, scope awsSigV4HostScope, creds aws.Credentials, now time.Time) error {
	for _, k := range []string{"Authorization", "X-Amz-Date", "X-Amz-Security-Token"} {
		out.Header.Del(k)
	}
	u := *out.URL
	shadow := &http.Request{Method: out.Method, URL: &u, Host: out.Host, Header: http.Header{}, ContentLength: out.ContentLength}
	for _, k := range signed {
		switch k = strings.ToLower(strings.TrimSpace(k)); k {
		case "host", "content-length", "authorization", "x-amz-date", "x-amz-security-token":
			continue
		}
		if v := out.Header.Values(k); len(v) > 0 {
			shadow.Header[http.CanonicalHeaderKey(k)] = v
		}
	}
	if err := s.SignHTTP(context.Background(), creds, shadow, payload, scope.service, scope.region, now,
		func(o *v4.SignerOptions) { o.DisableURIPathEscaping = scope.service == "s3" }); err != nil {
		return err
	}
	for _, k := range []string{"Authorization", "X-Amz-Date", "X-Amz-Security-Token"} {
		if v := shadow.Header.Get(k); v != "" {
			out.Header.Set(k, v)
		}
	}
	return nil
}

// awsSigV4Credentials returns the role credentials the proxy signs with, renewed before they
// expire: the grant resolves to the portal's bearer header, which GetRoleCredentials redeems.
func (p *Proxy) awsSigV4Credentials(ctx context.Context) (aws.Credentials, error) {
	d := p.sigv4
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creds.HasKeys() && time.Now().Before(d.creds.Expires.Add(-awsSigV4CredentialMargin)) {
		return d.creds, nil
	}
	if p.inject == nil {
		return aws.Credentials{}, errors.New("no control plane to resolve the run's AWS sign-in from")
	}
	// The injector's control-plane client and token: the grant resolves as every injection does.
	ri, err := resolveInjectionQuery(ctx, p.inject.base, p.inject.token.Get(), d.cfg.GrantID, nil, p.inject.client)
	if err != nil {
		return aws.Credentials{}, err
	}
	registerHeaderCredentialFor(p.maskKey, ri.Value)
	creds, err := p.getRoleCredentials(ctx, ri.Header, ri.Value)
	if err != nil {
		return aws.Credentials{}, err
	}
	procMaskFor(p.maskKey, []byte(creds.SecretAccessKey))
	procMaskFor(p.maskKey, []byte(creds.SessionToken))
	d.creds = creds
	return creds, nil
}

// errRoleCredentials is a GetRoleCredentials answer the proxy cannot sign with.
var errRoleCredentials = errors.New("GetRoleCredentials returned no usable role credentials")

// getRoleCredentials calls the portal's GetRoleCredentials for the grant's account and role
// with the resolved bearer header. The portal decides how long they live (the permission set's
// session duration); the call takes no duration.
func (p *Proxy) getRoleCredentials(ctx context.Context, header, value string) (aws.Credentials, error) {
	d := p.sigv4
	host := d.cfg.PortalHost
	target, _, err := p.egressTarget(host, awsSigV4Port)
	if err != nil {
		return aws.Credentials{}, err
	}
	scheme, defaultPort := p.upstreamSchemeFor(host, awsSigV4Port)
	u := url.URL{Scheme: scheme, Host: upstreamURLHost(host, awsSigV4Port, defaultPort), Path: awsSSOFederationPath,
		RawQuery: url.Values{"account_id": {d.cfg.AccountID}, "role_name": {d.cfg.RoleName}}.Encode()}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, vettedIPKey{}, target), http.MethodGet, u.String(), nil)
	if err != nil {
		return aws.Credentials{}, err
	}
	req.Header.Set(header, value)
	resp, err := p.roundTripUpstream(req)
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("GetRoleCredentials: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return aws.Credentials{}, fmt.Errorf("GetRoleCredentials answered %d", resp.StatusCode)
	}
	var out struct {
		RoleCredentials struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
			Expiration      int64  `json:"expiration"`
		} `json:"roleCredentials"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return aws.Credentials{}, errRoleCredentials
	}
	rc := out.RoleCredentials
	if rc.AccessKeyID == "" || rc.SecretAccessKey == "" || rc.SessionToken == "" || rc.Expiration == 0 {
		return aws.Credentials{}, errRoleCredentials
	}
	return aws.Credentials{AccessKeyID: rc.AccessKeyID, SecretAccessKey: rc.SecretAccessKey, SessionToken: rc.SessionToken,
		Source: "wardyn-proxy", CanExpire: true, Expires: time.UnixMilli(rc.Expiration)}, nil
}

// refuseAWSSigV4 is the door's one refusal point: a decision row under
// brokered:aws-sigv4:refused, the reason in X-Wardyn-Reason, and a 403 in the error shape the
// service's SDK parses (S3's XML, the query protocol's XML, or the JSON protocols' body), so the
// client prints the sentence.
func (p *Proxy) refuseAWSSigV4(w http.ResponseWriter, r *http.Request, host string, port int, scope awsSigV4HostScope, code, why string) {
	p.emitPATAPIDecision(r, host, port, egress.Deny, ruleSourceAWSSigV4Refused)
	msg := "Wardyn refused this AWS request (" + code + "): " + why + "."
	w.Header().Set("X-Wardyn-Reason", code)
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var esc bytes.Buffer
	_ = xml.EscapeText(&esc, []byte(msg))
	switch {
	case scope.service == "s3":
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code><Message>"+esc.String()+"</Message></Error>")
	case r.Header.Get("X-Amz-Target") == "" && (mt == "application/x-www-form-urlencoded" || r.URL.Query().Has("Action")):
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<ErrorResponse><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>"+esc.String()+"</Message></Error></ErrorResponse>")
	default:
		writeAWSSDKError(w, http.StatusForbidden, "AccessDeniedException", msg)
	}
}

// refuseAWSSigV4Plain refuses, on the plain forward lane, every request to a re-signed host,
// and reports whether it did: that lane never runs the door, so the placeholder would go
// upstream as it is.
func (p *Proxy) refuseAWSSigV4Plain(w http.ResponseWriter, r *http.Request) bool {
	if p.sigv4 == nil || r.URL == nil {
		return false
	}
	host, port := splitHostPort(r.URL.Host, defaultPortForScheme(r.URL.Scheme))
	if !p.isAWSSigV4Host(host) {
		return false
	}
	p.refuseAWSSigV4(w, r, host, port, p.sigv4.scopes[hostKey(host)], awsSigV4Scope,
		"this host is reached only through Wardyn's checked HTTPS tunnel (CONNECT), not as a plain proxy request")
	return true
}

// refuseAWSSigV4Tunnel refuses a CONNECT to a re-signed host the proxy will not terminate
// (another port): a blind tunnel would carry the placeholder past the door.
func (p *Proxy) refuseAWSSigV4Tunnel(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	if !p.isAWSSigV4Host(host) {
		return false
	}
	p.refuseAWSSigV4(w, r, host, port, p.sigv4.scopes[hostKey(host)], awsSigV4Scope,
		"this run reaches this host only through Wardyn's re-signing door on port 443")
	return true
}

// validateAWSSigV4 refuses, at boot, a re-signing grant the proxy could not honour: no MITM CA
// (re-signing runs only on a terminated connection), no grant, a portal, account or role that is
// not one, no host, a host it cannot re-sign, and a host that also carries an injection rule or
// an operator MITM entry — a host carries one credential.
func (c *Config) validateAWSSigV4() error {
	g := c.AWSSigV4
	if g == nil {
		return nil
	}
	if c.MITMCACertPEM == "" || c.MITMCAKeyPEM == "" {
		return fmt.Errorf("config: aws_sigv4 requires mitm_ca_cert_pem and mitm_ca_key_pem — re-signing runs only on a terminated connection")
	}
	if g.GrantID == uuid.Nil {
		return fmt.Errorf("config: aws_sigv4.grant_id is required")
	}
	if g.PortalHost != hostKey(g.PortalHost) || !isAWSSSOPortalHost(g.PortalHost) {
		return fmt.Errorf("config: aws_sigv4.portal_host %q is not an IAM Identity Center portal host", g.PortalHost)
	}
	if !awsAccountID.MatchString(g.AccountID) {
		return fmt.Errorf("config: aws_sigv4.account_id %q is not an AWS account id", g.AccountID)
	}
	if !awsRoleName.MatchString(g.RoleName) {
		return fmt.Errorf("config: aws_sigv4.role_name %q is not an IAM role name", g.RoleName)
	}
	if len(g.Hosts) == 0 {
		return fmt.Errorf("config: aws_sigv4.hosts must name at least one host")
	}
	for i, h := range g.Hosts {
		if _, _, ok := AWSSigV4Scope(h); !ok || h != hostKey(h) || slices.Contains(g.Hosts[:i], h) {
			return fmt.Errorf("config: aws_sigv4.hosts[%d]: %q is not an AWS service host Wardyn re-signs, listed once in lowercase", i, h)
		}
	}
	for i, inj := range c.Injection {
		if slices.Contains(g.Hosts, hostKey(inj.Host)) {
			return fmt.Errorf("config: injection[%d]: host %q is re-signed by aws_sigv4; a host carries one credential", i, inj.Host)
		}
	}
	for i, m := range c.MITMHosts {
		if h, _ := splitHostPort(strings.TrimSpace(m), 0); slices.Contains(g.Hosts, hostKey(h)) {
			return fmt.Errorf("config: mitm_hosts[%d]: host %q is re-signed by aws_sigv4 and is terminated by it alone", i, m)
		}
	}
	return nil
}
