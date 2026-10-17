// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package awskms is the AWS KMS key service for stored credentials
// (WARDYN_KEK=awskms): each data key is wrapped by a KMS Encrypt call under
// one symmetric key that never leaves AWS.
package awskms

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/smithy-go"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

const (
	wrapLabel = "wardyn/awskms-wrap/v1"
	// contextKey is the one encryption-context key a wrap carries.
	contextKey = "wardyn:aad-sha256"
	probeName  = "wardyn-kek-probe"
	// maxBlob is KMS's CiphertextBlob limit; a longer blob is not a KMS wrap.
	maxBlob        = 6144
	defaultTimeout = 10 * time.Second
	keySetting     = "WARDYN_AWS_KMS_KEY"
)

// keyIDRE is a KMS key id: a UUID, or mrk- and 32 hex digits for a
// multi-Region key.
var keyIDRE = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|mrk-[0-9a-f]{32})$`)

// Config configures the AWS KMS KEK (docs/ENV.md).
type Config struct {
	// KeyARN is the full ARN of the symmetric key, arn:<partition>:kms:<region>:<account>:key/<id>.
	KeyARN  string
	Timeout time.Duration
	// CACertFile is a PEM bundle trusted beside the system roots for the
	// KMS and credential endpoints (WARDYN_TRUSTED_CA_FILE); empty = system roots.
	CACertFile string
	// Endpoint overrides the KMS endpoint; tests only.
	Endpoint string
	// KeySetting names the setting KeyARN came from, for the boot errors;
	// empty = WARDYN_AWS_KMS_KEY.
	KeySetting string
}

// KEK is the AWS KMS KEK. Wrap is KMS Encrypt of the 32-byte data key the
// store already generated (not GenerateDataKey, which would hand back a key
// of KMS's own making and need a second KEK shape), and Unwrap is Decrypt
// with KeyId pinned to the configured ARN.
//
// The wrap is bound to its row by the encryption context, which KMS checks on
// Decrypt: {"wardyn:aad-sha256": hex(sha256(AAD_kek))}. The context is logged
// by CloudTrail in clear, so it carries a hash, never an owner or a name.
// KMS rotation is transparent (a ciphertext names its backing key), so the
// KEK is not kek.Versioned.
type KEK struct {
	client   *kms.Client
	arn      string
	id       string
	region   string
	static   bool
	nodeRole bool
	timeout  time.Duration
}

var _ kek.KEK = (*KEK)(nil)

// NewKEK validates cfg, loads credentials from the SDK's default chain and
// proves the key fits before any row depends on it: a probe data key must
// round-trip, and must not unwrap under another row. Any failure, a
// transient one included, fails boot.
func NewKEK(ctx context.Context, cfg Config) (*KEK, error) {
	setting := cmp.Or(cfg.KeySetting, keySetting)
	a, err := parseKeyARN(setting, cfg.KeyARN)
	if err != nil {
		return nil, err
	}
	timeout := cmp.Or(cfg.Timeout, defaultTimeout)
	// The client is the SDK's own buildable client, not a plain *http.Client:
	// the SDK adds an AWS_CA_BUNDLE (or shared-config ca_bundle) to it, and
	// refuses to boot when handed any other client type. The proxy comes from
	// http.DefaultTransport, which carries WARDYN_DAEMON_PROXY_URL once the
	// caller has installed it (serving boot and each maintenance mode do); the
	// trusted CA is added here, so it does not depend on that order.
	var tlsErr error
	hc := awshttp.NewBuildableClient().WithTransportOptions(func(tr *http.Transport) {
		tr.Proxy = http.DefaultTransport.(*http.Transport).Proxy
		tr.TLSClientConfig, tlsErr = tlsConfig(tr.TLSClientConfig, cfg.CACertFile)
	})
	if tlsErr != nil {
		return nil, fmt.Errorf("AWS KMS: %w", tlsErr)
	}
	sdkCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(a.Region), config.WithHTTPClient(hc))
	if err != nil {
		return nil, fmt.Errorf("AWS KMS: load the AWS configuration: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	creds, err := sdkCfg.Credentials.Retrieve(cctx)
	if err != nil {
		return nil, fmt.Errorf("AWS KMS: no AWS credentials from the default chain (IRSA, EKS Pod Identity, ...): %w", err)
	}
	k := &KEK{
		client: kms.NewFromConfig(sdkCfg, func(o *kms.Options) {
			if cfg.Endpoint != "" {
				o.BaseEndpoint = aws.String(cfg.Endpoint)
			}
		}),
		arn: cfg.KeyARN, id: kek.AWSKMSIDPrefix + cfg.KeyARN, region: a.Region, timeout: timeout,
		static:   strings.HasPrefix(creds.AccessKeyID, "AKIA"),
		nodeRole: creds.Source == ec2rolecreds.ProviderName,
	}
	if err := k.selfTest(ctx); err != nil {
		return nil, fmt.Errorf("AWS KMS KEK %s: %w", k.id, err)
	}
	return k, nil
}

// tlsConfig is base (or a fresh config) with the CA bundle at caFile added to
// the system roots.
func tlsConfig(base *tls.Config, caFile string) (*tls.Config, error) {
	cfg := base.Clone()
	if base == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if caFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read the trusted CA bundle: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("the trusted CA bundle %s holds no PEM certificate", caFile)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// parseKeyARN accepts only a full key ARN: an alias can be re-pointed, and a
// kek_id must name one key forever.
func parseKeyARN(setting, raw string) (arn.ARN, error) {
	if !arn.IsARN(raw) {
		return arn.ARN{}, fmt.Errorf("%s %q is not a key ARN; want arn:<partition>:kms:<region>:<account>:key/<id> (an alias or a bare key id is refused: an alias can be re-pointed to another key)", setting, raw)
	}
	a, err := arn.Parse(raw)
	if err != nil {
		return arn.ARN{}, fmt.Errorf("%s %q: %w", setting, raw, err)
	}
	id, isKey := strings.CutPrefix(a.Resource, "key/")
	switch {
	case a.Service != "kms":
		return arn.ARN{}, fmt.Errorf("%s %q is not a KMS ARN", setting, raw)
	case strings.HasPrefix(a.Resource, "alias/"):
		return arn.ARN{}, fmt.Errorf("%s %q is an alias; give the key's own ARN (arn:<partition>:kms:<region>:<account>:key/<id>): an alias can be re-pointed to another key", setting, raw)
	case !isKey || a.Region == "" || a.AccountID == "" || !keyIDRE.MatchString(id):
		return arn.ARN{}, fmt.Errorf("%s %q is not a key ARN; want arn:<partition>:kms:<region>:<account>:key/<id>", setting, raw)
	}
	return a, nil
}

func (k *KEK) selfTest(ctx context.Context) error {
	dek := make([]byte, kek.DEKSize)
	if _, err := rand.Read(dek); err != nil {
		return err
	}
	bind := kek.Bind("", probeName)
	w, err := k.Wrap(ctx, dek, bind)
	if err != nil {
		return err
	}
	got, err := k.Unwrap(ctx, w, bind)
	if err != nil {
		return fmt.Errorf("a probe data key did not unwrap: %w", err)
	}
	if !bytes.Equal(got, dek) {
		return errors.New("a probe data key unwrapped to different bytes")
	}
	// Only KMS's own refusal of the context proves the binding: any other
	// answer (an access error, a missing key, a proxy's 4xx) says nothing about it.
	_, err = k.Unwrap(ctx, w, kek.Bind("", probeName+"-moved"))
	switch {
	case err == nil:
		return errors.New("refusing it: a probe data key unwrapped under another row, so this key service does not bind a wrap to its row")
	case errors.Is(err, secretstore.ErrUnavailable):
		return err
	case !errors.Is(err, kek.ErrRefused):
		return fmt.Errorf("a probe data key moved to another row was answered with %v, not KMS's refusal of the context, so the binding is unproven", err)
	}
	return nil
}

// ID implements kek.KEK: "awskms:<key ARN>".
func (k *KEK) ID() string { return k.id }

// Describe names the key service for an operator ("AWS KMS in us-east-1").
func (k *KEK) Describe() string { return "AWS KMS in " + k.region }

// StaticKey reports whether the credentials the default chain gave at boot
// are a long-lived IAM user access key (AKIA...), not a role's temporary ones.
func (k *KEK) StaticKey() bool { return k.static }

// NodeRole reports whether those credentials are the EC2 node's instance role,
// shared by every pod on the node, not a pod's own role (IRSA, Pod Identity).
func (k *KEK) NodeRole() bool { return k.nodeRole }

// encryptionContext binds a wrap to its row by a hash of AAD_kek.
func (k *KEK) encryptionContext(bind map[string]string) (map[string]string, error) {
	aad, err := kek.WrapAAD(bind, k.id)
	if err != nil {
		return nil, fmt.Errorf("awskms KEK: %w", err)
	}
	sum := sha256.Sum256(aad)
	return map[string]string{contextKey: hex.EncodeToString(sum[:])}, nil
}

// Wrap implements kek.KEK.
func (k *KEK) Wrap(ctx context.Context, dek []byte, bind map[string]string) ([]byte, error) {
	if len(dek) != kek.DEKSize {
		return nil, fmt.Errorf("awskms KEK: data key is %d bytes, want %d", len(dek), kek.DEKSize)
	}
	ec, err := k.encryptionContext(bind)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, k.timeout)
	defer cancel()
	out, err := k.client.Encrypt(ctx, &kms.EncryptInput{
		KeyId: aws.String(k.arn), Plaintext: dek, EncryptionContext: ec, EncryptionAlgorithm: kmstypes.EncryptionAlgorithmSpecSymmetricDefault,
	})
	if err != nil {
		return nil, k.serviceErr(err)
	}
	if aws.ToString(out.KeyId) != k.arn {
		return nil, k.answerErr(fmt.Errorf("KMS Encrypt answered for key %q, not the configured key", aws.ToString(out.KeyId)))
	}
	if n := len(out.CiphertextBlob); n == 0 || n > maxBlob {
		return nil, k.answerErr(fmt.Errorf("KMS Encrypt answered a %d-byte ciphertext", n))
	}
	return kek.Encode(wrapLabel, string(out.CiphertextBlob)), nil
}

// Unwrap implements kek.KEK. A wrap of another format is a refusal that is
// not proof about the row (a reader fails closed); one of this format that
// KMS will not open under this row's context, or a blob that is not a KMS
// ciphertext, is kek.ErrRefused: KMS cannot tell a moved row from a wrong key.
func (k *KEK) Unwrap(ctx context.Context, wrapped []byte, bind map[string]string) ([]byte, error) {
	ec, err := k.encryptionContext(bind)
	if err != nil {
		return nil, err
	}
	blob, err := parseWrap(wrapped)
	switch {
	case errors.Is(err, errUnknownFormat):
		return nil, fmt.Errorf("awskms KEK %s: the row holds %w", k.id, err)
	case err != nil:
		return nil, fmt.Errorf("awskms KEK %s: %w: the row holds %w", k.id, kek.ErrCorrupt, err)
	}
	ctx, cancel := context.WithTimeout(ctx, k.timeout)
	defer cancel()
	out, err := k.client.Decrypt(ctx, &kms.DecryptInput{
		KeyId: aws.String(k.arn), CiphertextBlob: blob, EncryptionContext: ec, EncryptionAlgorithm: kmstypes.EncryptionAlgorithmSpecSymmetricDefault,
	})
	if err != nil {
		return nil, k.serviceErr(err)
	}
	if aws.ToString(out.KeyId) != k.arn {
		clear(out.Plaintext)
		return nil, k.answerErr(fmt.Errorf("KMS Decrypt answered for key %q, not the configured key", aws.ToString(out.KeyId)))
	}
	if len(out.Plaintext) != kek.DEKSize {
		clear(out.Plaintext)
		return nil, k.answerErr(fmt.Errorf("KMS Decrypt did not answer a %d-byte data key", kek.DEKSize))
	}
	return out.Plaintext, nil
}

var errUnknownFormat = errors.New("a wrap whose format is not " + wrapLabel + ": written by a newer wardynd, or not an AWS KMS wrap")

func parseWrap(w []byte) ([]byte, error) {
	var fs []string
	for b := w; len(b) > 0; {
		if len(b) < 4 {
			return nil, errors.New("no AWS KMS wrap: truncated")
		}
		n := binary.BigEndian.Uint32(b)
		b = b[4:]
		if uint64(n) > uint64(len(b)) {
			return nil, errors.New("no AWS KMS wrap: truncated")
		}
		fs = append(fs, string(b[:n]))
		b = b[n:]
	}
	switch {
	case len(fs) == 0 || fs[0] != wrapLabel:
		return nil, errUnknownFormat
	case len(fs) != 2 || len(fs[1]) == 0 || len(fs[1]) > maxBlob:
		return nil, errors.New("an AWS KMS wrap with a missing or oversized ciphertext")
	}
	return []byte(fs[1]), nil
}

// serviceErr classifies a KMS call failure. Only an unreachable or throttled
// service is transient (secretstore.ErrUnavailable); every other answer KMS
// gives is definitive (kek.ErrService), with the specific kek error beside it.
func (k *KEK) serviceErr(err error) error {
	var api smithy.APIError
	if !errors.As(err, &api) {
		return fmt.Errorf("awskms KEK %s: %w: %w", k.id, secretstore.ErrUnavailable, err)
	}
	status := 0
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		status = re.HTTPStatusCode()
	}
	switch code := api.ErrorCode(); {
	case code == "AccessDeniedException" || status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("awskms KEK %s: %w: %w: %w", k.id, kek.ErrService, kek.ErrAccess, err)
	case code == "NotFoundException":
		return fmt.Errorf("awskms KEK %s: %w: %w: %w", k.id, kek.ErrService, kek.ErrKeyMissing, err)
	case code == "InvalidCiphertextException" || code == "IncorrectKeyException":
		return fmt.Errorf("awskms KEK %s: %w: %w: %w", k.id, kek.ErrService, kek.ErrRefused, err)
	case transientCode(code) || status == http.StatusTooManyRequests || status >= 500:
		return fmt.Errorf("awskms KEK %s: %w: %w", k.id, secretstore.ErrUnavailable, err)
	}
	return fmt.Errorf("awskms KEK %s: %w: %w", k.id, kek.ErrService, err)
}

func transientCode(code string) bool {
	switch code {
	case "ThrottlingException", "RequestLimitExceeded", "KMSInternalException", "DependencyTimeoutException", "ServiceUnavailableException":
		return true
	}
	return false
}

// answerErr is an answer KMS gave that does not fit what was asked.
func (k *KEK) answerErr(err error) error {
	return fmt.Errorf("awskms KEK %s: %w: %w", k.id, kek.ErrService, err)
}
