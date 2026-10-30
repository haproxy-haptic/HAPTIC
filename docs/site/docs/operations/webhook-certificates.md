# Manage webhook certificates

The admission webhook uses its own certificate, separate from the certificates
your applications present to clients. Choose automatic renewal or supply a
certificate and CA bundle yourself. The commands below use release `haptic` in
namespace `haptic`.

## Default certificate

The admission webhook requires TLS certificates. By default the chart generates a self-signed certificate itself — no cert-manager required (`controller.webhook.certManager.enabled` is `false`):

```yaml
controller:
  webhook:
    enabled: true
    # certManager.enabled defaults to false → the chart issues a self-signed cert
```

The generated certificate lasts ten years and isn't renewed automatically.
Check its expiry with OpenSSL (for release `haptic` in namespace `haptic`):

```bash
kubectl get secret haptic-webhook-tls -n haptic \
  -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -enddate
```

Arrange renewal before it expires: an expired webhook certificate blocks changes
to resources covered by admission validation. Don't delete the serving Secret as
a rotation procedure; certificate replacement must preserve API-server trust.

If cert-manager is installed, select it for automatic renewal:

```yaml
controller:
  webhook:
    enabled: true
    certManager:
      enabled: true
      createIssuer: true  # Creates a self-signed Issuer automatically
```

The chart then creates:

- A self-signed `Issuer` and its `Certificate`, valid for one year by default
- Renewal 30 days before expiry
- CA-bundle injection into the webhook configuration

The controller reloads certificate updates without a restart. The default issuer
still uses a self-signed certificate; it doesn't issue a publicly trusted one.

To use an existing Issuer or ClusterIssuer instead:

```yaml
controller:
  webhook:
    certManager:
      enabled: true
      createIssuer: false
      issuerRef:
        name: my-existing-issuer
        kind: ClusterIssuer
```

## Supply your own webhook certificate

For a new release named `haptic` in namespace `haptic`, obtain a server certificate
with the DNS name `haptic-webhook.haptic.svc`. You need its PEM certificate chain
(`tls.crt`), private key (`tls.key`), and issuing CA bundle (`ca.crt`).

Create the namespace and serving Secret before installing the chart:

```bash
kubectl create namespace haptic --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret tls haptic-webhook-tls --namespace haptic \
  --cert=tls.crt --key=tls.key
base64 < ca.crt | tr -d '\n' > webhook-ca.base64
```

Install with the manual CA bundle and cert-manager disabled. Keep your other
settings in `haptic-values.yaml`:

```bash
helm install haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.9.0 --namespace haptic \
  --values haptic-values.yaml \
  --set controller.webhook.certManager.enabled=false \
  --set-file controller.webhook.caBundle=webhook-ca.base64
```

Keep this CA setting in your release configuration for subsequent upgrades.
For manual renewal under the same CA, update the serving Secret before expiry;
the controller reloads it automatically. When changing certificate authorities, first publish a
bundle trusting both old and new authorities, then replace the serving certificate. Remove
the old CA only after every controller replica serves the replacement certificate.
Switching an existing release from Helm or cert-manager certificate ownership
also requires transferring ownership of the Secret; this first-install procedure
doesn't perform that migration.
