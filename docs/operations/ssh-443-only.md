> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# SSH on a 443-only estate

- Some estates let nothing in but port 443, terminated by a TLS-terminating listener in front of the cluster (an Istio ingress gateway, say).
- The SSH gateway's listener is plain TCP, so `ssh` cannot reach it there as it is.

## The recipe

- The
  recipe wraps SSH in TLS on each person's computer and lets the listener
  unwrap it:

1. The listener terminates TLS on 443 under a hostname of its own and passes
   the bytes, as plain TCP, to the Service's `ssh` port.
2. `ssh.proxyCommand` (`WARDYN_SSH_PROXY_COMMAND`) tells each person's `ssh`
   to open that TLS connection first.
   - The run-detail card and
     `wardyn run ssh` carry it, so the copy-paste command works.

- In your own values file (Istio installed from its own charts; the Wardyn
  chart ships no Istio template, and `extraObjects` keeps these objects beside
  your values instead of in a fork of the chart):

```yaml
ssh:
  enabled: true
  advertiseHost: ssh.example.com   # the listener's own hostname for SSH
  proxyCommand: openssl s_client -quiet -verify_return_error -verify_hostname %h -connect %h:443 -servername %h

networkPolicy:
  ingress:
    from:
      - podSelector: {}            # keep the same-namespace default peer
      - namespaceSelector:         # and admit the ingress gateway's namespace
          matchLabels:
            kubernetes.io/metadata.name: istio-ingress

extraObjects:
  - apiVersion: networking.istio.io/v1
    kind: Gateway
    metadata:
      name: wardyn-ssh
      namespace: istio-ingress     # the gateway pods' namespace
    spec:
      selector:
        istio: ingressgateway      # your ingress gateway pods' labels
      servers:
        - port:
            number: 443
            name: tls-wardyn-ssh
            protocol: TLS          # never HTTPS
          tls:
            mode: SIMPLE
            credentialName: wardyn-ssh-tls
          hosts:
            - ssh.example.com
  - apiVersion: networking.istio.io/v1
    kind: VirtualService
    metadata:
      name: wardyn-ssh
    spec:
      hosts:
        - ssh.example.com
      gateways:
        - istio-ingress/wardyn-ssh
      tcp:
        - match:
            - port: 443
          route:
            - destination:
                host: '{{ include "wardyn.fullname" . }}.{{ .Release.Namespace }}.svc.cluster.local'
                port:
                  # Keep ssh.port above 1023: the gateway listens on 443 and
                  # forwards to it. wardynd runs non-root with every capability
                  # dropped, and on a runtime that does not allow unprivileged
                  # low ports (the default before containerd 2.0) a lower port
                  # rolls out Ready with no SSH listener behind it.
                  number: 2222     # ssh.port
```

## Notes on the values

- **`protocol: TLS`, not `HTTPS`.** `HTTPS` attaches an HTTP filter to the
  server and breaks SSH; `TLS` with `tls.mode: SIMPLE` terminates TLS and
  hands the `tcp:` route the raw stream.
- **The certificate.** `credentialName` names a `kubernetes.io/tls` Secret for
  the SSH hostname.
  - The Secret lives in the namespace of the gateway pods,
    which is where the recipe also puts the Gateway.
- **The Gateway's namespace.** `istio-ingress` here is the estate's own
  gateway namespace: the Gateway sits beside the gateway pods, and the
  VirtualService names it as `<namespace>/<name>`.
  - Your Helm identity needs
    rights to create Istio Gateways in that namespace, and the namespace must
    already exist.
- **The NetworkPolicy.** Setting `networkPolicy.ingress.from` replaces the
  same-namespace default, so list it again; the ssh rule passes the other
  named peers through.
  - The same peer list also applies to the console `http`
    rule, so the gateway namespace can reach the console port too; a
    `podSelector` for the gateway pods on that peer narrows it.
- **The proxy command.** `-servername %h` only sends SNI, which the listener
  routes on; `-verify_hostname %h` is what checks that the certificate names
  the host, and `-verify_return_error` makes a failed check end the
  connection.
  - Port 443 is written out because `%p` expands to the advertised
    SSH port (the chart always renders `host:ssh.port`; the `-p <port>` on the
    card is harmless, the proxy command ignores it).
  - When the listener's
    certificate is not from a publicly trusted CA, add `-CAfile <path>` naming
    a file every person keeps the CA at, e.g. `-CAfile ~/.ssh/wardyn-ca.pem`
    (ssh runs the command through the person's shell, so `~` expands).
  - Each
    person needs `openssl` on their computer.
  - The daemon never runs this value
    and refuses to boot on a control character, a newline, a single quote, or
    more than 512 bytes ([ENV.md](../ENV.md)).
- **Host keys are unaffected.** The SSH handshake runs end to end inside the
  tunnel, so the fingerprint on the card is still the one to verify.
- **Audit source IPs become the listener's.** The gateway sees the ingress
  gateway pod's address, so `ssh.*` audit rows record that, not the person's.
- **Nothing else needs it.** The browser terminal and `wardyn attach`
  already ride the console's own 443.
