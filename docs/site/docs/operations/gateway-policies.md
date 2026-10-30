# Protect Gateway routes

Attach a `HAProxyRoutePolicy` to an HTTPRoute or GRPCRoute rule to authenticate
requests, share a rate limit across the fleet, select a web application firewall (WAF) policy, or cache HTTP
responses. Install HAPTIC and the Gateway API CRDs first. This example assumes an
HTTPS Gateway named `public` and a Service named `api` on port 8080 in namespace
`apps`.

## Require an API key

1. Create the credential Secret.

    ```bash
    key=$(openssl rand -hex 32)
    printf '%s:example-client\n' "$key" |
      kubectl create secret generic api-keys-v1 -n apps \
        --from-file=keys=/dev/stdin --dry-run=client -o yaml |
      kubectl apply -f -
    ```

2. Make the credentials immutable.

    ```bash
    kubectl patch secret api-keys-v1 -n apps --type=merge -p '{"immutable":true}'
    ```

3. Create the policy.

    ```bash
    kubectl apply -f - <<'YAML'
    apiVersion: haproxy-haptic.org/v1alpha1
    kind: HAProxyRoutePolicy
    metadata:
      name: api-access
      namespace: apps
    spec:
      authentication:
        apiKey:
          secretRef:
            name: api-keys-v1
          consumerHeader: X-Authenticated-Consumer
    YAML
    ```

4. Attach the policy to a route rule.

    ```bash
    kubectl apply -f - <<'YAML'
    apiVersion: gateway.networking.k8s.io/v1
    kind: HTTPRoute
    metadata:
      name: api
      namespace: apps
    spec:
      parentRefs:
        - name: public
      hostnames:
        - api.example.com
      rules:
        - matches:
            - path:
                type: PathPrefix
                value: /
          filters:
            - type: ExtensionRef
              extensionRef:
                group: haproxy-haptic.org
                kind: HAProxyRoutePolicy
                name: api-access
          backendRefs:
            - name: api
              port: 8080
    YAML
    ```

5. Inspect the route conditions.

    ```bash
    kubectl get httproute api -n apps -o jsonpath='{.status.parents[*].conditions}'
    ```

    For the `public` parent, check that `Accepted` and `ResolvedRefs` are `True`.
    If either is `False`, read its message before testing requests.

6. Test authentication through the Gateway's HTTPS address.

    DNS for `api.example.com` must point to the Gateway, and its certificate must
    cover that hostname. In the same shell where you generated `key`, run:

    ```bash
    curl -i https://api.example.com/
    curl -i -H "X-API-Key: $key" https://api.example.com/
    ```

    The first request returns 401. The second reaches your `api` Service.

The policy and its credential Secret must share the route's namespace. Requests
without a valid `X-API-Key` receive 401. The authenticated request forwards
`X-Authenticated-Consumer: example-client`; a caller can't supply its own value.

## Rotate credentials

1. Create the replacement Secret.

    ```bash
    key=$(openssl rand -hex 32)
    printf '%s:example-client\n' "$key" |
      kubectl create secret generic api-keys-v2 -n apps --from-file=keys=/dev/stdin
    ```

2. Make the replacement immutable.

    ```bash
    kubectl patch secret api-keys-v2 -n apps --type=merge -p '{"immutable":true}'
    ```

3. Change the policy reference.

    ```bash
    kubectl patch haproxyroutepolicy api-access -n apps --type=merge \
      -p '{"spec":{"authentication":{"apiKey":{"secretRef":{"name":"api-keys-v2"}}}}}'
    ```

For an attached policy, admission validates the replacement before accepting the
reference change. Keep the previous Secret until every policy has moved to the
replacement. For an overlap window, include both old and new API keys in the
replacement, then publish another version containing only the new keys after
clients have switched.

## Add a shared rate limit

1. Enable the shared rate-limit infrastructure on the existing release.

    ```bash
    helm upgrade haptic \
      oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
      --version 0.9.0 \
      --namespace haptic --reuse-values \
      --set rateLimit.shared.enabled=true \
      --set rateLimit.shared.managedStore.enabled=true
    ```

2. Add a consumer budget to the policy.

    ```bash
    kubectl patch haproxyroutepolicy api-access -n apps --type=merge \
      -p '{"spec":{"rateLimit":{"requests":100,"period":"1s","burst":100,"algorithm":"gcra","key":"consumer"}}}'
    ```

Every rule that references `apps/api-access` shares this budget across HAProxy
replicas. Each authenticated consumer has its own budget. Requests without a
consumer identity fail authentication before quota enforcement.

## Add WAF inspection

1. Configure a WAF catalog in your release's values file.

    ```yaml
    spoaHub:
      plugins:
        coraza:
          enabled: true
    controller:
      config:
        templatingSettings:
          extraContext:
            waf:
              policies:
                inline:
                  api-headers:
                    enforcement: deny
                    requestBody:
                      mode: none
    ```

    Apply these settings through your existing Helm or GitOps release workflow.
    See [WAF policies](./waf-policies.md) for catalog sources and body inspection.

2. Select the catalog entry.

    ```bash
    kubectl patch haproxyroutepolicy api-access -n apps --type=merge \
      -p '{"spec":{"waf":{"policy":"api-headers"}}}'
    ```

GRPCRoute supports header and request metadata inspection with `mode: none`.
Policies that buffer request bodies are rejected for GRPCRoute because a stream
may never complete its body.

## Rotate a namespace WAF catalog

Enable `controller.config.templatingSettings.extraContext.waf.policies.selfService.enabled`
in your release values and apply the change through your existing release workflow.
The following example uses the `api-access` policy created above.

1. Create an immutable catalog version.

    ```bash
    kubectl apply -f - <<'YAML'
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: api-waf-v1
      namespace: apps
    immutable: true
    data:
      policies.yaml: |
        api-headers:
          enforcement: deny
          requestBody:
            mode: none
          allowedMethods: [GET, HEAD]
    YAML
    ```

2. Select the new catalog.

    ```bash
    kubectl patch haproxyroutepolicy api-access -n apps --type=merge \
      -p '{"spec":{"waf":{"policy":"api-headers","catalogRef":{"name":"api-waf-v1"}}}}'
    ```

3. Create the next version with its new policy settings.

    ```bash
    kubectl apply -f - <<'YAML'
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: api-waf-v2
      namespace: apps
    immutable: true
    data:
      policies.yaml: |
        api-headers:
          enforcement: deny
          requestBody:
            mode: none
          allowedMethods: [GET, HEAD, POST]
    YAML
    ```

4. Update the reference.

    ```bash
    kubectl patch haproxyroutepolicy api-access -n apps --type=merge \
      -p '{"spec":{"waf":{"catalogRef":{"name":"api-waf-v2"}}}}'
    ```

Admission validates the new selection against every attached route. Rejected
changes leave the old reference in place. Catalog creation alone doesn't change
traffic. Keep the previous version while any policy references it; deleting a
referenced catalog makes those rules return 503. Roll back by changing the
reference to `api-waf-v1`.

Kubernetes rejects edits to immutable catalog data. Create a new catalog and
change the policy reference for each update.

## Recover an invalid policy

Inspect the route conditions and Events, then repair the named policy or
dependency. A missing policy, malformed credential, or unavailable enforcement
component denies requests to the affected rule with 503. Valid sibling rules
continue serving; `PartiallyInvalid` identifies a route with both valid and
invalid rules. Admission rejects proposed changes that would introduce these
failures.

The [policy reference](../route-policy.md) lists all settings, including JWT
verification and private caching.
