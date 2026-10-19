# Backend mTLS with SPIFFE/SPIRE

Use [SPIFFE/SPIRE](https://spiffe.io/) to give HAProxy automatic mutual TLS (mTLS) to backend services: SPIRE issues and rotates short-lived X.509 certificates, and HAPTIC wires them into the backend configuration.

<a id="overview"></a>

## How certificates reach HAProxy

SPIRE issues and rotates workload certificates. In this example, `spiffe-helper`
writes HAProxy's identity and trust bundle into a shared pod volume. A second
sidecar updates the certificates loaded by HAProxy when those files change.
The credentials don't pass through Kubernetes Secrets.

## Prerequisites

Before following this guide, ensure:

- **SPIRE server and agents** are deployed in your cluster
- **SPIRE Container Storage Interface (CSI) driver** (`csi.spiffe.io`) is installed for exposing the Workload API socket to pods
- **Workload registration** exists for the HAProxy pod's service account and namespace
- A Community Edition HAPTIC installation you can update through [Helm values](../deploying-with-helm.md). The example uses UID `99`, matching its HAProxy image.
- Backend applications that require a client certificate and trust your SPIRE issuer. Their certificates must include the Service DNS name; see [DNS SAN configuration](#dns-san-configuration).
- Bash, `kubectl`, and OpenSSL for the validation fixtures and checks below.

## Configure the integration {#configuration}

<a id="haproxy-pod-setup"></a>
<a id="spiffe-helper-configuration"></a>
<a id="backend-mtls-via-custom-annotation"></a>

Download the [complete example values](../examples/spiffe-values.yaml) as
`spiffe-values.yaml`. The file combines:

- `spiffe-helper` and `cert-reloader` sidecars, with their shared volumes.
- The helper's configuration in a ConfigMap.
- A template snippet for the `example.com/server-mtls-spire` annotation.
- Controller-only mounts for the validation fixtures created below.

Keep these settings alongside your normal `haptic-values.yaml`. If you already
configure sidecars, init containers, volumes, mounts, or `extraDeploy`, combine
those list entries in the example file before applying it; Helm replaces lists.

The snippet requires verified backend TLS and sends `<service>.<namespace>.svc`
as the TLS server name. It rejects combinations with `haproxy.org/server-ssl`,
`haproxy.org/server-crt`, or `haproxy.org/server-ca`, which configure the same
connection through a different certificate source.

Continue with the backend DNS names and controller validation fixtures before
applying these values.

### DNS SAN configuration

The `sni str(...)` directive in the snippet above requires that backend SVIDs include DNS SANs matching the Kubernetes service name. Enable [`autoPopulateDNSNames`](https://github.com/spiffe/spire-controller-manager/blob/main/docs/clusterspiffeid-crd.md) on the default ClusterSPIFFEID so that SPIRE automatically adds service DNS names (for example `my-backend`, `my-backend.default.svc`, `my-backend.default.svc.cluster.local`) as DNS SANs in all SVIDs:

```yaml
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: spire-default
spec:
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
  autoPopulateDNSNames: true
```

If you use the [SPIRE Helm chart](https://artifacthub.io/packages/helm/spiffe/spire), set this via values:

```yaml
spire-server:
  controllerManager:
    identities:
      clusterSPIFFEIDs:
        default:
          autoPopulateDNSNames: true
```

`autoPopulateDNSNames` uses the Services each pod belongs to. Certificates
update without restarting HAProxy, so short lifetimes such as `1h` are supported.

## Controller validation

The HAPTIC controller checks rendered configuration with its local `haproxy -c` binary. Since the SPIRE certificates only exist on the HAProxy pods (managed by spiffe-helper), the controller pod needs placeholder files at the same absolute paths so that validation passes.

The example values mount a `spiffe-validation-certs` ConfigMap on the controller
at the same paths used by HAProxy.

Create the validation ConfigMap before applying those Helm values. These files
are validation fixtures, never credentials for backend connections; mount them
only on the controller. The HAProxy pods continue to obtain real credentials
from SPIRE.

```bash
HAPTIC_VALIDATION_DIR=$(mktemp -d)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout "$HAPTIC_VALIDATION_DIR/tls.key" \
  -out "$HAPTIC_VALIDATION_DIR/tls.crt" -days 3650 -nodes \
  -subj '/CN=validation-placeholder'
kubectl create configmap spiffe-validation-certs --namespace haptic \
  --from-file=svid.pem="$HAPTIC_VALIDATION_DIR/tls.crt" \
  --from-file=svid.pem.key="$HAPTIC_VALIDATION_DIR/tls.key" \
  --from-file=bundle.pem="$HAPTIC_VALIDATION_DIR/tls.crt" \
  --dry-run=client -o yaml | kubectl apply -f -
rm -r "$HAPTIC_VALIDATION_DIR"
```

## Apply the values

For release `haptic` in namespace `haptic`, apply both values files:

```bash
helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.7.0 --namespace haptic \
  --values haptic-values.yaml --values spiffe-values.yaml
kubectl rollout status deployment/haptic-controller --namespace haptic
kubectl rollout status deployment/haptic-haproxy --namespace haptic
```

Keep pre-rollout and admission validation enabled. The helper must obtain its
first identity before the HAProxy pod becomes ready.

## Enable backend mTLS for an Ingress

This example assumes an existing Ingress `my-backend` in namespace `default`,
pointing to a backend that serves TLS. Add the annotation:

```bash
kubectl annotate ingress my-backend --namespace default \
  example.com/server-mtls-spire=true --overwrite
```

For a Service named `my-backend`, HAProxy verifies the server certificate against
`my-backend.default.svc` and presents its SPIRE client identity. Until an annotated
route loads these certificates, the certificate reload sidecar has no certificate store to update
and may log retries.

## Verification

After deploying, verify the integration is working:

```bash
# Check spiffe-helper received certificates
kubectl -n haptic logs deployment/haptic-haproxy -c spiffe-helper

# Expected output:
# level=info msg="Received update" spiffe_id="spiffe://..." system=spiffe-helper
# level=info msg="X.509 certificates updated" system=spiffe-helper
```

```bash
# Verify certificate files exist on the HAProxy pod
kubectl -n haptic exec deployment/haptic-haproxy -c haproxy -- ls -la /etc/haproxy/spiffe/

# Expected: svid.pem, svid.pem.key, bundle.pem owned by UID 99
```

```bash
# Inspect the SPIFFE ID in the issued certificate
kubectl -n haptic exec deployment/haptic-haproxy -c haproxy -- \
  openssl x509 -in /etc/haproxy/spiffe/svid.pem -noout -text \
  | grep -A1 "Subject Alternative Name"

# Expected: URI:spiffe://<trust-domain>/ns/<namespace>/sa/<service-account>
```

```bash
# Verify the backend mTLS annotation is reflected in HAProxy config
kubectl -n haptic exec deployment/haptic-haproxy -c haproxy -- \
  cat /etc/haproxy/haproxy.cfg | grep -A2 'default-server.*ssl.*verify'
```

```bash
# Check cert-reloader is running and updating certificates
kubectl -n haptic logs deployment/haptic-haproxy -c cert-reloader

# Expected output after a rotation:
# cert-reloader: polling for cert changes
# cert-reloader: certificates updated via runtime API
```

## Troubleshooting

<a id="spiffe-helper-can't-connect-to-spire-agent"></a>

<a id="spiffe-helper-cannot-connect-to-spire-agent"></a>

### `spiffe-helper` can't connect to SPIRE agent

```
Error while watching x509 context: ... dial unix /spiffe-workload-api/agent.sock: no such file or directory
```

The SPIRE CSI driver creates the socket as `spire-agent.sock`, not `agent.sock`. Verify the correct socket name:

```bash
kubectl -n haptic exec deployment/haptic-haproxy -c spiffe-helper -- ls /spiffe-workload-api/
```

Update `agent_address` in your spiffe-helper config to match.

### `spiffe-helper` config parse error

```
failed to parse configuration ... got: LBRACK
```

spiffe-helper uses HashiCorp Configuration Language (HCL) syntax, not TOML. Replace `[section]` with `section { ... }`:

```hcl
# Wrong (TOML)
[health_checks]
listener_enabled = true

# Correct (HCL)
health_checks {
  listener_enabled = true
}
```

<a id="certificate-directory-does-not-exist"></a>

### Certificate directory doesn't exist

```
Unable to dump bundle ... open /etc/haproxy/spiffe/svid.pem: no such file or directory
```

The `haproxy-runtime` emptyDir doesn't include the `spiffe/` subdirectory by default. Ensure the init container is configured to create it before spiffe-helper starts. The example values file already sets `resources.requests` and `resources.limits`; keep them in place if you customized it, so the init container isn't rejected by a namespace ResourceQuota.

### `ImagePullBackOff` for `spiffe-helper`

```
Back-off pulling image "ghcr.io/spiffe/spiffe-helper:v0.11.0"
```

The spiffe-helper container image uses tags **without** the `v` prefix. Use `0.11.0`, not `v0.11.0`.

### Controller rejects config with cert path errors

If the controller logs show validation failures referencing `/etc/haproxy/spiffe/*.pem`, the validation placeholder ConfigMap isn't mounted on the controller pod. Verify:

```bash
kubectl -n haptic exec deployment/haptic-controller -c controller -- ls /etc/haproxy/spiffe/
# Should list: bundle.pem  svid.pem  svid.pem.key
```

## See also

- [Security Guide](./security.md) — TLS configuration and credential management
- [Chart Values Reference](../reference.md) — `haproxy.sidecars`, `haproxy.initContainers`, `extraDeploy`
- [SPIFFE/SPIRE Documentation](https://spiffe.io/docs/latest/) — SPIFFE concepts, SPIRE deployment, workload registration
- [spiffe-helper on GitHub](https://github.com/spiffe/spiffe-helper) — Configuration reference and release notes
- [Templating Guide](../templating.md) — Writing custom `templateSnippets`
