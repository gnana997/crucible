---
title: Observability
description: "Per-app metrics on Prometheus /metrics, a reference Grafana dashboard, and (coming) OTLP export — crucible emits open standards and delegates routing to your collector."
---

# Observability

crucible is a telemetry **source**, not a pipeline. It emits open standards —
Prometheus `/metrics` today, OTLP (metrics, logs, traces) as it lands — and
delegates routing and fan-out to the ecosystem (an OpenTelemetry Collector,
Vector, or Grafana Alloy). One OTLP export reaches Grafana/Tempo/Loki, SigNoz,
Datadog, Honeycomb, and the rest without any vendor-specific code in the daemon.

## Metrics — `GET /metrics`

The daemon exposes a Prometheus endpoint (default on the API listener). Point a
scrape at it:

```yaml
scrape_configs:
  - job_name: crucible
    static_configs:
      - targets: ["<daemon-host>:7878"]
```

### Per-app series

Labels are kept to a fixed, low-cardinality set — `app`, `code` (HTTP status
**class**: `2xx`…`5xx`), never a raw path or client IP. A request for an unknown
host is not counted, so an attacker can't inflate labels.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `app_requests_total` | counter | `app`, `code` | requests the ingress proxy routed to an app, by status class |
| `app_request_duration_seconds` | histogram | `app` | request latency (accept → response written) |
| `app_replicas` | gauge | `app` | desired instances |
| `app_ready_replicas` | gauge | `app` | ready (serving) instances |
| `app_up` | gauge | `app` | 1 if the app has a running instance |
| `app_asleep` | gauge | `app` | 1 if scaled to zero (asleep) — the scale-to-zero density signal |
| `app_sleep_total` | counter | `app` | sleep cycles the app has been through |
| `app_last_wake_latency_ms` | gauge | `app` | most recent wake latency |

Plus the existing global series: `sandboxes_active`, `sandboxes_created_total`,
`snapshots_active`, `fork_duration_seconds`, `snapshot_restore_duration_seconds`,
`app_wake_latency_seconds` (aggregate histogram), `app_internal_requests_total`.

### Reference dashboard

Import [`docs/observability/grafana-dashboard.json`](observability/grafana-dashboard.json)
into Grafana (Dashboards → Import → upload JSON, pick your Prometheus source). It
charts RPS and 5xx ratio per app, request-latency percentiles, replicas
(desired vs ready), the fraction of the fleet asleep, and wake-latency p95.

## Profiling — `--pprof-listen`

For profiling the daemon itself (CPU, heap, goroutines):

```bash
crucible daemon … --pprof-listen 127.0.0.1:6060
go tool pprof http://127.0.0.1:6060/debug/pprof/heap
```

Off by default. pprof exposes process memory, so bind **loopback** (or protect
the port) — the daemon warns on a non-loopback bind.

## OTLP metric export

The daemon can push the **same `/metrics` series** over OTLP to any OTLP backend
or your own Collector/Vector/Alloy — no metric is redefined; an OpenTelemetry
Prometheus **bridge** pulls the registry and exports it. One flag turns it on:

```bash
crucible daemon … --otlp-endpoint http://collector:4317        # gRPC (default)
crucible daemon … --otlp-endpoint http://collector:4318 --otlp-protocol http
```

- `--otlp-protocol grpc|http`, `--otlp-headers k=v,k=v` (auth/tenant),
  `--otlp-insecure` (plaintext).
- Standard **`OTEL_EXPORTER_OTLP_*`** and `OTEL_RESOURCE_ATTRIBUTES` /
  `OTEL_SERVICE_NAME` env vars are honored natively (flags override them), so if
  you already run OpenTelemetry it just works.
- The export carries a resource of `service.name` (default `crucible`),
  `service.version`, and `host.name`, plus your `OTEL_RESOURCE_ATTRIBUTES`.
- Off by default. Setup failures are logged and skipped — `/metrics` keeps
  serving regardless.

`/metrics` and OTLP are two views of one registry; use either, or both.

## OTLP log export

When `--otlp-endpoint` is set (and `--log-dir` is on), the daemon also streams
**app logs** over OTLP — every durable log line becomes an OTel log record with:

- `service.*` resource + `crucible.app.instance` (the instance id),
- `log.source` (`service` | `exec`) and `log.stream` (`stdout` | `stderr` | `event`),
- the original timestamp; `stderr` maps to severity `WARN`, else `INFO`.

It taps the log store's **best-effort fanout** — a slow OTLP backend can never
back-pressure the app (records drop rather than block). Disable with
`--otlp-logs=false` (metrics-only). Logs remain locally readable via `crucible
logs` / `crucible app logs -f` regardless.

## Traces over OTLP (coming)

Trace export is the next milestone (deploy / sleep / wake / proxy spans).
