# Generate configuration and files

Use these examples when writing your own configuration or auxiliary-file
templates. To add one setting to the bundled configuration, start with
[Write your first template](templating.md) instead.

The examples run in your browser. They omit installation-specific credentials
and pod selectors, so use them as template examples rather than applying their
`HAProxyTemplateConfig` objects directly to a cluster. With Helm, put template
settings under `controller.config`; see the [configuration reference](crd-reference.md)
for a complete standalone object.

## What you can template

| Template Type | Use When |
|---------------|----------|
| `haproxyConfig` | Main HAProxy configuration (frontends, backends, global settings) |
| `maps` | HAProxy lookup tables for host/path routing decisions |
| `files` | Auxiliary files like custom error pages |
| `sslCertificates` | TLS certificate files assembled from Kubernetes Secrets |

### HAProxy Configuration

The main `haproxyConfig` template generates the complete HAProxy configuration file. This example loops over the sample Ingresses and emits a backend for each. Open **Resources** to add or edit an Ingress, then check the output. The backends have no servers yet; this example teaches template rendering, not a working route.

<div class="pg-embed" markdown data-tab="haproxy.cfg" data-controls="tabs,resources" data-title="One backend per Ingress" data-height="480">

```yaml
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateConfig
metadata:
  name: haproxy-config-demo
spec:
  watchedResources:
    ingresses:
      apiVersion: networking.k8s.io/v1
      resources: ingresses
      indexBy:
        - metadata.name
  maps:
    host.map:
      template: |
        {%- for _, ingress := range resources.ingresses.List() %}
        {%- for _, rule := range ingress.spec.rules %}
        {{ rule.host }} {{ ingress.metadata.name }}
        {%- end %}
        {%- end %}
  haproxyConfig:
    template: |
      global
        log stdout len 4096 local0 info
        daemon
        maxconn 4096
      defaults
        mode http
        timeout connect 5s
        timeout client 50s
        timeout server 50s
      frontend http
        bind *:80
        use_backend %[req.hdr(host),lower,map({{ pathResolver.GetPath("host.map", "map") }})]
      {%- for _, ingress := range resources.ingresses.List() %}
      backend {{ ingress.metadata.name }}
        balance roundrobin
      {%- end %}
```

```yaml
apiVersion: v1
kind: List
items:
  - apiVersion: networking.k8s.io/v1
    kind: Ingress
    metadata:
      name: shop
    spec:
      rules:
        - host: shop.example.com
          http:
            paths:
              - path: /
                pathType: Prefix
                backend:
                  service:
                    name: shop
                    port:
                      number: 80
  - apiVersion: networking.k8s.io/v1
    kind: Ingress
    metadata:
      name: blog
    spec:
      rules:
        - host: blog.example.com
          http:
            paths:
              - path: /
                pathType: Prefix
                backend:
                  service:
                    name: blog
                    port:
                      number: 80
```

</div>

!!! important
    Whenever your HAProxy config references a map file, error file, certificate, or crt-list, use `pathResolver.GetPath(filename, type)` instead of a hard-coded path. The controller deploys these files to a configurable directory (set in `spec.dataplane.mapsDir`, `sslCertsDir`, `generalStorageDir`) and `pathResolver` knows where they live, so the path stays correct even if you reconfigure those directories.

HAProxy's `http-response set-header` directive adds a response header. For
example, `http-response set-header X-Team storefront` adds `X-Team: storefront`.
Try it in the `frontend web` section below.

<div class="pg-embed" markdown data-tab="haproxy.cfg" data-focus="11" data-title="Your turn: add a response header" data-difficulty="1">

<p class="pg-task" markdown>Add a line to `frontend web` that sets the response header `X-Example` to `hello`. Find the new directive in the output.</p>

```yaml
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateConfig
metadata:
  name: hsts-demo
spec:
  haproxyConfig:
    template: |
      global
        log stdout format raw local0
        daemon
      defaults
        mode http
        timeout connect 5s
        timeout client 30s
        timeout server 30s
      frontend web
        bind *:80
        # TODO(you): add a line so every response carries X-Example: hello
        default_backend app
      backend app
        server s1 127.0.0.1:8080 check
```

<details class="pg-solution" markdown>
<summary>Peek at the solution</summary>

```yaml
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateConfig
metadata:
  name: hsts-demo
spec:
  haproxyConfig:
    template: |
      global
        log stdout format raw local0
        daemon
      defaults
        mode http
        timeout connect 5s
        timeout client 30s
        timeout server 30s
      frontend web
        bind *:80
        http-response set-header X-Example hello
        default_backend app
      backend app
        server s1 127.0.0.1:8080 check
```

</details>
</div>

!!! note "Named and multiple `defaults` sections"
    Templates can emit multiple named `defaults` sections. A `frontend`, `backend`, or `listen` section selects one with `from <name>`. The bundled base library uses named profiles, including `haptic-base`; see [reload-free routing](libraries/reload-free.md) before changing profiles used by dynamic backends.

### Map files

Each `maps` entry renders one HAProxy lookup table. They're written to `spec.dataplane.mapsDir` (default `/etc/haproxy/maps/`) on the HAProxy pod. This template turns each Ingress host into a backend-name entry — switch to the **maps** tab to read the generated `host.map`.

<div class="pg-embed" markdown data-tab="maps" data-controls="tabs,resources" data-title="A host → backend map" data-height="440">

```yaml
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateConfig
metadata:
  name: map-demo
spec:
  watchedResources:
    ingresses:
      apiVersion: networking.k8s.io/v1
      resources: ingresses
      indexBy:
        - metadata.name
  maps:
    host.map:
      template: |
        {%- for _, ingress := range resources.ingresses.List() %}
        {%- for _, rule := range ingress.spec.rules %}
        {%- if len(rule.http.paths) > 0 %}
        {{ rule.host }} ing_{{ ingress.metadata.name }}
        {%- end %}
        {%- end %}
        {%- end %}
  haproxyConfig:
    template: |
      global
        log stdout format raw local0
        daemon
      defaults
        mode http
        timeout connect 5s
        timeout client 30s
        timeout server 30s
      frontend http
        bind *:80
        use_backend %[req.hdr(host),lower,map({{ pathResolver.GetPath("host.map", "map") }})]
```

```yaml
apiVersion: v1
kind: List
items:
  - apiVersion: networking.k8s.io/v1
    kind: Ingress
    metadata:
      name: shop
    spec:
      rules:
        - host: shop.example.com
          http:
            paths:
              - path: /
                pathType: Prefix
                backend:
                  service:
                    name: shop
                    port:
                      number: 80
  - apiVersion: networking.k8s.io/v1
    kind: Ingress
    metadata:
      name: blog
    spec:
      rules:
        - host: blog.example.com
          http:
            paths:
              - path: /
                pathType: Prefix
                backend:
                  service:
                    name: blog
                    port:
                      number: 80
```

</div>

### General files

Auxiliary files like custom error pages. Written to `spec.dataplane.generalStorageDir` (default `/etc/haproxy/general/`). The `errorfile` directive points HAProxy at the rendered file — open the **files** tab to see `503.http`.

<div class="pg-embed" markdown data-tab="files" data-controls="tabs" data-title="A custom 503 error page" data-height="440">

```yaml
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateConfig
metadata:
  name: files-demo
spec:
  files:
    503.http:
      template: |
        HTTP/1.0 503 Service Unavailable
        Cache-Control: no-cache
        Connection: close
        Content-Type: text/html

        <html><body><h1>503 Service Unavailable</h1></body></html>
  haproxyConfig:
    template: |
      global
        log stdout format raw local0
        daemon
      defaults
        mode http
        timeout connect 5s
        timeout client 30s
        timeout server 30s
      frontend http
        bind *:80
        errorfile 503 {{ pathResolver.GetPath("503.http", "file") }}
        default_backend web
      backend web
        server s1 10.0.0.1:8080 check
```

</div>

General-file changes trigger a reload by default. For a file consumed only by a
sidecar that reloads its own configuration, set `reloadOnPush: false` to update
the file without reloading HAProxy:

```yaml
spec:
  files:
    vector.yaml:
      reloadOnPush: false
      template: |
        sources: {}
```

Registering the file at render time takes the same flag as a fourth argument:

```scriggo
{%- var _, err = fileRegistry.Register("file", "spoa-hub-config.toml", content, false) %}
{% if err != nil %}{{ fail(err.Error()) }}{% end %}
```

### SSL certificates

SSL/TLS certificate files are assembled from Kubernetes Secrets. Written to `spec.dataplane.sslCertsDir` (default `/etc/haproxy/ssl/`). This reads a TLS Secret and concatenates its certificate and key into one PEM — the **certs** tab shows the result.

<div class="pg-embed" markdown data-tab="certs" data-controls="tabs,resources" data-title="A PEM assembled from a Secret" data-height="440">

```yaml
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateConfig
metadata:
  name: cert-demo
spec:
  watchedResources:
    secrets:
      apiVersion: v1
      resources: secrets
      indexBy:
        - metadata.namespace
        - metadata.name
  sslCertificates:
    example-com.pem:
      template: |
        {%- var secret = resources.secrets.GetSingle("default", "example-com-tls") %}
        {%- if secret != nil %}
        {{ secret.data["tls.crt"] | b64decode() }}
        {{ secret.data["tls.key"] | b64decode() }}
        {%- end %}
  haproxyConfig:
    template: |
      global
        log stdout format raw local0
        daemon
      defaults
        mode http
        timeout connect 5s
        timeout client 30s
        timeout server 30s
      frontend web
        bind *:443 ssl crt {{ pathResolver.GetPath("example-com.pem", "cert") }}
        default_backend app
      backend app
        server s1 10.0.0.1:8080 check
```

```yaml
apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: Secret
    type: kubernetes.io/tls
    metadata:
      name: example-com-tls
      namespace: default
    data:
      tls.crt: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSURUekNDQWplZ0F3SUJBZ0lVV3ZyRGg3bVB5ck5rclB2N1FjeWQ1cXBZVFZFd0RRWUpLb1pJaHZjTkFRRUwKQlFBd056RVVNQklHQTFVRUF3d0xaWGhoYlhCc1pTNWpiMjB4SHpBZEJnTlZCQW9NRmtoQlVGUkpReUJRYkdGNQpaM0p2ZFc1a0lFUmxiVzh3SGhjTk1qWXdOekE1TWpNd056QTJXaGNOTXpZd056QTJNak13TnpBMldqQTNNUlF3CkVnWURWUVFEREF0bGVHRnRjR3hsTG1OdmJURWZNQjBHQTFVRUNnd1dTRUZRVkVsRElGQnNZWGxuY205MWJtUWcKUkdWdGJ6Q0NBU0l3RFFZSktvWklodmNOQVFFQkJRQURnZ0VQQURDQ0FRb0NnZ0VCQUxtYXBnQlZTNERmQ29jcApNUk1ocnIxeG42M1RCL3plL2kxT3hQV1k5eUhmc0hOelZPakRUT054elE1SERVMVFBUFZXb2I0YmlKemZWbDF6Cm5qVCs4MkordXVUZWVCbWUxcFJhRUhyNjgvbWxCelAvM3V0NDBDNlJ1Y0xSbzVWYlVvd3d2WnpOVHJGbW1Jdk4KcDdXdVNsWDFhTFBSSENvRE0zYUtndU94MS9MdHl6TGw3eGtPdkRBa0ZoYmNWc0tVSUFzb01KaWliREYrdzBYZApXenJDUmZOSDdzMjNldTBDRDBnZk1lT0lTV3R5MU40SWRUT2NBcGU4aWpMNi80SkJYOG51NmFhOXMwd3JmMXhpCm9yeEhEV2dDMFpva21EMGlvZ0NYaWptNXFJUGZySnZ5NkMyNzgrRnErK2I3ZzR0dzlFdjlmS1YyeGJYUjdNVTQKTTNRaUZpVUNBd0VBQWFOVE1GRXdIUVlEVlIwT0JCWUVGSUQzOG51WmszaklHQVRVZWMzV3pwMi9tNmpxTUI4RwpBMVVkSXdRWU1CYUFGSUQzOG51WmszaklHQVRVZWMzV3pwMi9tNmpxTUE4R0ExVWRFd0VCL3dRRk1BTUJBZjh3CkRRWUpLb1pJaHZjTkFRRUxCUUFEZ2dFQkFHQmFYa1JhcTRReEoxTDl2WHdnemlyWjR1dzltRzBWL1gzVkNtUDUKVXhicnJrQ3JiZzZEYURYRWpUTEk5bm92VVFmK2NaMWhPRDI0TDN4d1dvUHZ2Z25BNlBlR240c2F1Q0Z0WFNrSwp5RzZOemFrWmdjdHY0OHUzQnNLUDRJenZmTVRhZENNWmlyb2xMV0MrWWlDc1doSVRSR1RSd3JnVXlwN3JiTVgzCk9uNXpEYlU3MjU4RXhiN01NYlBvMlpJRWZZcUErKzIzVlZ6alBQamR4Yy81NjhLZTFPZUhKenR3SG5ENmk3WVAKM3NaTyt0dC83OU5TQlBUNk5TcUg2eWdGWUpCMWpYOWhYKzA1VHJzb010UnVUMmFsU1duY2VVOHJRd2dYalFLVQpiZnUrVE4xdnBrVjk0ZFZERnVKRFhhWFIyQ0ptUmVTM1prWDlJYWxNc1cvTHpwWT0KLS0tLS1FTkQgQ0VSVElGSUNBVEUtLS0tLQo=
      tls.key: LS0tLS1CRUdJTiBQUklWQVRFIEtFWS0tLS0tCk1JSUV2UUlCQURBTkJna3Foa2lHOXcwQkFRRUZBQVNDQktjd2dnU2pBZ0VBQW9JQkFRQzVtcVlBVlV1QTN3cUgKS1RFVElhNjljWit0MHdmODN2NHRUc1QxbVBjaDM3QnpjMVRvdzB6amNjME9SdzFOVUFEMVZxRytHNGljMzFaZApjNTQwL3ZOaWZycmszbmdabnRhVVdoQjYrdlA1cFFjei85N3JlTkF1a2JuQzBhT1ZXMUtNTUwyY3pVNnhacGlMCnphZTFya3BWOVdpejBSd3FBek4yaW9ManNkZnk3Y3N5NWU4WkRyd3dKQllXM0ZiQ2xDQUxLRENZb213eGZzTkYKM1ZzNndrWHpSKzdOdDNydEFnOUlIekhqaUVscmN0VGVDSFV6bkFLWHZJb3krditDUVYvSjd1bW12Yk5NSzM5YwpZcUs4Uncxb0F0R2FKSmc5SXFJQWw0bzV1YWlEMzZ5Yjh1Z3R1L1BoYXZ2bSs0T0xjUFJML1h5bGRzVzEwZXpGCk9ETjBJaFlsQWdNQkFBRUNnZ0VBRW4zcmN4WU1ienNKbi96RkpHeFRMaEcvZ0lDSmg3S3A3VmF2UGU3dkZHTm0KZjZJcWdBUlJTVW5oemIzYmYrdnNKSVZzbVBYQ1R5cmJQblZSK21LNldnSlpXWXNtdVJxL3Mwa2o0alRWa1BaVgp1T01SMFRFWXdNTUpHSFZ0a0dob1dZcFRvZWM4bzJVZTVyTG5OaTAydjhpekZWTk10SXpjR0QvbG1ZenpBSU53CkV0UFJRRHdsMks1NDFFckdZTjA1c2RyQmFWNkFFdjRFWHh4cldzVXJCK3k2cW1XQ1kvUDdSUHkwNzFCVHJnTmUKSkhYUnk5NnJOSE9DUHZYK1kzQWRYSGw4T01yMTV0M3IyMVVlMmpqVlltY29UT1pSTTVMSjN2emRRSEFESFV4ZQoyZUFORXJkWGNNdVgyUi9wK0IvNnBtUE1LVTJLT2JJeWlOK1p0Zm9ya3dLQmdRRHFLZ083Z1BqY0RIVGc4bEdaCk14Z282emErL1VaOUN2K2JMTzk2RzBzWlpkUEJpYjR0cStvMXRnSXlqWjZ5SHBzbTBpanRSZHhjZEtuQXlIcUcKNmRwU3pJbXlUQU9DV3JsbkFFY05XQitIeTR1cTVuMUY3M0VrSitiYi9saDRUbm94SmFSeEIweDM3QjJlRVhBcQppUkhjeGdyKzljOTU3ajVuSk5RWnJ2eE1id0tCZ1FESzZXZW9jcEdSeFoxM1ZUYUVrWERFL3ZQaVBpWVJBWEZjCmVQUmVrNnhZbVAxdmxDVUdpK2VPNGgyTW9ycEoxWVBlbDBzcHNDTCs2bk5ZV0Z2K3cyUjlsb0RqY1BOSnY0WGQKdkdGeFRzS0Zkdlp0ZkxodVpqeXljM01FeWRpckt3dmpuK2lieHo2NWZOdWtWcjFhSlExQnUvN2wycmJTSEsxbwpzSERiOENsNHF3S0JnQVhMb0dnRm15TW5FOFYxZWR1R3pqUkZEZ2ZRRU95TFZ5UXFDb3RGSGFpMVFuWnB5RkV0CkRoRGlQayt0L1oxKzhHd1hpM2ZENE41UTdOcWVtNW0zTS9ZVXBkdkowZFJxRm1pY015WDdabHhnQjBibGlYZ3YKb3VjNExaaUlSUHhGUlBUdWI1RjBrc250Q0JhZmE5MUJveldKbVVBU0tWNWxMUm8wYVNOeGwwRDFBb0dCQU1hVgpWV0J5OStwdE42WFJYTEN6VW1WSmkwL1JPUm9OaW05UTVRQW1rRmFKTEFkbU9qSkUrOU5Ia2xuUDdIZFVJbUhYCk9iVkw3NFFCMmU4TlVzTnJZTTdVVzhHOENpNFQ1YVJUdUIzWFVlS2l3WnYzb3R4UTdIaE5LclQyQWpuS3dERCsKai96ZEs1TUhFa0tzclZZcXl1V1pZbVo3L2M1MlNIUWJzZWhlQzRoUEFvR0FDNW9zY2NqQlpiK2xMOW9lMnp1WgpZQ0pDMjNzQnB2bnc2cmFBdXMzZXBFdDVXQnBxL0t0cmhEVjBvL1FaVU1JUEtOM3d3dUxyd01pM0VsMHNLand2CmtHNGxhRThhU1BGek16TjBVdTRXbEhCY01xT2N3UVpVUzIwM2o4eTl3SjVtdVllNU9FMzRUdndOQ3dtVFZXNkcKK3RkNElYaHgvMGpEbXZaSzNjRDd5V3M9Ci0tLS0tRU5EIFBSSVZBVEUgS0VZLS0tLS0K
```

</div>

Use `b64decode` to decode certificate data from Secrets.

### Template snippets

Reusable template fragments are included via `{{ render "snippet-name" }}` — or `{{ render_glob "pattern" }}` to pull in every match at once. This config keeps each backend in its own snippet and stitches them into the config with `render_glob`, which renders matches in alphabetical order.

<div class="pg-embed" markdown data-tab="haproxy.cfg" data-controls="tabs" data-title="Snippets assembled with render_glob" data-height="460">

```yaml
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateConfig
metadata:
  name: snippet-demo
spec:
  templateSnippets:
    backend-api:
      template: |
        backend api
          server s1 10.0.1.5:9000 check
    backend-web:
      template: |
        backend web
          server s1 10.0.0.1:8080 check
  haproxyConfig:
    template: |
      global
        log stdout format raw local0
        daemon
      defaults
        mode http
        timeout connect 5s
        timeout client 30s
        timeout server 30s
      frontend http
        bind *:80
        default_backend web
      {{ render_glob "backend-*" }}
```

</div>

Include a single snippet in a template:

```go
{{ render "backend-name" }}
```

Include all snippets matching a glob pattern (rendered in alphabetical order):

```go
{{ render_glob "backend-*" }}
```

`render_glob` joins the snippets exactly as they render, so a snippet that
doesn't end with a line break runs into the next one. To start each snippet on a
new line, wrap the glob in `on_own_lines()`. It also starts the first snippet on
a new line, so strip the template's own line break before the call with `{{-`:

```go
{{- on_own_lines(render_glob "backend-*") }}
```

Pass local variables to rendered snippets with `inherit_context`:

```go
{%- var service_name = "my-service" %}
{{ render "backend-servers" inherit_context }}
```

Use an [incremental snippet](./crd-reference.md#incremental-snippets) when each
watched object can produce an independently cached fragment. The reference
describes bindings, tracked inputs, shared groups, and effects.

### Post-processing

The `haproxyConfig` section supports a `postProcessing` list that transforms the rendered output before deployment. Post-processors run sequentially on the rendered configuration.

Available types:

| Type | Description |
|------|-------------|
| `regex_replace` | Line-by-line regex find/replace (`pattern` and `replace` params) |
| `template` | Template transformation with access to the rendered output via the `input` variable (`source` param) |

The config below renders a `__REGION__` marker, then runs two post-processors in order: a `template` step rewrites the marker to a value, and a `regex_replace` step renames the header. The **haproxy.cfg** tab shows the final, post-processed output.

<div class="pg-embed" markdown data-tab="haproxy.cfg" data-controls="tabs" data-title="Rewriting the output after render" data-height="460">

```yaml
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateConfig
metadata:
  name: postproc-demo
spec:
  haproxyConfig:
    template: |
      global
        log stdout format raw local0
        daemon
      defaults
        mode http
        timeout connect 5s
        timeout client 30s
        timeout server 30s
      frontend http
        bind *:80
        http-response set-header X-Region __REGION__
        default_backend web
      backend web
        server s1 10.0.0.1:8080 check
    postProcessing:
      - type: template
        params:
          source: |
            {%- if strings_contains(input, "__REGION__") -%}
            {{ replace(input, "__REGION__", "eu-west-1") }}
            {%- else -%}
            {{ input }}
            {%- end -%}
      - type: regex_replace
        params:
          pattern: "X-Region"
          replace: "X-Deployment-Region"
```

</div>

The `template` post-processor receives the fully rendered output as the `input` variable and has access to the standard template functions (`regexp`, `replace`, `len`, `tostring`, etc.). Its output becomes the new rendered content.

## Next steps

Read [resource fields](template-resources.md) in your templates and add
[validation tests](validation-tests.md) for the generated output.
