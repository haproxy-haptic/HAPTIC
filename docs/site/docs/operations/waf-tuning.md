# Tune WAF rules before blocking requests

Start a [WAF policy](waf-policies.md) in `enforcement: detect` mode to observe
rule matches before blocking requests. Review legitimate traffic, narrow any
false-positive exclusions, then switch to `deny`.

A policy without `enforcement` uses `waf.dispatch.defaultEnforcement`. A
self-service catalog can set a different enforcement only when
`waf.ingressPermissions.allowEnforcementOverride` is `true`; otherwise leave
`enforcement` out of self-service policies.

You need a configured policy, access logs, and [Prometheus monitoring](monitoring.md).
The examples use policy `my-policy`; select your policy in the queries. Apply
changes through the [catalog that defines it](waf-policies.md#configure-a-trusted-catalog).

## Read the per-rule hit metrics

The hub serves Prometheus metrics on `spoaHub.hub.metricsAddr` (default `127.0.0.1:9095` inside the HAProxy pod). The coraza plugin (v0.7.0+) exports:

| Metric | Labels | Meaning |
| ------ | ------ | ------- |
| `plugin_coraza_rule_hits_total` | `phase`, `rule_id`, `severity`, `app` | Every rule that matched, on every evaluation — including traffic that was allowed. This is the detect-mode signal. |
| `plugin_coraza_denials_total` | `phase`, `rule_id`, `app` | Requests denied, labeled with the single interrupting rule. Stays flat in detect mode. |
| `plugin_coraza_evaluations_total` | `phase`, `action`, `app` | All evaluations by outcome. |

The `app` label is the Coraza application: `policy:<name>` for a trusted-catalog policy, `policy:<namespace>/<name>` for a self-service policy, and `<namespace>/<name>` for route-local rules. Rules that declare no severity (the ruleset's administrative and reporting rules) carry `severity="none"`.

With the default Vector sidecar, the hub listens on loopback. Vector re-exports
its metrics on port `9598`; the [bundled PodMonitor](monitoring.md#enable-the-bundled-monitoring)
scrapes that endpoint. If Vector is disabled, the hub exposes port `9095` for
direct scraping. To inspect the default loopback endpoint from the pod:

```console
kubectl exec -n haptic deployment/haptic-haproxy -c haproxy -- \
  sh -c 'command -v curl >/dev/null && curl -s 127.0.0.1:9095/metrics || wget -qO- 127.0.0.1:9095/metrics' \
  | grep plugin_coraza_rule_hits_total
```

## Identify would-block rules

In detect mode, a request is "would block" when its accumulated anomaly score crosses the ruleset's threshold — visible as hits on the blocking-evaluation rules `949110`/`949111`. Over a representative traffic window (a week that includes your batch jobs and deploys is a good default):

```promql
# How often would this policy have blocked?
sum by (app) (increase(plugin_coraza_rule_hits_total{rule_id=~"94911[01]"}[7d]))

# Which rules fired at all, worst first?
sort_desc(sum by (rule_id, severity) (
  increase(plugin_coraza_rule_hits_total{app="policy:my-policy", severity!="none"}[7d])
))
```

Classify the hits before changing enforcement. Check that the observation
window includes normal application traffic and that logs and metrics are being
collected. No recorded hits alone doesn't prove that a policy is suitable.

## Classify hits from the access log

Rule-hit metrics tell you *which* rules fire. To tie a rule hit to one request, read HAProxy's JSON access log — every request already carries the WAF verdict, correlated with `req_id`:

| Field | Answers |
| ----- | ------- |
| `waf_rule_id` | which Core Rule Set (CRS) rule interrupted, for the one 403 a user complained about |
| `waf_score` | The request's anomaly score compared with the policy threshold |
| `waf_rules_hit` | how many rules matched on this request — one noisy rule, or twenty |
| `waf_rule_ids` | **which rules** put the request over the threshold, most severe first: `930130,920420`. In anomaly mode `waf_rule_id` is the scoring rule `949110` |
| `waf_matched_var` | **which request fields** the rules matched on, as names: `ARGS_GET:id,REQUEST_LINE`. Never the values |
| `waf_action` | `allow` or `deny`, so detect-mode traffic is distinguishable |
| `denied_by` | `waf` when the WAF blocked the request |

These fields are enabled with Coraza. See [access logging](access-logging.md)
for collection and destination settings.

`waf_matched_var` names up to five request fields, with the reported rule's
fields first. It contains field names rather than their values.

```json
{"waf_action":"deny","waf_rule_id":942100,"waf_score":5,"waf_rules_hit":3,
 "waf_matched_var":"ARGS_GET:id,REQUEST_LINE","denied_by":"waf"}
```

This example reports rule 942100 and the `id` query argument. If you confirm
that legitimate values in this field trigger the rule, you can exclude that
field while retaining the rule for other inputs:

```yaml
my-policy:
  ruleExclusions:
    - rules: [942100]
      excludeTarget: "ARGS:id"
```

If other applications share `my-policy`, put the exclusion in a policy that
[extends](waf-policies.md#extend-a-policy) it, and select that policy on the
affected route only:

```yaml
my-app:
  extends: my-policy
  ruleExclusions:
    - rules: [942100]
      excludeTarget: "ARGS:id"
```

## Exclude false positives

Use `ruleExclusions` for specific rules or fields and `allowedMethods` for
legitimate HTTP methods. Both work in self-service catalogs. Prefer excluding
the request field or path that triggers a rule over disabling the whole rule,
which removes its check from every input.

`ruleExclusions` covers the full range from a whole attack category down to a single rule on a single path. You supply only rule IDs or CRS tags, an exact target variable, and a literal path; the chart writes the CRS directive:

```yaml
my-policy:
  ruleExclusions:
    # drop a request field from a whole attack category (a search box
    # tripping SQL-injection and XSS):
    - tags: [attack-sqli, attack-xss]
      excludeTarget: "ARGS:q"
    # disable one rule only on matching paths (a git host, where CRS rule
    # 930130 fires on every .git/ git-over-HTTP URL):
    - rules: [930130]
      onPathContains: ".git/"        # or onPathPrefix / onPathExact / onPathSuffix
    # drop one parameter from a single rule (optionally path-scoped):
    - rules: [941320]
      excludeTarget: "ARGS:wp_post"
    # disable a rule everywhere in this app:
    - rules: [913100]
```

Exclusions can't disable CRS setup, scoring, or correlation rules
(900000–901999, 949xxx, 959xxx, 980xxx, 990xxx+). Use exact variable names;
regex collection keys such as `ARGS:/regex/` are rejected.

Widen the method allowlist when a whole class of hits comes from a method the app legitimately uses (`PUT`, `PATCH`, `DELETE` on an HTTP API) — set the policy's `allowedMethods` instead of excluding rule targets one by one.

## See the value a rule matched on

To recognize the same matched value across requests, enable rule-match logging
with `matched_data_log = "hash"`. This records a hash in place of that value.
Keep your other plugin parameters: setting `params` replaces the whole block.

```yaml
spoaHub:
  plugins:
    coraza:
      params: |
        detect_only = false
        transaction_ttl_ms = 10000
        max_cached_transactions = 1024
        rule_match_log = true
        matched_data_log = "hash"
        expose_matched_data = false
```

The line carries `sha256:<hex>` in place of the value, plus the rule id and the rule's own message.

!!! warning "Matched values can contain credentials"
    `matched_data_log = "truncate"` writes request content to the plugin's logs.
    Prefer field names or hashes. If you need literal values, restrict access
    and retention for the destination before enabling collection.

When you need the literal value, return it through SPOE rather than logging it in the hub. Set `expose_matched_data = true` (the value is capped by `matched_data_max_bytes`, default 128 bytes) and the plugin hands the matched data back to HAProxy in `txn.hub.coraza.data`:

```yaml
spoaHub:
  plugins:
    coraza:
      params: |
        detect_only = false
        transaction_ttl_ms = 10000
        max_cached_transactions = 1024
        rule_match_log = false
        matched_data_log = "none"
        expose_matched_data = true
```

Then add it to the access log with a `log-fields-*` snippet. Contribute it as a snippet rather than through `accessLog.fields`, because a log-format item is evaluated when the line is written — after the WAF has run — while `accessLog.fields` captures at request time:

```yaml
controller:
  config:
    templateSnippets:
      log-fields-900-waf-matched-data:
        template: |-
          %(waf_data)[var(txn.hub.coraza.data)]
```

Route this access log to a restricted destination through `accessLog.targets`.

## Last resort: the Coraza audit log

When you need the full transaction — every matched rule with its target and the request metadata together — enable Coraza's own audit engine through the trusted policy's `secLang`. A self-service catalog can't: ask the administrator to adopt the policy, or to enable the log in the shared directives.

Set `SecAuditLogParts` explicitly when enabling this log. The example below
omits request and response bodies, but still records headers, query strings,
and matched values. These can contain credentials or personal data.

```yaml
my-policy:
  enforcement: detect
  secLang: |
    SecAuditEngine RelevantOnly
    SecAuditLogParts ABFHKZ
    SecAuditLog /dev/stdout
    SecAuditLogFormat JSON
```

Records land on the spoa-hub container's stdout as JSON, one per request that matched a rule:

```console
kubectl logs -n haptic deployment/haptic-haproxy -c spoa-hub | grep '"transaction"'
```

The audit writer writes directly to `SecAuditLog`; hub log levels and
`accessLog.targets` don't control it. Choose a mounted file destination instead
of `/dev/stdout` if these records must stay out of your general log collection.

Turn it off once the policy is tuned.

## Enable blocking {#flip-to-deny}

After checking the policy against representative legitimate traffic, set
`enforcement: deny` in its catalog and apply the change. Watch
`plugin_coraza_denials_total` and investigate unexpected blocks through the
access log's `waf_rule_id` and `denied_by` fields. Disable any temporary
matched-value or audit logging after the investigation.
