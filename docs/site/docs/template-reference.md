# Template reference

Look up template context variables, HAPTIC functions, file paths, and status-patch
helpers. For syntax and runnable examples, start with the [templating guide](./templating.md).

## Context variables

All templates have access to the following top-level variables:

| Variable | Type | Description |
|----------|------|-------------|
| `resources` | map of stores | Kubernetes resources indexed per `watchedResources` config — entries are wrappers exposing `.List()` / `.Fetch(keys...)` / `.GetSingle(keys...)` / `.APIVersion()` (the group/version this resource is actually watched at), plus the type-carrying `.T` |
| `controller` | map of stores | Controller-managed stores; currently only `controller.haproxy_pods` for the discovered HAProxy pod set |
| `pathResolver` | object | Resolves filenames to HAProxy paths — see [`pathResolver`](#pathresolver) |
| `capabilities` | map (bool values) | Feature flags for the lowest HAProxy version in the fleet. See [version checks](#check-haproxy-features). |
| `currentConfig` | server index (or nil) | The servers the running configuration has: `currentConfig.ServerIndex["<backend>"]["<server>"].Address` and `.Port` (a pointer — check it for nil before you read it). **Nil on first deployment** — guard with `{% if !isNil(currentConfig) %}`. |
| `currentFiles` | `map[string]string` | Last validated map, general-file, and crt-list contents, keyed by base filename. Empty on first deployment; excludes Secret data. See [previous output](#read-previous-output). |
| `dataplane` | `config.Dataplane` block | The CRD's `spec.dataplane` block — the agent port, timeouts, and the auxiliary paths |
| `shared` | `*SharedContext` | Thread-safe compute-once cache for expensive computations (`shared.ComputeIfAbsent(key, factory)` + `shared.Get(key)`; no `Set` — prevents racy check-then-act patterns) |
| `templateSnippets` | list | Names of all available template snippets — useful for dynamic `render_glob` patterns |
| `runtimeEnvironment` | object | Runtime info exposed by the controller (for example `runtimeEnvironment.GOMAXPROCS`) |
| `fileRegistry` | object | Register generated files and get their deployed paths. See [file registration](#register-generated-files). |
| `http` | object | HTTP fetcher for `http.Fetch("https://example.com/...")` — see [Watching Resources — HTTP Resources](./watching-resources.md#http-resources) for the auto-registration and refresh mechanism |
| `extraContext` | map | The full `templatingSettings.extraContext` map. Read a key with `extraContext.key` or `extraContext["key"]` — see [Custom Template Variables](./templating.md#custom-template-variables). |
| `renderMode` | string | `"admission"` for a proposed resource change; `"reconcile"` for live state, config loading, and `haptic validate`. See [admission checks](#limit-a-check-to-the-admitted-resource). |
| `admissionSubject` | map | Identifies the watched resource under admission review. Empty outside single-resource admission. See [admission checks](#limit-a-check-to-the-admitted-resource). |

### Check HAProxy features

Use `capabilities` for feature checks, for example
`{% if capabilities.supports_crt_list %}…{% end %}`. Keys use `snake_case`;
a misspelled key evaluates to false rather than raising an error.

The flags reflect the lowest HAProxy version reported by the fleet. Before pod
discovery, they reflect the controller image's HAProxy binary. The chart selects
matching controller and HAProxy versions through `haproxyVersion`.

The chart also sets `extraContext.haproxyVersion`. If you deploy without the
chart, set that value yourself when your templates need it. Prefer `capabilities`
for feature checks, since a fleet can temporarily contain mixed versions.

### Read previous output

Use `currentFiles["filename"]` to read a previously validated auxiliary file.
The map is always non-nil; `len(currentFiles) == 0` covers the first deployment.
The controller sets this variable; `extraContext.currentFiles` can't override it.

The files come from the last complete, validated configuration. If that
published set is incomplete, rendering fails until it's available.

### Register generated files

`fileRegistry.Register(kind, filename, content)` registers a file during rendering
and returns `(path, error)`. Handle the error before using the path. `kind` can be `"file"`, `"cert"`, `"map"`,
`"crt-list"`, or `"ca-file"`. For example, the bundled libraries use it to
assemble CA bundles and client certificates from Secrets.

For `"file"`, an optional fourth argument, `reloadOnPush`, defaults to `true`.
Pass `false` only for a file read by a sidecar rather than HAProxy; a content
change then deploys without reloading HAProxy.

### Limit a check to the admitted resource

Combine `renderMode` with `admissionSubject` when a rule should reject only the
resource being created or edited. A `fail()` against live state aborts the whole
render and can prevent unrelated changes from deploying. Use
[`recordEvent()`](#recordevent) when you need to report a live violation without
blocking the render.

`admissionSubject` contains `namespace`, `name`, and a `stores` map of affected
`watchedResources` aliases. Check an alias with
`admissionSubject | dig("stores", alias) | fallback(false)`. The `store` field
is set only when exactly one alias is affected.

The map is empty during reconciliation, configuration proposals, and bulk overlays.
Both `admissionSubject` and `renderMode` are controller-set variables;
`extraContext` can't override them.

## Functions and filters

Every entry below is callable in two equivalent styles: as a plain function (`fn(x, args...)`) or via the pipe operator (`x | fn(args...)`), which passes the left-hand value as the first argument. The pipe requires parentheses on the right side — `{{ value | toLower() }}` works, `{{ value | toLower }}` is a parse error. The one exception is `len`, a language builtin: call `len(x)` directly.

| Function | Purpose | Example |
|----------|---------|---------|
| `fallback(value, default)` | Return `default` if `value` is nil. Empty strings and zeroes pass through — only `dig()` on optional typed fields normalises zero values to nil first | `fallback(dig(settings, "port"), 80)` |
| `dig(obj, "k1", "k2", ...)` | Walk a nested map / typed struct without nil-checking each level (navigates JSON tags on typed structs) | `dig(ing, "metadata", "annotations")` |
| `toSlice(v)` | Coerce `any` to `[]any` (safe to range over even if nil) | `for _, r := range toSlice(dig(item, "rules"))` |
| `to_str_map(v)` | Copy a string-keyed map to `map[string]string`; each value must be a deterministic scalar | <code>for k, v := range route.metadata.labels &#124; to_str_map()</code> |
| `shard_slice(items, idx, n)` | Type-preserving split of a slice into `n` shards, returning shard `idx` — input element type is kept | `shard_slice(gateways, i, totalShards)` |
| `tostring(v)` | Convert nil, a boolean, a finite number, a string, or a pointer to one of those scalars to text. Composite values and custom formatting methods fail the render; use field access or `toJSON()` instead | `name = tostring(dynamicName)` |
| `toint(v)`, `tofloat(v)` | Numeric conversions from `any` | `port = toint(annotation)` |
| `len(v)` | Length of slice / map / string (language builtin — not pipeable) | `len(ing.spec.rules)` |
| `keys(m)` | Sorted keys of a map | `for _, k := range keys(annotations)` |
| `merge(a, b)` | New map combining `a` and `b` (b wins on conflict) | `merge(defaults, overrides)` |
| `toLower(s)` / `toUpper(s)` | Case conversion | `host = toLower(rule.host)` |
| `replace(s, old, new)`, `split(s, sep)`, `join(slice, sep)`, `strip(s)`, `trim(s, cutset)`, `hasPrefix(s, p)`, `hasSuffix(s, p)` | String operations (`strip` trims whitespace; `trim` takes an explicit cutset) | `join(items, ", ")` |
| `first_seen(prefix, keys...)` | Returns `true` only the first time the key tuple is seen — for deduplicating | `if first_seen("backend", svc.metadata.namespace, svc.metadata.name)` |
| `sanitize_regex(s)` | Escape regex metacharacters in user input | `sanitize_regex(annotation)` |
| `regex_search(s, pattern)` | True when the RE2 pattern matches anywhere in the string. Both arguments are coerced with `tostring()` first, and a pattern that doesn't compile aborts the render | `{% if regex_search(name, "ssl.*passthrough") %}` |
| `semver_gte(version, "3.3")` | Compare a semver string (major.minor) against a target | `if semver_gte(extraContext.haproxyVersion, "3.3")` (the chart auto-populates `extraContext.haproxyVersion`; outside the chart, set it yourself via `templatingSettings.extraContext.haproxyVersion` — see [Custom Template Variables](./templating.md#custom-template-variables)) |
| `fail(msg)` | Abort rendering with an error message (surfaces in validation tests and webhooks) | `fail("missing required annotation")` |
| `b64decode(s)` | Decode base64 strings (Secret `.data` values) | <code>{{ secret.data.password &#124; b64decode() }}</code> |
| `b64encode(s)` | Encode a value as standard base64 | <code>{{ configmap.data.schema &#124; b64encode() }}</code> |
| `parse_yaml(text)` | Decode one YAML document into a value plus an error; rejects duplicate keys and additional documents | `{% var value, err = parse_yaml(text) %}` |
| `public_key_info(pem)` | Parse one RSA, ECDSA, or Ed25519 public key; returns a map with `algorithm`, `bits`, `curve`, and canonical `PUBLIC KEY` PEM in `pem`, plus an error. Private keys and extra PEM blocks are rejected | `{% var info, err = public_key_info(publicKey) %}` |
| `untar_gz(archive)` | Expand a `.tar.gz` archive into a map of entry path to content. Returns `(map[string]string, error)`; any failure returns no files. Keeps regular files and their directory paths; select with `keys()` + `glob_match()`. Rejects path traversal and invalid compression checksums. Limits: 4096 entries, 8 MiB per file, 32 MiB of extracted content, and 64 MiB for the complete decompressed stream, including skipped entries and metadata | `{%- var files, err = untar_gz(archive) %}` |
| `glob_match(items, pattern)` | Filter strings by glob pattern | <code>{{ templateSnippets &#124; glob_match("backend-*") }}</code> |
| `map_extract(items, keyPath)` | Pluck one field (dotted key path) from each item into a flat slice | <code>{{ routes &#124; map_extract("routeId") }}</code> |
| `indent(s, n)` | Indent lines by N spaces (first and blank lines excluded) | <code>{{ render "snippet" &#124; indent(4) }}</code> |
| `on_own_lines(render_glob "prefix-*")` | Render the matching snippets so that each starts on a new line. A line break is added only where a snippet would otherwise continue the line before it. The text before the call counts as an unfinished line, so write the call after `{{-`. Takes only a `render_glob` expression | `{{- on_own_lines(render_glob "frontend-extra-*") }}` |
| `debug(v, label)` | Output as JSON comment | <code>{{ routes &#124; debug("routes") }}</code> |
| `toJSON(v)` | Convert a value to JSON; an unsupported value fails the render | <code>{{ myMap &#124; toJSON() }}</code> |
| `basename(path)` | Filename portion of a path, like Unix `basename` | `basename("/etc/haproxy/maps/hosts.map")` |
| `namespace(init)` | Mutable `map[string]any` for accumulating state across loop iterations | `{%- var acc = namespace(map[string]any{"n": 0}) %}` |
| `isNil(v)` | Nil check that also catches a typed nil pointer boxed in an `any` | `{% if !isNil(currentConfig) %}` |
| `coalesce(value, default)` | First non-nil of the two — the plain-call spelling of `fallback` | `coalesce(annotation, "default")` |

### Strings and types

| Function | Purpose | Example |
|----------|---------|---------|
| `strings_contains(s, sub)`, `strings_split(s, sep)`, `strings_splitn(s, sep, n)`, `strings_trim(s)`, `strings_lower(s)` | `any`-tolerant string operations — they coerce their arguments, so they work on values read out of a `map[string]any` without a cast first. `strings_trim` trims whitespace; `strings_splitn` caps the result at `n` parts | `strings_splitn(annotation, ":", 2)` |
| `title(s)` | Title-case a string | `title(mode)` |
| `isdigit(s)` | True when the string is non-empty and all digits — check before `toint()` | `{% if isdigit(port) %}` |
| `toStringSlice(items)` | Copy scalar elements from a slice to `[]string`; a composite element fails the render | `toStringSlice(hosts)` |
| `cidr_partition(cidrs)` | Split a set of IPv4/IPv6 prefixes (a bare address is a host prefix) into the disjoint blocks each prefix is a union of, as `map[input][]block`. A `map_ip` lookup over all blocks names exactly one block for any address, so per-route CIDR lists become two map lookups instead of a config line | `cidr_partition(allowlists)["10.0.0.0/8"]` |
| `sort_strings(items)` | Sort deterministic scalar elements lexicographically, returning `[]string` | `sort_strings(keys(m))` |
| `sort_ints(items)` | Sort a `[]any` numerically, returning `[]int` — use for ports and IDs, where `sort_strings` would put `"10"` before `"2"`. Non-integer entries coerce through `toint()` and sort to the front | `sort_ints(ports)` |
| `ceil(f)` | Round a float up | `ceil(tofloat(total) / 4)` |
| `seq(n)` | `[]int{0, 1, …, n-1}`, for fixed-count loops | `{% for _, i := range seq(count) %}` |
| `append(slice, item)` | Go's builtin, type-preserving; `append(dst, src...)` spreads a slice of the **same** type. Widening into `[]any` is a compile error — box per element in a loop. A slice reached through `any` is asserted at the boundary | `append(gf["hosts"].([]any), h)` |
| `dig_string(obj, default, keys...)` | `dig` + `fallback` + `tostring` in one call, for annotation and metadata lookups | <code>ing &#124; dig_string("", "metadata", "annotations", key)</code> |
| `join_key(sep, parts...)` | Join any values into one composite key string | `join_key("_", ns, name, port)` |
| `make_guid(parts...)` | Build a value for HAProxy's `guid` directive from parts joined by `:`, auto-truncating with a hash suffix past its 127-character limit | `guid {{ make_guid("be", beKey) }}` |
| `selectattr(items, attr[, op, value])` | Jinja2-style attribute filter. `op` is `eq`, `ne` or `in`; omitted, it keeps items whose `attr` is truthy. `attr` is one literal key — a dotted path matches nothing, so prefer `filter` with a closure | `selectattr(rules, "host", "ne", "")` |

### Collection pipelines

Type-preserving stages, chained with `|`. Each stage retains type information for its result, so typed field access works in later stages and misspelled fields fail compilation. Predicates are closures — write them long-hand (`func(e T) bool { … }`) or as `x => expr` with both types inferred. See [Templating — Collection pipelines](./template-resources.md#collection-pipelines) for the guided version.

| Function | Purpose | Example |
|----------|---------|---------|
| `map(items, fn)` | One output per input | <code>pods &#124; map(p => p.metadata.name)</code> |
| `filter(items, pred)` | Keep the elements the predicate accepts | <code>ingresses &#124; filter(i => len(i.spec.tls) > 0)</code> |
| `reject(items, pred)` | Drop them instead, so the call site reads as a positive statement | <code>eps &#124; reject(e => e.targetRef.name == "")</code> |
| `flat_map(items, fn)` | Map to slices and concatenate, flattening exactly one level | <code>slices &#124; flat_map(s => s.endpoints)</code> |
| `unique(items)` | First occurrence of each distinct element, input order preserved | <code>hosts &#124; unique()</code> |
| `unique_by(items, key)` | First element per key. `key` is a closure, or an attribute path for `any`-shaped data | <code>hosts &#124; unique_by(h => toLower(h))</code> |
| `group_by(items, key)` | Bucket by string key, input order preserved within each bucket. Same two key forms. Iterate the result through `keys()` — Go map order isn't stable, and a reordered render reads as a change to the controller | <code>ingresses &#124; group_by("metadata.namespace")</code> |
| `sort_by(items, criteria)` | Sort by JSONPath expressions — see [`sort_by` modifiers](#sort_by-modifiers) | <code>routes &#124; sort_by([]string{"$.priority:desc"})</code> |
| `sort_by(items, cmp)` | Sort with a `func(a, b T) int` comparator (Go's `cmp` convention: negative when `a` sorts first), for orderings the criteria language can't state. Stable, like the criteria form | <code>routes &#124; sort_by(func(a, b Route) int { return a.Rank - b.Rank })</code> |

`sort_by` is the one stage that returns `(value, error)`. As a pipe stage that's invisible — the pipe keeps only the first result, so `x | sort_by(…)` assigns to one variable. A **direct** call returns both and needs two: `var rows, err = sort_by(items, criteria)`.

The attribute-path form of `unique_by` and `group_by` navigates dotted paths:
`"metadata.namespace"` reads the `namespace` field inside `metadata`.

### Governance helpers

Read or derive fields on watched resources by JSONPath. The chart's
[governance rules](./operations/governance.md) use these helpers.

| Function | Purpose | Example |
|----------|---------|---------|
| `resource(name)` | The per-render items of a watched resource named *dynamically* | `resource(extraContext.targetStore)` |
| `jsonpathGet(item, path)` | Read a concrete JSONPath out of an item | `jsonpathGet(ing, "$.spec.ingressClassName")` |
| `deriveResource(name, item, path, value)` | Publish a detached resource value to subsequent `List`, `Fetch`, `GetSingle`, and `resource` reads. Returns the transformed item | `ing = deriveResource("ingresses", ing, "$.metadata.annotations.x", "1")` |
| `jsonpathSet(item, path, value)` | Write a concrete JSONPath into an item, in place. Returns whether the write landed | `jsonpathSet(ing, "$.metadata.annotations.x", "1")` |

Without incremental snippets, a root can call `deriveResource` before later
reads. When incremental snippets are configured, migrate every derivation
producer to the one component that declares the `deriveResource` effect for its
source. The derived view freezes before roots render, so a later root call fails.

### Incremental values

| Function | Purpose | Example |
|----------|---------|---------|
| `incremental_values(group, cell)` | Return the winning structured values published by a configured incremental group, in deterministic winner order | `{% for _, host := range incremental_values("tls", "hosts") %}` |
| `incremental_value_count(group, cell)` | Return how many winning values a cell holds without decoding them. Use it for presence tests: the root then depends on the count alone, so a change to one value doesn't re-run the root | `{% if incremental_value_count("tls", "hosts") > 0 %}` |

An incremental component that declares the `publishValue` effect publishes a
value with `shared.Publish(cell, key, value)`. `incremental_values` is available
only to root templates. It can evaluate a group before the group's normal
render call, but roots must still render one or more complete group sequences.
Each sequence contains every component in snippet-name order and stays within
one root template. Repeating a sequence mounts its cached text again without
re-executing component bodies or effects.
An auxiliary root may read a group mounted in `haproxy.cfg`, which renders
first. Once a root starts its own producer sequence, that sequence must complete
before the root reads the group. A group mounted only in auxiliary roots must
complete its sequence in each consuming root because auxiliary roots render
concurrently.
Each call returns newly detached, immutable values. Unknown groups and groups
without `publishValue` fail; an absent cell in a known publication group returns
an empty slice.

Inside a component that declares the producer group in `consumes`, use
`shared.Select(group, cell, key)` for one exact winner or
`shared.SelectValues(group, cell)` for all winners in canonical order. Use
`shared.Count(group, cell)` for the number of unique winning publication keys
in a cell. These calls require a complete authenticated producer-group call and
register exact dependencies. Present and missing `Select` reads change only for
that key; losing or unrelated publishers don't execute the consumer.
`SelectValues` reruns the consumer only when the returned values or their order
changes. `Count` is O(1) and reruns it only when the observed count changes;
deleting the last owner changes it, while promoting another owner for the same
key doesn't.

Use `shared.PublishRanked(cell, key, rank, value)` when ownership has a chart
precedence rule. The lexicographically smallest non-empty rank wins, followed
by the normal deterministic owner order. Don't mix ranked and rank-free owners
for the same cell and key.

For complete coverage including crypto, encoding, and Scriggo built-ins (`abs`, `min`, `max`, `sprintf`, `now()`, etc.), see the [Scriggo built-ins reference](https://scriggo.com/templates/builtins).

### `sort_by` modifiers

`sort_by` criteria accept modifiers: `:desc` (descending), `:exists` (by field presence), `| length` (by length).

`sort_by` is a stable sort: items whose keys compare equal on every criterion keep their original input order. Add a final deterministic criterion (such as `$.name`) only when you want a specific order for otherwise-equal items rather than input order.

**Example — route precedence sorting:**

```go
{% var sorted, sortErr = sort_by(routes, []string{
    "$.match.method:exists:desc",
    "$.match.headers | length:desc",
    "$.match.path.value | length:desc",
}) %}
{% if sortErr != nil %}{{ fail("sorting routes: " + sortErr.Error()) }}{% end %}
```

### Regex flavor

HAPTIC has two regex surfaces, and they use different engines:

- **Template-level regex** — `regex_search`, `sanitize_regex`, and the `regex_replace` post-processor — runs on Go's `regexp` package, which implements RE2 syntax. RE2 has no backreferences and no look-around assertions; a pattern that needs those won't compile. The `regex_replace` post-processor also runs line by line, so a pattern can't span a newline, and `^` / `$` anchor to each line rather than the whole document.
- **HAProxy-runtime regex** — patterns HAProxy evaluates itself, such as `map_reg` lookups, a Gateway API `RegularExpression` path match, or `server-alias-regex` — uses HAProxy's Perl Compatible Regular Expressions (PCRE) engine, which does support backreferences and look-around. A pattern that works in one surface may be rejected by the other.

!!! warning "Regex safety with user-supplied patterns"
    Template-level regex runs on RE2, which is linear-time and can't catastrophically backtrack, so a pattern in `regex_search`, `sanitize_regex`, or `regex_replace` is never a regular-expression denial-of-service (ReDoS) vector, whatever the input. PCRE does backtrack, and HAPTIC imposes no PCRE match limit — a pathological pattern-and-input pair reaching the HAProxy-runtime surface is a potential data-plane CPU-exhaustion risk. When patterns can come from tenants you don't control (for example an Ingress or HTTPRoute authored in a namespace you don't own), match on the RE2 side where you can, and review any tenant-supplied runtime regex.

### Emitting warnings

Use [`recordEvent()`](#recordevent) to publish a Kubernetes Warning Event
without aborting the render. Use `fail(message)` when invalid input must stop
configuration generation.

For troubleshooting a template value, `debug(value, label)` writes a JSON
comment into the generated configuration. The comment is visible in the
rendered `HAProxyCfg`, so don't use it for secrets.

## `pathResolver`

`pathResolver` is available in every template. Its `GetPath(filename, type)` method returns the path that HAProxy should use to reference an auxiliary file (map, error file, certificate, crt-list). Use it instead of writing paths by hand so the controller and HAProxy agree on where files live.

**Arguments**: `filename` (string), `type` (one of `"map"`, `"file"`, `"cert"`, `"crt-list"`)

```go
{# Map files — resolves to maps/host.map #}
use_backend %[req.hdr(host),lower,map({{ pathResolver.GetPath("host.map", "map") }})]

{# General files — resolves to general/504.http #}
errorfile 504 {{ pathResolver.GetPath("504.http", "file") }}

{# SSL certificates — resolves to ssl/example_com.pem (dots in cert/crt-list names are sanitized to _) #}
bind *:443 ssl crt {{ pathResolver.GetPath("example.com.pem", "cert") }}

{# crt-list files — resolves to general/cert-list.txt #}
bind *:443 ssl crt-list {{ pathResolver.GetPath("cert-list.txt", "crt-list") }}
```

`GetPath` returns paths relative to HAProxy's `default-path`. The bundled base
library sets this automatically. If you replace that library, include the
following directive in your `global` section so HAProxy can locate the files:

```go
default-path origin {{ pathResolver.GetBaseDir() }}
```

## Status-patch functions

Templates register status patches with `statusPatch()`; the controller applies them to the resource's `/status` subresource via Server-Side Apply (SSA) after each reconciliation phase. The [Templating Guide — Status Patches](./template-status.md) walks through the pattern with runnable examples.

### `statusPatch()`

Registers a status patch for a Kubernetes resource with outcome-keyed variants. Pass the resource object itself; the function reads `apiVersion`, `kind`, `metadata.namespace`, `metadata.name`, `metadata.uid`, and `metadata.resourceVersion` generically. Each variant's value is the resource's `.status` content directly (for example `conditions`, `loadBalancer`) — the controller writes it under `.status` via SSA, so don't wrap it in another `status` key.

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `resource` | resource object | The watched resource to patch — a typed resource or an unstructured resource map |
| `variants` | `map[string]any` | Status payloads keyed by pipeline phase |
| `ownedLists` (optional) | `map[string]any` | JSON pointers within `.status`, mapped to selectors identifying the list entries your template owns |

**Variants:**

| Key | Applied When |
|-----|-------------|
| `rendered` | After successful template rendering (before deployment) |
| `deployed` | After successful HAProxy deployment |
| `renderFailed` | When a later rendering phase fails |
| `deployFailed` | When HAProxy deployment fails |

Templates render all variants upfront. The controller selects the variant for
the current outcome; pass the original watched resource to preserve its identity.

If another controller writes entries to the same list, declare which entries you
own. HAPTIC replaces matching entries and preserves the others, rereading the
resource if a concurrent update causes a conflict.

```go
{{ statusPatch(resource, map[string]any{
  "deployed": map[string]any{
    "entries": []any{map[string]any{"writer": "my-controller", "ready": true}},
  },
}, map[string]any{"/entries": map[string]any{"writer": "my-controller"}}) }}
```

Each selector must be a nonempty object. Every entry you supply for that list
must match it. Pass an empty list to remove your entries while retaining other
writers' entries. Omitting the list from a variant leaves it out of that patch.

### `condition()`

Creates a `metav1.Condition`-compatible map.

**Parameters:** `type`, `status`, `reason`, `message`, `observedGeneration`, `lastTransitionTime`

```go
{{ condition("Accepted", "True", "Accepted", "Resource is accepted", 1, "2024-01-01T00:00:00Z") | toJSON() }}
```

### `transitionTime()`

Returns the correct `lastTransitionTime` for a condition: preserves the existing timestamp if the condition status hasn't changed, or returns the current time if it has changed or doesn't exist yet.

**Parameters:** `existingConditions` (the resource's existing conditions list), `type`, `status`.

For nested conditions, find the entry by its identity and owner before calling
`transitionTime()`. Its position in a list can change when other controllers
write status. For Gateway API routes, match `controllerName` and `parentRef`,
then pass that entry's `conditions` list.

## Event functions

### `recordEvent()`

Records a Kubernetes `Warning` Event for a watched resource. Pass the resource
object and inspect the resulting Event with `kubectl describe <kind> <name>` or
`kubectl get events`. Only the leader emits Events.

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `resource` | resource object | The watched resource to record the Event against — a typed resource, a `map`, or an unstructured object (for example, an item from `resources.ingresses.List()`) |
| `reason` | `string` | Short, machine-readable `PascalCase` reason (for example, `RouteConflict`) |
| `message` | `string` | Human-readable description |

```go
{% recordEvent(ingress,
    "RouteConflict", "host \"" + rule.host + "\" path \"" + path.path + "\" is already served by another Ingress") %}
```

The Event is a side-effect only — the call renders nothing. Identical `(resource, reason, message)` tuples emitted during one render collapse into a single Event. The controller re-emits on every reconcile while the condition holds, so the standard Kubernetes Event aggregation keeps it fresh and it ages out (default TTL ~1 hour) once the template stops recording it. The bundled Ingress library uses this to surface [route conflicts](./libraries/ingress.md#conflicting-routes-the-oldest-ingress-wins) on the losing Ingress.

<a id="typed-access-internals"></a>

## Typed resource types

With a schema loaded, `resources.<name>.List()` and `.Fetch()` return
`[]*resources.<name>.T`; `.GetSingle()` returns `*resources.<name>.T` or `nil`.
A top-level variable named `<name>` also exposes the typed resource list.
Use these types in macros, functions, slices, or type switches:

```scriggo
{% macro GatewayName(gateway *resources.gateways.T) %}
{{ gateway.metadata.name }}
{% end %}
```

Nested types use the field path: `resources.gateways.SpecListeners` is the
listener type and `resources.endpoints.endpoints` is the EndpointSlice endpoint
type. The watch must have a schema defining that path.

JSON field names and generated Go names both work: `gateway.metadata.name`
and `gateway.Metadata.Name`. Generated names capitalize the first character;
`apiVersion` becomes `ApiVersion`, not `APIVersion`. Non-letter/digit characters
become underscores. Prefer JSON field names in your templates.

For examples and schema setup, see [Read resources in templates](template-resources.md).
