# Kogia: Lightweight Docker-Compatible Container Runtime (Go)

## Context

Kogia is a minimal, memory-efficient Docker Engine API-compatible container runtime daemon in Go. Instead of reimplementing compose/build logic, kogia exposes the Docker REST API on a Unix socket so the official `docker` CLI, `docker compose`, and `docker buildx` work unmodified. Target: ~30-50 MB steady-state RSS vs Docker's ~260 MB.

The Go implementation trades ~3-4x more memory than a theoretical Rust version for near-free API compat via generated types from Docker's Swagger spec, battle-tested image/storage libraries (containers/image, containers/storage), and dramatically lower maintenance burden. It's ~5x lighter than Docker and lighter than Podman in daemon mode (~50-80 MB).

Based on research in `cni-research.md` and `observability-research.md`.

---

## Architecture

```
Official Docker CLI / docker compose / docker buildx
          │  REST/JSON over Unix socket
          ▼
┌──────────────────────────────────────┐
│         kogia daemon (Go)            │
│  go-swagger types + net/http         │
│  ┌──────────────┐  ┌──────────────┐  │
│  │ generated API│  │ crun         │  │  ← ~30-50 MB RSS
│  │ types+router │  │ (fork/exec)  │  │
│  ├──────────────┤  ├──────────────┤  │
│  │ containers/  │  │ netlink +    │  │
│  │ image+storage│  │ nftables     │  │
│  ├──────────────┤  ├──────────────┤  │
│  │ bbolt        │  │ miekg/dns    │  │
│  │ state DB     │  │ container DNS│  │
│  └──────────────┘  └──────────────┘  │
│  moby stdcopy (streaming helpers)    │
│  [buildkitd — on-demand subprocess]  │
└──────────────────────────────────────┘
          │ fork/exec crun
          ▼
     containers (via crun)
```

---

## Key Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| OCI runtime | crun (fork/exec) | Battle-tested (Podman/CRI-O), no daemon RSS cost per container |
| Docker API types + routing | go-swagger from Docker's Swagger 2.0 spec directly | 100% spec-accurate types and routes, generated not hand-written. No conversion step. Compile-time handler contracts — missing endpoints won't compile. Update = download new swagger.yaml → regenerate. |
| Docker streaming helpers | moby stdcopy + jsonfilelog (imported) | Streaming protocols (attach, logs, exec) aren't well-modeled in Swagger. Import moby's battle-tested implementations for these. |
| Image management | containers/image + containers/storage | Full Podman-grade stack. Handles registry auth, layer extraction, overlayfs whiteouts, all storage driver complexity |
| Rootfs | containers/storage Mount/Unmount | No hand-rolled overlayfs |
| State | bbolt (go.etcd.io/bbolt) | Pure Go, no CGo, proven (etcd/Kubernetes). Single-writer but sufficient. |
| Networking | vishvananda/netlink + google/nftables + miekg/dns (in-process) | Custom CNI-compatible. internal/network/ code + thin cmd/kogia-cni binary |
| DNS | miekg/dns authoritative server on bridge gateway IP | Dynamic updates as containers join/leave. resolv.conf points containers here |
| BuildKit | On-demand subprocess | Start buildkitd when builds requested, stop after idle. Uses kogia's socket as Docker backend. |
| Container supervision | In-daemon goroutine per container | waitpid on crun process, collect exit code, manage stdio, update bbolt |
| Live-restore | Deferred to v2 | Daemon shutdown stops all containers. Simpler v1. |
| Interface | Docker API only (Unix socket) | No MCP. `DOCKER_HOST=unix:///run/kogia.sock` |
| CGo | Not required | bbolt is pure Go. containers/storage overlay driver uses pure Go. crun is external binary. |
| Logging | log/slog (stdlib) | Structured JSON, zero deps, zero overhead. Forward-looking (all major runtimes use logrus but are considering migration to slog). |
| Metrics | Prometheus + go-swagger middleware, opt-in | Off by default. `--metrics-addr=:9090` to enable. When off: 0 MB overhead, no listener, no middleware. When on: +2-3 MB RSS, ~2μs/req. Generated operation names → semantic per-endpoint metrics. Custom container lifecycle metrics added manually. |

---

## API Code Generation Workflow

```
Docker swagger.yaml (Swagger 2.0, from moby/moby repo — used directly, no conversion)
    ↓ go-swagger generate server -f api/swagger.yaml -t internal/api/gen --exclude-main
internal/api/gen/
    ├── models/              ← all Docker API types (ContainerCreateBody, ImageInspect, etc.)
    ├── restapi/             ← route registration, param binding, validation
    └── restapi/operations/  ← one handler type per endpoint
```

go-swagger generates:
- **Models**: all request/response types with JSON tags + validation
- **Operations**: one handler function type per endpoint (e.g. `container.ListContainersHandler`)
- **Route wiring**: `configureAPI()` that registers all routes on a stdlib-compatible handler
- **Param binding**: query params, path params, headers, body — parsed and validated

We implement each handler and plug into `configureAPI()`. Streaming endpoints (attach, logs follow, events, pull progress) need manual HTTP hijack/chunked handling — use moby's `stdcopy` for the wire format.

---

## Project Structure

```
kogia/
├── go.mod
├── mise.toml                        # tool versions + task runner (includes go-swagger generate task)
├── api/
│   └── swagger.yaml                 # Docker API spec (Swagger 2.0, from moby/moby — used directly)
├── cmd/
│   ├── kogia/
│   │   └── main.go                  # cobra CLI, daemon subcommand
│   └── kogia-cni/
│       └── main.go                  # standalone CNI plugin binary (thin shim)
├── internal/
│   ├── daemon/
│   │   └── daemon.go                # startup orchestrator, signal handling, shutdown sequence
│   ├── api/
│   │   ├── gen/                     # GENERATED by go-swagger (do not edit)
│   │   │   ├── models/             # all Docker API types
│   │   │   └── restapi/            # route wiring, param binding, operations
│   │   ├── server.go                # net/http server setup, middleware, Unix socket listener
│   │   └── handlers/
│   │       ├── container.go         # container CRUD, logs, attach, resize, wait
│   │       ├── exec.go              # exec create/start/inspect
│   │       ├── image.go             # pull, list, inspect, remove, tag
│   │       ├── network.go           # create, remove, connect, disconnect, inspect, list
│   │       ├── volume.go            # create, remove, inspect, list, prune
│   │       ├── build.go             # /build endpoint → buildkitd proxy
│   │       └── system.go            # _ping, version, info, events
│   ├── runtime/
│   │   ├── crun.go                  # crun exec helper (--root, error handling)
│   │   ├── container.go             # create, start, stop, kill, remove lifecycle
│   │   ├── exec.go                  # exec via crun exec
│   │   ├── spec.go                  # Docker Config → OCI runtime spec (~600-800 LOC, highest risk)
│   │   ├── io.go                    # PTY (console-socket), pipes, stdcopy multiplexing
│   │   └── wait.go                  # supervision goroutine, exit code collection
│   ├── image/
│   │   ├── pull.go                  # containers/image pull + NDJSON progress streaming
│   │   ├── store.go                 # containers/storage wrapper, Docker types translation
│   │   └── auth.go                  # ~/.docker/config.json credential reading
│   ├── network/
│   │   ├── manager.go               # NetworkManager: create/remove networks, connect/disconnect
│   │   ├── bridge.go                # netlink: bridge, veth pairs, IP assignment, namespace moves
│   │   ├── ipam.go                  # bitmap IPAM per subnet (bbolt-backed)
│   │   ├── nat.go                   # nftables: masquerade, DNAT port mapping
│   │   ├── dns.go                   # miekg/dns: authoritative DNS + host forwarding
│   │   └── cni.go                   # CNI protocol handler (ADD/DEL/CHECK/VERSION)
│   ├── store/
│   │   ├── db.go                    # bbolt setup, bucket structure, generic helpers
│   │   ├── container.go             # container state CRUD + name index
│   │   ├── network.go               # network/endpoint CRUD
│   │   └── volume.go                # volume CRUD
│   ├── volume/
│   │   └── manager.go               # named volumes at /var/lib/kogia/volumes/
│   ├── build/
│   │   ├── manager.go               # buildkitd lifecycle (start/stop/idle timeout)
│   │   └── proxy.go                 # /build + /grpc session proxy to buildkitd
│   ├── metrics/
│   │   └── metrics.go               # Prometheus metrics definitions + go-swagger middleware
│   └── events/
│       └── bus.go                   # event fan-out (Go channels → SSE NDJSON)
└── embed/
    ├── crun_linux_amd64             # static crun binary (go:embed)
    └── crun_linux_arm64
```

---

## Dependencies

```
# Docker API — generated types + routes (from Swagger 2.0 directly)
github.com/go-openapi/runtime         # runtime helpers for go-swagger generated code
github.com/go-openapi/errors          # error types used by generated code
github.com/go-openapi/loads           # spec loading
github.com/go-openapi/strfmt          # format types (date-time, uri, etc.)

# Docker streaming helpers (NOT types — those are generated)
github.com/docker/docker/pkg/stdcopy       # multiplexed stream format (attach, logs, exec)
github.com/docker/docker/daemon/logger     # jsonfilelog format for container logs
github.com/docker/docker/profiles/seccomp  # default seccomp profile

# Image + storage
github.com/containers/image/v5       # registry pull, auth, manifest parsing
github.com/containers/storage         # overlay storage driver, layer/image management

# OCI specs
github.com/opencontainers/runtime-spec  # OCI runtime spec types
github.com/opencontainers/image-spec    # OCI image spec types

# State
go.etcd.io/bbolt                     # key-value state store

# Networking
github.com/vishvananda/netlink        # bridge, veth, IP, routes
github.com/vishvananda/netns          # network namespace management
github.com/google/nftables            # NAT, port mapping, firewall

# DNS
github.com/miekg/dns                 # container DNS server

# CNI (for cmd/kogia-cni)
github.com/containernetworking/cni    # CNI spec types + skel framework

# Metrics
github.com/prometheus/client_golang    # Prometheus metrics + /metrics handler

# CLI + system
github.com/spf13/cobra               # CLI framework
golang.org/x/sys                      # Linux syscalls
```

### Build-time tools (not Go dependencies)
```
github.com/go-swagger/go-swagger      # generates types + server from Swagger 2.0 directly (go install)
```

---

## Runtime Layout

```
/var/lib/kogia/
├── kogia.db                  # bbolt database
├── containers/{id}/
│   ├── config.json           # OCI runtime spec
│   ├── json.log              # container logs (jsonfilelog format)
│   ├── hostname, hosts, resolv.conf
├── image/                    # containers/storage graphroot
│   ├── overlay/              # layer data
│   ├── overlay-images/       # image metadata
│   └── overlay-layers/       # layer metadata
├── volumes/{name}/_data/
└── buildkit/buildkitd        # extracted buildkitd binary

/run/kogia/
├── kogia.sock                # API socket
├── kogia.pid
├── crun/                     # crun state directory
├── image/                    # containers/storage runroot
└── buildkit.sock             # buildkit socket (when running)
```

---

## Observability

### Logging
`log/slog` (Go stdlib). Structured JSON output. Configurable via `--log-level` (debug, info, warn, error).

### Metrics (opt-in via `--metrics-addr=:9090`)

Following CRI-O's patterns (manual Prometheus, operation-level labels):

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

The go-swagger middleware handles `kogia_api_*` automatically (semantic operation names from generated handler names). Container/image/network lifecycle metrics are manual (~50-70 LOC).

When `--metrics-addr` is not set: zero overhead, no listener, no middleware registration.

### Tracing
Deferred to v2. Not needed for standalone Docker replacement. Can add OTel behind a flag later if kogia is used in orchestrated environments.

---

## Implementation Phases

### Phase 0: Skeleton
**Goal:** `docker version`, `docker info`, `docker ps` work.

**Create:**
- `go.mod`, `mise.toml` (Go version, go-swagger, crun download task, build task, `generate` task)
- `api/swagger.yaml` — Docker's swagger.yaml from moby/moby repo (Swagger 2.0, used directly)
- Run `swagger generate server -f api/swagger.yaml -t internal/api/gen --exclude-main` to generate models + restapi + operations
- `cmd/kogia/main.go` — cobra CLI with `daemon` subcommand. Flags: `--socket`, `--root`, `--log-level`, `--metrics-addr`
- `internal/daemon/daemon.go` — create dirs, open bbolt, init managers, start HTTP server on Unix socket, signal handler (SIGTERM/SIGINT → graceful shutdown)
- `internal/api/server.go` — net/http server setup, wraps generated handler with middleware (API version extraction from `/v{version}/...` prefix, error formatting as `{"message":"..."}`, request logging). Version prefix stripping happens in middleware before hitting generated routes.
- `internal/api/handlers/system.go` — implement generated handler interfaces for `SystemPing` (returns `"OK"` + `Api-Version`, `Docker-Experimental`, `OSType`, `Server` headers), `SystemVersion` (version JSON), `SystemInfo` (basic host data)
- `internal/api/handlers/container.go` — stub `ContainerList` handler returning `[]`
- `internal/store/db.go` — bbolt init with buckets: `containers`, `images`, `networks`, `volumes`, `ipam`, `meta`

**Note on version prefix:** Docker CLI sends requests like `/v1.47/containers/json`. The generated routes may or may not include version prefixes depending on how the swagger spec defines `basePath`. Handle with middleware that strips `/v{version}/` before dispatching to the generated handler if needed.

**Verify:**
```bash
export DOCKER_HOST=unix:///run/kogia.sock
sudo kogia daemon &
docker version && docker info && docker ps
```

---

### Phase 1: Image Pull
**Goal:** `docker pull alpine` works. `docker images` lists it.

**Create:**
- `internal/image/store.go` — init containers/storage (`StoreOptions{GraphRoot, RunRoot, GraphDriverName: "overlay"}`), translate storage.Image ↔ Docker types.ImageSummary/ImageInspect
- `internal/image/pull.go` — `docker.Transport.ParseReference()` → `storage.Transport.ParseStoreReference()` → `copy.Image()`. Stream NDJSON progress (`{"status":"Pulling...","progressDetail":{}}`)
- `internal/image/auth.go` — parse `~/.docker/config.json`, extract credentials, pass as `types.DockerAuthConfig`
- `internal/api/handlers/image.go` — `POST /images/create` (pull with progress stream), `GET /images/json` (list), `GET /images/{name}/json` (inspect), `DELETE /images/{name}` (remove), `POST /images/{name}/tag`

**Verify:**
```bash
docker pull hello-world && docker pull alpine:latest
docker images                    # lists both
docker image inspect alpine      # full JSON
docker rmi hello-world && docker images  # only alpine
```

---

### Phase 2: Container Run
**Goal:** `docker run --rm hello-world` prints output and exits.

**Create:**
- `internal/runtime/crun.go` — exec helper: `crun --root /run/kogia/crun/ <command> <args>`, captures stdout/stderr, wraps errors
- `internal/runtime/spec.go` — Docker Config → OCI spec. Start minimal (args, env, cwd, root, default namespaces, default mounts: /proc, /dev, /dev/pts, /dev/shm, /sys). Expand iteratively.
- `internal/runtime/container.go` — **Create:** generate ID (64 hex), generate name, resolve image via containers/storage, `store.CreateContainer()` for RW layer, write OCI spec to bundle dir, persist in bbolt. **Start:** `store.Mount()` → update spec root → set up stdio pipes → `crun create --bundle=... --pid-file=...` → `crun start` → launch supervision goroutine. **Stop:** `crun kill SIGTERM` → timeout → `crun kill SIGKILL`. **Remove:** stop if force, `crun delete`, `store.Unmount()`, `store.DeleteContainer()`, rm bundle dir, remove from bbolt.
- `internal/runtime/io.go` — for non-TTY: `os.Pipe()` pairs for stdout/stderr as `exec.Cmd.Stdout/Stderr`. Write to jsonfilelog format. Multiplex via stdcopy for attach/logs endpoints.
- `internal/runtime/wait.go` — one goroutine per running container, `cmd.Wait()` or direct `unix.Wait4` on container PID, collect exit code, update bbolt, close pipes, signal waiters via channel, handle auto-remove.
- `internal/store/container.go` — bbolt CRUD, name→ID index bucket, list with status/name/label/ancestor filters
- `internal/api/handlers/container.go` — `POST /containers/create`, `POST /containers/{id}/start`, `POST /containers/{id}/stop`, `POST /containers/{id}/kill`, `POST /containers/{id}/restart`, `POST /containers/{id}/wait`, `DELETE /containers/{id}`, `GET /containers/json` (with filters), `GET /containers/{id}/json`, `GET /containers/{id}/logs` (with `follow`, `stdout`, `stderr`, `since`, `tail`)

**Verify:**
```bash
docker run --rm hello-world              # prints message, exits 0
docker run -d --name ng nginx
docker ps                                # shows ng
docker logs ng                           # nginx startup
docker stop ng && docker rm ng
docker run --rm alpine cat /etc/os-release
```

---

### Phase 3: Interactive + Exec
**Goal:** `docker run -it alpine sh` and `docker exec` work.

**Modify:**
- `internal/runtime/io.go` — add TTY support: `crun create --console-socket=...`, receive PTY master fd via SCM_RIGHTS (`unix.Recvmsg`), bidirectional proxy between HTTP connection and PTY fd. Window resize via `unix.IoctlSetWinsize`.
- `internal/runtime/exec.go` — `crun exec --process=process.json {containerID}`. For TTY exec: same console-socket flow. For non-TTY: pipe stdin/stdout/stderr.
- `internal/api/handlers/container.go` — add `POST /containers/{id}/attach` (HTTP hijack via `w.(http.Hijacker).Hijack()`, then raw bidirectional stream; stdcopy framing for non-TTY, raw for TTY), `POST /containers/{id}/resize`, `POST /containers/{id}/pause` (`crun pause`), `POST /containers/{id}/unpause` (`crun resume`), `GET /containers/{id}/top`
- `internal/api/handlers/exec.go` — `POST /containers/{id}/exec` (create), `POST /exec/{id}/start` (HTTP hijack + crun exec), `GET /exec/{id}/json`

**Verify:**
```bash
docker run -it --rm alpine sh            # interactive shell, exit
docker run -d --name test alpine sleep 3600
docker exec test ls /
docker exec -it test sh                  # interactive exec
docker exec -e FOO=bar test env
docker stop test && docker rm test
```

---

### Phase 4: Networking
**Goal:** Bridge networking, port mapping, DNS resolution, container-to-container communication.

**Create:**
- `internal/network/bridge.go` — `createBridge(name, gateway, subnet)` via netlink. `connectContainer(bridge, containerPid, containerIP)`: create veth pair, move one end to container netns, assign IP, set default route, attach host end to bridge. `disconnectContainer(vethHost)`.
- `internal/network/ipam.go` — per-subnet bitmap in bbolt `ipam` bucket. `Allocate(subnet) → IP`, `Release(subnet, ip)`. Skip .0 (network) and .1 (gateway).
- `internal/network/nat.go` — nftables table `kogia` with chains: `postrouting` (masquerade per subnet), `prerouting` (DNAT per port mapping), `forward` (allow inter-container + external). `AddPortMapping()`, `RemovePortMapping()`, `Cleanup()` (flush table).
- `internal/network/dns.go` — miekg/dns server on each bridge gateway IP:53 (UDP+TCP). In-memory name→IP map per network, synced from bbolt on startup. `Register(network, name, ip)`, `Deregister(network, name)`. Forward unknown queries to host nameservers from `/etc/resolv.conf`.
- `internal/network/manager.go` — orchestrates the above. On startup: create default "bridge" network (172.17.0.0/16). Transactional connect: IPAM allocate → bridge create → veth → IP assign → nftables → DNS register → write /etc/resolv.conf. Rollback on any step failure via defer cleanup stack.
- `internal/network/cni.go` — CNI spec 1.0.0 handler (ADD/DEL/CHECK/VERSION). Reads env vars + stdin config, delegates to same bridge/IPAM/NAT code, returns CNI result JSON on stdout.
- `cmd/kogia-cni/main.go` — thin binary calling `network.CNIMain()`
- `internal/store/network.go` — bbolt CRUD for networks + endpoints
- `internal/api/handlers/network.go` — `GET /networks`, `GET /networks/{id}`, `POST /networks/create`, `DELETE /networks/{id}`, `POST /networks/{id}/connect`, `POST /networks/{id}/disconnect`, `POST /networks/prune`
- **Modify** `internal/runtime/spec.go` — add network namespace to OCI spec, generate and bind-mount `/etc/hosts`, `/etc/resolv.conf`, `/etc/hostname`
- **Modify** `internal/runtime/container.go` — on start: create netns, call `network.Connect()` per network, set up port mappings. On stop: remove NAT rules, deregister DNS, release IPs.

**Verify:**
```bash
docker run -d --name web -p 8080:80 nginx
curl localhost:8080                              # nginx welcome page

docker network create mynet
docker run -d --name db --network mynet redis
docker run --rm --network mynet alpine ping -c1 db
docker run --rm --network mynet alpine nslookup db

docker run --rm alpine wget -qO- http://example.com  # outbound NAT

docker network inspect mynet
docker stop web db && docker rm web db
docker network rm mynet
```

---

### Phase 5: Volumes + Compose
**Goal:** `docker compose up -d` with a multi-service stack works.

**Create:**
- `internal/volume/manager.go` — volumes at `/var/lib/kogia/volumes/{name}/_data/`. Create (mkdir + bbolt), Remove, Get, List.
- `internal/store/volume.go` — bbolt CRUD
- `internal/api/handlers/volume.go` — `GET /volumes`, `POST /volumes/create`, `GET /volumes/{name}`, `DELETE /volumes/{name}`, `POST /volumes/prune`
- `internal/events/bus.go` — central EventBus with subscriber channels. `Publish(event)`, `Subscribe(ctx, filters) <-chan Message`. Events for container/image/network/volume lifecycle.
- **Modify** `internal/api/handlers/system.go` — add `GET /events` (SSE NDJSON stream with filters)
- **Modify** `internal/runtime/spec.go` — add volume/bind mounts to OCI spec
- **Modify** `internal/runtime/container.go` — auto-create named volumes if they don't exist, handle `VolumesFrom`

**Compose requirements** (all must work):
- Container labels (com.docker.compose.project/service/container-number) — just regular labels
- Label-filtered container list — `GET /containers/json?filters={"label":[...]}`
- Network connect/disconnect per compose service
- Events stream for lifecycle tracking
- `POST /containers/{id}/wait` with correct exit code
- Pull before create

**Verify:**
```bash
docker volume create mydata
docker run --rm -v mydata:/data alpine sh -c 'echo hello > /data/test.txt'
docker run --rm -v mydata:/data alpine cat /data/test.txt  # "hello"

# Compose test (nginx + redis)
DOCKER_HOST=unix:///run/kogia.sock docker compose -f test-stack.yml up -d
docker compose ps && docker compose logs
curl localhost:8080
docker compose down -v
```

---

### Phase 6: Build
**Goal:** `docker build .` works via on-demand BuildKit subprocess.

**Create:**
- `internal/build/manager.go` — start `buildkitd --addr unix:///run/kogia/buildkit.sock --oci-worker-binary=crun --containerd-worker=false` on first build request. Set `DOCKER_HOST=unix:///run/kogia.sock` so BuildKit uses kogia as backend. Idle timeout (5 min) → SIGTERM → stop. Track PID.
- `internal/build/proxy.go` — proxy `/build`, `/session`, `/grpc` endpoints to buildkitd. Docker CLI uses BuildKit gRPC over HTTP upgrade; kogia upgrades the connection and proxies to buildkitd's gRPC socket.

**Verify:**
```bash
echo 'FROM alpine\nRUN echo hi > /x\nCMD cat /x' | docker build -t test -
docker run --rm test  # "hi"
```

---

## Graceful Shutdown

```
SIGTERM/SIGINT received →
  1. Stop accepting connections (close Unix socket listener)
  2. Drain in-flight API requests (5s timeout)
  3. Stop buildkitd if running
  4. For each running container (parallel):
     a. crun kill {id} SIGTERM
     b. Wait up to 10s
     c. crun kill {id} SIGKILL if still running
     d. Collect exit code, update bbolt state
     e. Unmount rootfs via containers/storage
     f. crun delete {id}
  5. Cleanup networking: flush nftables kogia table, stop DNS server
  6. Close bbolt
  7. Remove PID file + socket, exit 0
```

---

## Estimated Memory (steady-state RSS)

| Component | Estimate |
|-----------|----------|
| Go runtime | ~8 MB |
| net/http (stdlib) | ~2 MB |
| bbolt (mmap'd) | ~1-5 MB |
| containers/storage (in-memory index) | ~5-10 MB |
| Per-container (goroutine + pipes + metadata) | ~50 KB each |
| DNS server (miekg/dns) | ~2 MB |
| nftables client | ~1 MB |
| Prometheus metrics (when enabled) | ~2-3 MB |
| **Idle (0 containers, no metrics)** | **~25-35 MB** |
| **Idle (0 containers, with metrics)** | **~28-38 MB** |
| **50 containers** | **~50-65 MB** |

---

## Estimated Performance

| Operation | Latency | Notes |
|---|---|---|
| Container create | ~8-15 ms | crun fork/exec + bbolt write |
| Container start | ~15-30 ms | crun fork/exec |
| Container exec | ~10-20 ms | crun fork/exec |
| Container kill | ~3-5 ms | signal delivery |
| API reads (list/inspect) | ~3-8 ms | bbolt scan |
| User-perceived `docker run -d nginx` | ~0.5s | |
| User-perceived `docker run --rm hello-world` | ~1.0s | includes image resolve + run + cleanup |

---

## Comparison

| | Docker | Podman (daemon) | containerd (raw) | **Kogia (Go)** |
|---|---|---|---|---|
| RSS (idle) | ~260 MB | ~50 MB | ~20 MB (+shims) | **~30 MB** |
| RSS (50 containers) | ~400 MB | ~100 MB | ~240 MB (shims!) | **~55 MB** |
| Docker CLI compat | 100% | ~92% | 0% (nerdctl ~85%) | **~97%** |
| `docker compose` | 100% | ~90% | ~80% (nerdctl) | **~95%** |
| Per-container overhead | shim (4 MB) | none | shim (4 MB) | **none** |
| Maintenance burden | — | high (compat layer) | low | **low (generated API)** |

---

## Verification Strategy

- **Per-phase:** test with real `docker` CLI commands as shown above
- **Compat debugging:** `socat -v UNIX-LISTEN:/tmp/proxy.sock,fork UNIX-CONNECT:/var/run/docker.sock` to capture real Docker traffic, diff against kogia's responses
- **Unit tests:** focus on `spec.go` (highest risk), bbolt CRUD, IPAM bitmap, CNI protocol
- **CI:** GitHub Actions with privileged runner (needs root for namespaces, cgroups, overlayfs)
- **Logging:** `log/slog` structured logging, configurable via `--log-level`

---

## Critical Risks & Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| **Docker API fidelity** — undocumented CLI expectations beyond swagger spec | High | go-swagger gives us spec coverage; use socat traffic capture for undocumented behavior; golden-file tests |
| **Streaming endpoints** — 4 protocols (logs, attach, pull progress, build session) | High | Import moby's stdcopy. Implement in order of difficulty. |
| **spec.go complexity** — Docker→OCI config translation is ~800 LOC of edge cases | High | Start minimal (hello-world), expand iteratively. Extensive unit tests. |
| **containers/storage edge cases** — whiteouts, opaque dirs, metacopy | Medium | Battle-tested library handles this. Trust it. |
| **bbolt single-writer** — concurrent container creates queue on DB writes | Medium | DB writes are <1ms. Real bottleneck is cgroup/namespace setup. |
| **crun fork/exec overhead** — ~15ms per operation | Low | Acceptable for target use case. In-process option (libcrun CGo) available for v2 if needed. |
