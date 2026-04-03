# Research: Container Runtime Observability Infrastructure

## Problem Statement

Understand how Docker (dockerd + containerd), Podman, and CRI-O implement observability (metrics, tracing, logging) to inform kogia's instrumentation design.

---

## Docker (dockerd + containerd)

### Metrics

**dockerd** exposes Prometheus metrics on a dedicated port, configured via `daemon.json`:
```json
{ "metrics-addr": "127.0.0.1:9323" }
```
Endpoint: `http://<host>:9323/metrics`

Uses `docker/go-metrics` — a thin wrapper around `prometheus/client_golang` that enforces naming conventions via namespaces:
```go
ns := metrics.NewNamespace("engine", "daemon", nil)
containerActions = ns.NewLabeledTimer("container_actions", "...", "action")
metrics.Register(ns)
// Usage:
containerActions.WithValues("start").UpdateSince(startTime)
```

**Daemon metrics** (`engine_daemon_*`):

| Metric | Type | Labels |
|---|---|---|
| `engine_daemon_container_actions_seconds` | Histogram | `action` = {start, changes, commit, create, delete} |
| `engine_daemon_network_actions_seconds` | Histogram | `action` |
| `engine_daemon_host_info_functions_seconds` | Histogram | `function` |
| `engine_daemon_engine_info` | Gauge | `version`, `commit`, `architecture`, `graphdriver`, `kernel`, `os`, `os_type`, `os_version`, `daemon_id` |
| `engine_daemon_engine_cpus_cpus` | Gauge | — |
| `engine_daemon_engine_memory_bytes` | Gauge | — |
| `engine_daemon_health_checks_total` | Counter | — |
| `engine_daemon_health_checks_failed_total` | Counter | — |
| `engine_daemon_container_states_containers` | Gauge | `state` = {running, paused, stopped} |

**Builder metrics** (`builder_*`):

| Metric | Type | Labels |
|---|---|---|
| `builder_builds_triggered_total` | Counter | — |
| `builder_builds_failed_total` | Counter | `reason` (8 categories) |

Plus standard `go_*`, `process_*`, `promhttp_*`.

**containerd** exposes metrics separately:
```toml
[metrics]
  address = "127.0.0.1:1338"
  grpc_histogram = false
```
Endpoint: `http://<host>:<port>/v1/metrics`

Uses `prometheus/client_golang` + `go-grpc-prometheus` interceptors:
- `grpc_server_handled_total{grpc_code, grpc_method, grpc_service}`
- `grpc_server_handling_seconds_bucket` (only if `grpc_histogram = true`)
- `grpc_server_msg_received_total`, `grpc_server_msg_sent_total`
- `container_cpu_usage_usec_microseconds`, `container_memory_usage_bytes` (via cgroups monitor plugin)

### Tracing

**dockerd**: No OTel tracing support.

**containerd**: Full OTel tracing, gRPC-level:
```toml
[plugins."io.containerd.tracing.processor.v1.otlp"]
  endpoint = "http://localhost:4318"
  protocol = "http/protobuf"
[plugins."io.containerd.internal.v1.tracing"]
  sampling_ratio = 1.0
  service_name = "containerd"
```
Trace context propagates from kubelet via gRPC metadata. Without the tracing plugin, all spans are no-ops.

### Logging

Both use **logrus** (`github.com/sirupsen/logrus`). containerd wraps it in `containerd/log` with context propagation (`log.G(ctx)`). Text format default, JSON via `--log-format=json`. containerd considering `log/slog` for 2.0.

---

## CRI-O

### Metrics

Prometheus metrics on a dedicated HTTP server:
```toml
[crio.metrics]
enable_metrics = true
metrics_port = 9090
metrics_host = "127.0.0.1"
```
Endpoint: `http://<host>:9090/metrics`

Uses **manual `prometheus/client_golang`** instrumentation (NOT go-grpc-prometheus middleware).

**Operation metrics** (labels include `operation` — one per CRI RPC method):

| Metric | Type | Description |
|---|---|---|
| `crio_operations_total` | Counter | Cumulative operations by type |
| `crio_operations_latency_seconds_total` | Summary | Latency including sub-operations |
| `crio_operations_latency_seconds` | Gauge | Last-observed call latency |
| `crio_operations_errors_total` | Counter | Errors by operation type |

The `operation` label covers 24+ CRI methods: `RunPodSandbox`, `CreateContainer`, `StartContainer`, `StopContainer`, `PullImage`, `Exec`, `ExecSync`, `Attach`, etc. Plus sub-operations: `network_setup_pod`, `network_setup_overall`.

**Image pull metrics:**

| Metric | Type | Labels |
|---|---|---|
| `crio_image_pulls_bytes_total` | Counter | `mediatype`, `size` |
| `crio_image_pulls_skipped_bytes_total` | Counter | `size` |
| `crio_image_pulls_success_total` | Counter | — |
| `crio_image_pulls_failure_total` | Counter | `error` (21 categories: UNAUTHORIZED, NOT_FOUND, TOOMANYREQUESTS, etc.) |
| `crio_image_pulls_layer_size` | Histogram | bucket boundaries |
| `crio_image_layer_reuse_total` | Counter | — |

**Container lifecycle metrics:**

| Metric | Type | Labels |
|---|---|---|
| `crio_containers_oom_total` | Counter | — |
| `crio_containers_oom_count_total` | Counter | `name` |
| `crio_containers_dropped_events_total` | Counter | — |
| `crio_containers_seccomp_notifier_count_total` | Counter | `name`, `syscall` |
| `crio_resources_stalled_at_stage` | Gauge | stage |
| `crio_processes_defunct` | Gauge | — |

**Selective metric collectors**: `metrics_collectors` config allows enabling only specific metric groups — directly addresses cardinality and memory concerns.

### Tracing

Native **OTel distributed tracing**:
```toml
[crio.tracing]
enable_tracing = true
tracing_endpoint = "127.0.0.1:4317"
tracing_sampling_rate_per_million = 1000000
```

Uses `otelgrpc` interceptors on the CRI gRPC server. Spans created per CRI method with nested child spans for sub-operations (network setup, sandbox creation). Trace context propagates from kubelet (Kubernetes 1.27+).

### Logging

**logrus** with context-aware wrapper (`internal/log`). gRPC log interceptors in `internal/log/interceptors`.

---

## Podman

### Metrics

**No native Prometheus metrics endpoint.** Architectural consequence of being daemonless.

External exporter: **[prometheus-podman-exporter](https://github.com/containers/prometheus-podman-exporter)** (containers/ org), port 9882. Uses libpod Go library directly — doesn't need `podman.socket`.

Exposes ~24 container metrics (`podman_container_*`):
- State, health, CPU, memory, block I/O, network I/O, PIDs, rootfs size, exit code, timestamps

**Zero internal instrumentation.** No metrics middleware, no counters, no histograms anywhere in the API server code.

### Tracing

**None.** No OTel, no Jaeger, no distributed tracing.

### Logging

**logrus** throughout. Container log drivers: journald (default), k8s-file, json-file, passthrough, none.

### Events System (Podman's unique strength)

`Eventer` interface with journald/file/none backends. 8 event types, 50+ statuses. Structured journald fields:

| Field | Content |
|---|---|
| `PODMAN_EVENT` | Status (start, stop, create, etc.) |
| `PODMAN_TYPE` | Resource type (container, image, pod) |
| `PODMAN_NAME` | Resource name |
| `PODMAN_ID` | Resource ID |
| `PODMAN_IMAGE` | Image reference |
| `PODMAN_EXIT_CODE` | Exit code |
| `PODMAN_CONTAINER_INSPECT_DATA` | Full inspect JSON (opt-in) |

---

## Comparison

| Capability | Docker (dockerd) | containerd | CRI-O | Podman |
|---|---|---|---|---|
| **Native /metrics** | Yes (:9323) | Yes (/v1/metrics) | Yes (:9090) | No (external exporter) |
| **Metrics library** | docker/go-metrics (prometheus wrapper) | prometheus + go-grpc-prometheus | prometheus (manual) | None |
| **Metric prefix** | `engine_daemon_*` | `grpc_server_*`, `container_*` | `crio_*` | `podman_container_*` (external) |
| **Operation-level metrics** | Yes (container actions) | Yes (gRPC methods) | Yes (24+ CRI ops + sub-ops) | No |
| **Image pull metrics** | No | No | Yes (bytes, success, failure by error category) | No |
| **Container resource metrics** | Via cAdvisor | Via cgroups plugin | No (defers to kubelet) | Via external exporter |
| **OTel tracing** | No | Yes (gRPC-level) | Yes (gRPC + sub-operations) | No |
| **Selective metric groups** | No | No | Yes (`metrics_collectors`) | N/A |
| **Logging library** | logrus | logrus (with ctx wrapper) | logrus (with ctx wrapper) | logrus |
| **Structured events** | Docker events API | containerd events | CRI events | journald (richest) |

## Key Takeaways for Kogia

1. **Everyone uses logrus** — but all are considering migration (containerd → slog for 2.0). Kogia's choice of `log/slog` is forward-looking.

2. **Manual Prometheus is the standard** — none use auto-generated metrics. CRI-O's manual instrumentation with operation-level labels is the gold standard.

3. **CRI-O's patterns to adopt:**
   - Operation-level metrics with sub-operation breakdown (network setup time separate from total)
   - Image pull failure categorization by error type (21 categories)
   - Selective metric collectors to control cardinality
   - OOM and seccomp violation tracking

4. **Docker's patterns to adopt:**
   - `engine_daemon_container_actions_seconds` histogram by action — directly maps to kogia's container lifecycle
   - `engine_daemon_container_states_containers` gauge by state — cheap overview metric
   - Builder metrics for BuildKit operations

5. **Kogia's metric namespace**: `kogia_*` (following CRI-O's pattern of runtime-prefixed metrics)

6. **Recommended kogia metrics (initial set):**

   | Metric | Type | Labels |
   |---|---|---|
   | `kogia_container_operations_seconds` | Histogram | `operation` = {create, start, stop, kill, remove, exec} |
   | `kogia_container_operations_total` | Counter | `operation`, `status` = {success, error} |
   | `kogia_container_states` | Gauge | `state` = {running, paused, stopped, created} |
   | `kogia_api_request_duration_seconds` | Histogram | `operation` (from go-swagger handler name) |
   | `kogia_api_requests_total` | Counter | `operation`, `code` |
   | `kogia_image_pull_duration_seconds` | Histogram | — |
   | `kogia_image_pull_bytes_total` | Counter | — |
   | `kogia_image_pulls_total` | Counter | `status` = {success, error} |
   | `kogia_network_operations_seconds` | Histogram | `operation` = {connect, disconnect, create, remove} |
   | `kogia_containers_oom_total` | Counter | — |

7. **OTel tracing**: Not needed for v1. CRI-O and containerd add it for Kubernetes trace propagation — kogia doesn't have that use case. Can be added later behind a flag.

---

*Research conducted: 2026-04-03*
