// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// AWS SigV4 re-signing at the proxy (WARDYN_AWS_SIGV4_AT_PROXY, off by default).
//
// Off, both AWS sign-in lanes dispatch exactly what they did before it existed: the sandbox's
// SDK redeems the proxy-injected session for role credentials and signs with them itself, so
// the role credentials are resident. On, a lane whose AWS hosts the proxy can all re-sign
// (proxy.AWSSigV4Scope) dispatches a profile whose credential_process prints a placeholder key
// pair instead of the SSO profile and token cache, and no route to the sign-in hosts; the run's session grant is authored as before but handed to the proxy's
// re-signer (proxy.Config.AWSSigV4), never to the injector, so nothing in the sandbox can
// redeem it. A lane with a host the proxy cannot re-sign (a wildcard, a private endpoint, a
// test endpoint) stays on the path it had.

// ResolveAWSSigV4AtProxy reads WARDYN_AWS_SIGV4_AT_PROXY: on, true or 1 turns re-signing on,
// and anything else, unset included, leaves it off.
func ResolveAWSSigV4AtProxy(raw string) bool {
	return resolveAWSSSOProxyInject(raw, false)
}

// AWSSigV4AtProxyFlagDefault is the default the boot flag advertises.
const AWSSigV4AtProxyFlagDefault = "off"

// awsSigV4Hosts is the lane's hosts as the proxy re-signs them, and whether re-signing applies:
// the switch is on (with the proxy-side session it builds on, and no test endpoint), and every
// host is an exact AWS service host on port 443 the proxy can re-sign. One host it cannot
// re-sign keeps the whole lane on its old path: a lane is signed one way.
func (s *Server) awsSigV4Hosts(hosts []string) ([]string, bool) {
	if !s.cfg.AWSSigV4AtProxy || !s.cfg.AWSSSOProxyInject || s.cfg.AWSSSOEndpointOverride != "" || len(hosts) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		host := strings.ToLower(strings.TrimSpace(h))
		if hh, port, err := net.SplitHostPort(host); err == nil {
			if port != "443" {
				return nil, false
			}
			host = hh
		}
		host = strings.TrimSuffix(host, ".")
		if _, _, ok := proxy.AWSSigV4Scope(host); !ok {
			return nil, false
		}
		if !slices.Contains(out, host) {
			out = append(out, host)
		}
	}
	return out, true
}

// awsSigV4CredentialFile is the file, beside ~/.aws/config, that holds the placeholder pair in
// the process-credential shape.
const awsSigV4CredentialFile = "wardyn-placeholder-credentials.json"

// awsSigV4ConfigFileContents is a re-signed lane's ~/.aws/config: the one profile, whose
// credential_process prints the placeholder pair the SDK signs with (the proxy replaces the
// signature on every request), and the Region when the lane names one. A process credential
// rather than environment keys, so the profile still decides, as it does on the SSO lane.
func awsSigV4ConfigFileContents(region string) string {
	cfg := fmt.Sprintf("[profile %s]\ncredential_process = cat %s/%s\noutput = json\n", awsSSOProfileName, sandboxAWSDir, awsSigV4CredentialFile)
	if region != "" {
		cfg += "region = " + region + "\n"
	}
	return cfg
}

// awsSigV4CredentialFileContents is the placeholder pair as a credential_process prints it.
func awsSigV4CredentialFileContents() string {
	b, _ := json.Marshal(map[string]any{"Version": 1,
		"AccessKeyId": proxy.AWSSigV4PlaceholderAccessKeyID, "SecretAccessKey": proxy.AWSSigV4PlaceholderSecretAccessKey})
	return string(b)
}

// stageAWSSigV4Files is the sandbox side of a re-signed lane: stageAWSSSOFiles's profile with
// the placeholder in place of the SSO session, and no token cache.
func stageAWSSigV4Files(env map[string]string, region string) {
	env[envAWSConfigFile] = sandboxAWSDir + "/config"
	env[envAWSSharedCredsFile] = sandboxAWSDir + "/credentials"
	env[envAWSProfile] = awsSSOProfileName
	env[awsSSOConfigEnvVar] = encodeArtifactConfig(map[string]string{".aws/config": awsSigV4ConfigFileContents(region),
		".aws/" + awsSigV4CredentialFile: awsSigV4CredentialFileContents()})
}
