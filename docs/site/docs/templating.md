---
search:
  boost: 2
---

# Write your first template

<a id="templating"></a>
<a id="overview"></a>

Extend the bundled configuration with a snippet when you need behavior the
available settings don't cover. A snippet adds a small piece of HAProxy
configuration at an [extension point](template-libraries.md#extension-points).
You can also replace the complete configuration or read your own resource types.

This walkthrough adds an `X-Team: storefront` response header to the shared
HTTP routing frontend. It uses an existing Helm installation named `haptic` in
the `haptic` namespace. [Install HAPTIC](getting-started.md#install-with-helm)
first if needed. You can learn the [template syntax](template-language.md) in
your browser without installing anything.

## 1. Add a snippet to your values

Keep your existing settings and add this snippet to your
[complete Helm values file](deploying-with-helm.md#change-settings),
`haptic-values.yaml`:

```yaml
controller:
  config:
    templateSnippets:
      frontend-extra-400-team-header:
        template: |
          http-response set-header X-Team storefront
```

The `frontend-extra-*` extension point includes matching snippets in the shared
HTTP frontend. The rest of the bundled templates continue to generate routing,
backends, and certificates. Snippets with the same name replace one another;
choose a new name when adding behavior.

## 2. Apply your values

Upgrade with the complete file:

```bash
helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.7.0 --namespace haptic \
  -f haptic-values.yaml
```

The chart's validation hook checks the candidate before rollout. To run that
check separately, follow [Validate before deploying](operations/validate-before-deploy.md).
For ongoing customization, add [tests](validation-tests.md) for the behavior you expect.

## 3. Check the result

Inspect the generated configuration using the CLI already in a controller pod:

```bash
kubectl exec --namespace haptic deployment/haptic-controller --container controller \
  -- haptic config view --namespace haptic
```

Find `http-response set-header X-Team storefront` in the HTTP frontend. Then
send a request through one of your Ingress routes and inspect its response
headers. If you used the [sample application](getting-started.md#optional-walkthrough-route-a-sample-app),
start port forwarding:

```bash
kubectl port-forward --namespace haptic svc/haptic-haproxy 8080:80
```

In another terminal:

```bash
curl -i -H 'Host: echo.example.local' http://localhost:8080/
```

The response includes `X-Team: storefront`. If the generated directive exists
but the response doesn't include the header, check [deployment status](operations/diagnostics.md)
and confirm that the request reaches this installation.

To remove the example, delete `frontend-extra-400-team-header` from your values
and run the same upgrade command.

## Custom template variables

To let the same template use different settings in each environment, supply
values through `templatingSettings.extraContext`. Replace the first example with
this version to choose the team name in your values file:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        team: storefront
    templateSnippets:
      frontend-extra-400-team-header:
        template: |
          http-response set-header X-Team {{ extraContext.team }}
```

Use `extraContext` for settings you control. Validate values from other users before
inserting them into HAProxy directives; see [annotation helpers](libraries/ingress-annotations-compat.md#other-exported-macros).

## Continue with your use case

<a id="what-you-can-template"></a>
<a id="haproxy-configuration"></a>
<a id="map-files"></a>
<a id="general-files"></a>
<a id="ssl-certificates"></a>
<a id="template-snippets"></a>
<a id="post-processing"></a>

### Generate configuration and files

Follow the examples in [Generate configuration and files](template-files.md) to
create a complete HAProxy configuration, maps, error pages, or certificates.

<a id="template-syntax"></a>
<a id="control-structures"></a>
<a id="helper-functions"></a>
<a id="mutable-variables"></a>
<a id="whitespace-control"></a>

### Learn the syntax

Learn expressions, loops, helper functions, and whitespace control with the
interactive examples in [Template syntax](template-language.md).

<a id="available-template-data"></a>
<a id="context-variables"></a>
<a id="the-resources-variable"></a>
<a id="typed-resource-access"></a>
<a id="collection-pipelines"></a>
<a id="x-expr"></a>
<a id="asking-whether-an-optional-field-was-set"></a>
<a id="index-configuration"></a>
<a id="common-patterns"></a>
<a id="reading-a-custom-annotation"></a>
<a id="servers-named-after-their-pods-avoid-reloads"></a>
<a id="cross-resource-lookups"></a>
<a id="safe-iteration"></a>
<a id="filtering-with-conditionals"></a>
<a id="challenge-add-health-checks"></a>
<a id="challenge-default-a-missing-port"></a>

### Read Kubernetes resources

Use [Kubernetes resources](template-resources.md) in your templates: access typed
fields, read annotations, and look up related resources.

<a id="statuspatch"></a>
<a id="condition"></a>
<a id="transitiontime"></a>
<a id="using-status-patches-in-custom-templates"></a>

### Report resource status

[Report resource status](template-status.md) with patches and conditions for the
resources your templates manage.

<a id="path-resolution"></a>
<a id="status-patches"></a>
<a id="complete-example"></a>
<a id="see-also"></a>

For a complete resource-driven routing implementation, see the
[Ingress library](libraries/ingress.md) and the
[custom-resource example](https://github.com/haproxy-haptic/HAPTIC/tree/main/examples/byo-crd).
Use the [template reference](template-reference.md) to look up functions and
[template libraries](template-libraries.md) to share your changes.
