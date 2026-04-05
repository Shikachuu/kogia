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
| Docker API types | moby/moby/api types directly (no generation) | Identical to Docker's own types — zero drift risk. Handlers import from `github.com/moby/moby/api/types/*` sub-packages. |
| HTTP routing | Custom generator (`hack/gen-routes`) from Docker's Swagger 2.0 spec | Reads spec with `go-openapi/loads`, emits `Handler` interface + `RegisterRoutes()` + `NotImplemented` struct using stdlib `net/http.ServeMux`. Compile-time handler contracts — missing methods won't compile. Streaming/hijack endpoints get correct `http.ResponseWriter` signatures (unlike go-swagger/oapi-codegen). Update = download new swagger.yaml → regenerate. |
| Docker streaming helpers | moby stdcopy + jsonfilelog (imported) | Streaming protocols (attach, logs, exec) aren't well-modeled in Swagger. Import moby's battle-tested implementations for these. |
| Image management | containers/image + containers/storage | Full Podman-grade stack. Handles registry auth, layer extraction, overlayfs whiteouts, all storage driver complexity |
| Rootfs | containers/storage Mount/Unmount | No hand-rolled overlayfs |
| State | bbolt (go.etcd.io/bbolt) | Pure Go, no CGo, proven (etcd/Kubernetes). Single-writer but sufficient. |
| Networking | vishvananda/netlink + google/nftables + miekg/dns (in-process) | Custom CNI-compatible. internal/network/ code + thin cmd/kogia-cni binary |
| DNS | miekg/dns authoritative server on bridge gateway IP | Dynamic updates as containers join/leave. resolv.conf points containers here |
| BuildKit | docker-container buildx driver | Builds use `docker buildx` with the docker-container driver, which manages its own buildkitd container. No in-daemon build subprocess. |
| Container supervision | In-daemon goroutine per container | waitpid on crun process, collect exit code, manage stdio, update bbolt |
| Live-restore | Deferred to v2 | Daemon shutdown stops all containers. Simpler v1. |
| Interface | Docker API only (Unix socket) | No MCP. `DOCKER_HOST=unix:///run/kogia.sock` |
| CGo | Not required | bbolt is pure Go. containers/storage overlay driver uses pure Go. crun is external binary. |
| Logging | log/slog (stdlib) | Structured JSON, zero deps, zero overhead. Forward-looking (all major runtimes use logrus but are considering migration to slog). |
| Metrics | Prometheus, opt-in | Off by default. `--metrics-addr=:9090` to enable. When off: 0 MB overhead, no listener, no middleware. When on: +2-3 MB RSS, ~2μs/req. Custom container lifecycle metrics added manually. |

---

## API Code Generation Workflow

```
Docker swagger.yaml (Swagger 2.0, from moby/moby repo — used directly, no conversion)
    ↓ hack/gen-routes (custom generator using go-openapi/loads)
internal/api/gen/routes.go
    ├── Handler interface    ← one method per endpoint (107 operations), plain (http.ResponseWriter, *http.Request)
    ├── RegisterRoutes()     ← wires all routes to stdlib net/http.ServeMux
    └── NotImplemented       ← embeddable struct providing default 501 for every method
```

**No go-swagger. No OpenAPI 3.0 conversion. No generated models.**

Docker API types come directly from `github.com/moby/moby/api/types/*` — the same types Docker itself uses. Handlers import from moby's sub-packages (`types/system`, `types/container`, etc.) and encode/decode JSON themselves.

The custom generator (`hack/gen-routes/main.go`) was chosen over go-swagger and oapi-codegen because:
- **Streaming endpoints** (attach, exec, logs, events) use HTTP hijacking that can't be modeled in OpenAPI. go-swagger/oapi-codegen produce wrong handler signatures for these. Our generator emits standard `(http.ResponseWriter, *http.Request)` — handlers can hijack as needed.
- **go-swagger's generated models** conflict with moby's types (`x-go-type` extensions, inline schema extraction, validation interfaces). Using moby types directly eliminates all compatibility issues.
- **Moby's own OpenAPI 3.0 migration** (PR #51565) is still unmerged after 4+ months, confirming spec conversion is non-trivial.
- **Zero framework dependencies.** Just stdlib `net/http`, `encoding/json`, and moby types.

The `NotImplemented` struct allows incremental implementation: embed it in the handler struct, override methods as each endpoint is implemented. Unimplemented endpoints return `501 Not Implemented` with a JSON error body.

Streaming endpoints (attach, logs follow, events, pull progress) need manual HTTP hijack/chunked handling — use moby's `stdcopy` for the wire format.

---

## Project Structure

```
kogia/
├── go.mod
├── mise.toml                        # tool versions + task runner
├── api/
│   └── swagger.yaml                 # Docker API spec (Swagger 2.0, from moby/moby — used directly)
├── hack/
│   ├── download-swagger.sh          # downloads swagger.yaml from moby (version from go.mod)
│   └── gen-routes/
│       └── main.go                  # custom route generator (go-openapi/loads → routes.go)
├── cmd/
│   ├── kogia/
│   │   └── main.go                  # cobra CLI, daemon subcommand
│   └── kogia-cni/
│       └── main.go                  # standalone CNI plugin binary (thin shim)
├── internal/
│   ├── daemon/
│   │   └── daemon.go                # startup orchestrator, signal handling, shutdown sequence
│   ├── api/
│   │   ├── gen/                     # GENERATED by hack/gen-routes (do not edit)
│   │   │   ├── gen.go               # go:generate directive
│   │   │   └── routes.go            # Handler interface, RegisterRoutes(), NotImplemented
│   │   ├── server.go                # net/http server setup, middleware, Unix socket listener
│   │   └── handlers/
│   │       ├── respond.go           # generic respondJSON[T] helper
│   │       ├── container.go         # container CRUD, logs, attach, resize, wait
│   │       ├── exec.go              # exec create/start/inspect
│   │       ├── image.go             # pull, list, inspect, remove, tag
│   │       ├── network.go           # create, remove, connect, disconnect, inspect, list
│   │       ├── volume.go            # create, remove, inspect, list, prune
│   │       ├── build.go             # /build, /build/prune, /session stubs (→ use docker-container driver)
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
# Docker API types (used directly, not generated)
github.com/moby/moby/api             # Docker API types from moby's own packages (types/system, types/container, etc.)

# Docker streaming helpers
github.com/moby/moby/v2/pkg/stdcopy       # multiplexed stream format (attach, logs, exec)
github.com/moby/moby/v2/daemon/logger     # jsonfilelog format for container logs

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
github.com/go-openapi/loads           # used by hack/gen-routes to parse swagger.yaml at generation time
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
└── volumes/{name}/_data/

/run/kogia/
├── kogia.sock                # API socket
├── kogia.pid
├── crun/                     # crun state directory
└── image/                    # containers/storage runroot
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

### Phase 0: Skeleton ✅
**Goal:** `docker version`, `docker info`, `docker ps` work.

**Implemented:**
- `go.mod`, `mise.toml` (Go 1.26.1, golangci-lint, goreleaser, trivy, yq — build task injects Docker API version from swagger spec via ldflags)
- `api/swagger.yaml` — Docker's swagger.yaml (API v1.54), downloaded from moby/moby via `hack/download-swagger.sh` (version derived from `github.com/moby/moby/v2` in go.mod)
- `hack/gen-routes/main.go` — custom code generator reads swagger spec with `go-openapi/loads`, emits `internal/api/gen/routes.go` with 107 operations (Handler interface + RegisterRoutes + NotImplemented). Regenerate with `mise run gogen`.
- `cmd/kogia/main.go` — cobra CLI with `daemon` subcommand. Flags: `--socket`, `--root`, `--log-level`. Docker API version injected at build time via ldflags.
- `internal/daemon/daemon.go` — create dirs, write PID file, open bbolt, init handlers, start HTTP server on Unix socket, signal handler (SIGTERM/SIGINT → graceful shutdown with 5s timeout)
- `internal/api/server.go` — net/http server setup using stdlib `ServeMux`, middleware chain: API version prefix rewriting (`/v{any}/...` → `/v{dockerAPIVersion}/...` + bare `/_ping`), request logging (slog), panic recovery. Routes wired via `gen.RegisterRoutes()`.
- `internal/api/handlers/system.go` — `SystemPing` (returns `"OK"` + Docker headers), `SystemPingHead`, `SystemVersion` (JSON-encoded `system.VersionResponse` from moby types), `SystemInfo` (JSON-encoded `system.Info`). Daemon ID generated once and persisted in bbolt.
- `internal/api/handlers/container.go` — `ContainerList` returns typed empty `[]*container.Summary{}`
- `internal/api/handlers/respond.go` — generic `respondJSON[T]` helper for type-safe JSON responses
- `internal/api/handlers/handlers.go` — `Handlers` struct embeds `gen.NotImplemented` for default 501 on all unimplemented endpoints
- `internal/store/db.go` — bbolt init with `meta` bucket (other buckets added in later phases)
- All 102 unimplemented endpoints return `501 Not Implemented` with JSON error body

**Note on version prefix:** Docker CLI sends requests like `/v1.47/containers/json`. Middleware rewrites any `/v{version}/` prefix to the basePath from the swagger spec (`/v1.54/`) before dispatching to the `ServeMux`. Bare `/_ping` is also handled.

**Verify:**
```bash
mise run build && mise run dev &
export DOCKER_HOST=unix://$(pwd)/bin/.kogia-dev/run/kogia.sock
docker version && docker info && docker ps
```

---

### Phase 1: Image Management ✅
**Goal:** Full image lifecycle — pull, list, inspect, tag, remove, history, prune, search, save/load, push.

**Implemented:**
- `internal/image/store.go` — containers/storage wrapper. Configurable driver via `StorageDriver` type (overlay, vfs, fuse-overlayfs). `List()`, `Get()`, `Remove()`, `Tag()`, `History()`, `Prune()`. OCI config parsing for inspect (architecture, rootfs, labels, config). Image name resolution with normalization (`alpine` → `docker.io/library/alpine:latest`).
- `internal/image/pull.go` — `copy.Image()` from docker transport → storage transport. NDJSON progress streaming via `progressWriter` (wraps `containers/image` text into `{"status":"..."}` lines, flushes via `http.Flusher`). In-band error reporting for streaming responses.
- `internal/image/push.go` — Push from local storage to docker registry. Auth support, NDJSON progress streaming. Resolves name+tag for images where CLI sends them separately.
- `internal/image/export.go` — Export one or more images as Docker archive tar via `docker/archive.Writer`. Used by `docker save`.
- `internal/image/load.go` — Import images from Docker archive tar via `docker/archive.Reader`. NDJSON progress streaming. Used by `docker load`.
- `internal/image/search.go` — Docker Hub registry search via HTTP API. Supports `term` and `limit` params.
- `internal/image/auth.go` — `X-Registry-Auth` header decoding (base64 JSON) + `~/.docker/config.json` parsing (inline auth field). Priority: header → config.json → anonymous.
- `internal/api/handlers/image.go` — All image handlers except `ImageCommit` (requires running containers, Phase 2):
  - `ImageCreate` (POST /images/create) — pull with NDJSON progress
  - `ImageList` (GET /images/json) — list all images
  - `ImageInspect` (GET /images/{name}/json) — full inspect with OCI config
  - `ImageDelete` (DELETE /images/{name}) — remove/untag
  - `ImageTag` (POST /images/{name}/tag) — add tag
  - `ImageHistory` (GET /images/{name}/history) — layer history from OCI config
  - `ImagePrune` (POST /images/prune) — remove dangling images
  - `ImageSearch` (GET /images/search) — Docker Hub search
  - `ImageGet` (GET /images/{name}/get) — export single image as tar
  - `ImageGetAll` (GET /images/get) — export multiple images as tar
  - `ImageLoad` (POST /images/load) — import from tar
  - `ImagePush` (POST /images/{name}/push) — push to registry
- `internal/api/handlers/respond.go` — `respondJSON[T]`, `errorJSON`, `pathValue` (URL-decodes path params after slashy-path middleware encoding)
- `internal/api/server.go` — `encodeSlashyPathParams` middleware: URL-encodes slashes in image/plugin/distribution names so `{name}` in ServeMux matches multi-segment references (e.g., `docker.io/library/alpine`). `responseWriter.Flush()` delegation for streaming endpoints.
- `internal/daemon/daemon.go` — Image store initialization at startup (GraphRoot from `--root`, RunRoot from socket dir parent). Configurable `--storage-driver` flag.
- `cmd/kogia/main.go` — `PreRunE` validates all CLI params (log level, storage driver, paths resolved to absolute). `--storage-driver` flag (overlay/vfs/fuse-overlayfs).
- `cmd/kogia/reexec.go` — Handles `containers/storage` subprocess re-execution for chroot layer operations. Detects reexec invocations via environmental signals (OPT env var, fd probing) and patches `os.Args[0]` before `reexec.Init()` — works around Go 1.26+ behavior where `os.Args[0]` resolves to binary path instead of handler name.
- `.goreleaser.yaml` — Updated `main` to package path (not single file) for multi-file builds. Build tag `containers_image_openpgp` for CGO_ENABLED=0 compatibility.
- `.golangci-lint.yml` — Added `containers_image_openpgp` build tag.
- `mise.toml` — Added `containers_image_openpgp` build tag to build/test tasks.

**Dependencies added:** `containers/image/v5`, `containers/storage`

**Verify:**
```bash
docker pull hello-world && docker pull alpine:latest
docker images                    # lists both
docker image inspect alpine      # full JSON
docker image history alpine      # layer history
docker image tag alpine myalpine:v1
docker rmi myalpine:v1
docker rmi hello-world && docker images  # only alpine
docker save alpine -o /tmp/alpine.tar && docker rmi alpine
docker load -i /tmp/alpine.tar && docker images  # alpine restored
docker search nginx              # Docker Hub search
docker image prune               # remove dangling images
```

---

### Phase 2: Container Run ✅
**Goal:** `docker run --rm hello-world` prints output and exits.

**Implemented:**
- `internal/runtime/crun.go` — `CrunConfig` struct (BinaryPath + RootDir). `run()` generic command executor appends `--root` flag, captures stderr. `createWithIO()` runs `crun create --bundle --pid-file` passing stdout/stderr `*os.File` directly (not wrappers) to avoid internal pipe + goroutine leak. `start()`, `kill()` (with `--signal`), `deleteContainer()` (with `--force`). All commands have 30s timeout.
- `internal/runtime/spec.go` — `GenerateSpec()` produces OCI 1.2.0 spec from Docker config. Merges entrypoint + cmd per Docker precedence rules. Builds environment (image + container config), ensures PATH and HOSTNAME. User/group resolution via `/etc/passwd` and `/etc/group` in rootfs (`parseUser()`, `lookupUser()`, `lookupGroup()` — supports all Docker formats: uid, uid:gid, username, username:group). Sets default Linux capabilities (42 caps) or all for privileged. Resource limits (memory, CPU, PIDs). Default mounts: /proc, /dev, /dev/pts, /dev/shm, /dev/mqueue, /sys, /sys/fs/cgroup. Namespaces: PID, mount, IPC, UTS (no network yet — Phase 4). Cgroup path: `kogia/{hostname}`. Exported `BuildArgs()` helper for entrypoint + cmd merging.
- `internal/runtime/container.go` — `Manager` struct with `active` map (tracks running containers), `pidMap` (container PID → ID), bundleRoot, store, images, crun references. **Create:** 32 random bytes → 64-char hex ID. Auto-generated Docker-style names ("adjective_scientist##") via `names.go`, or user-provided with `/` prefix. Resolves image, creates RW layer via containers/storage, mounts rootfs, generates OCI spec, writes config.json to bundle dir, persists `InspectResponse` to bbolt. **Start:** Mounts rootfs, updates spec root path, creates jsonfile log driver, creates stdio pipes, starts copy goroutines *before* `crun create` (deadlock prevention), reads PID from pid-file, registers in `active`+`pidMap` *before* `crun start` (race prevention for instant-exit containers), updates bbolt state to "running", calls `crun start`. **Stop:** Sets `manuallyStopped` flag (prevents restart policy from firing), sends SIGTERM with timeout, waits on `ac.done` channel, sends SIGKILL if timeout, cleans up crun state. **Kill:** Sends arbitrary signal. **Remove:** Stops if force, `crun delete`, unmounts rootfs, deletes storage container, removes bundle dir, removes from bbolt. **Restart:** Stop → Start. **Wait:** Handles "created" state by polling until active entry appears, blocks on `ac.done` channel, returns stored exit code. **Inspect/List:** Read from bbolt. **HandleExit:** Called by reaper — closes stdio/log driver, updates bbolt to exited, handles auto-remove and restart policies (exponential backoff: 100ms base, 2x multiplier, 1 min cap). **RecoverOrphans:** On startup, marks "running"/"restarting" containers as exited (exit code 137), cleans up "created" containers entirely. **Shutdown:** Gracefully stops all containers (10s timeout).
- `internal/runtime/io.go` — `containerIO` struct with stdout/stderr pipe pairs. `newContainerIO()` creates `os.Pipe()` pairs. `startCopyLoop()` launches 2 goroutines reading pipes → writing to jsonfile log driver (buffered scanner, 64KB buffer, 1MB max line, each line timestamped). `WriterFds()` returns write FDs for crun. `MarkWritersClosed()` records that Start() closed write-ends after crun inherits them. `Close()` closes read-ends, waits for copy goroutines, closes log driver. `writersClosed` flag guards against double-close.
- `internal/runtime/wait.go` — `SetSubreaper()` sets `PR_SET_CHILD_SUBREAPER` on daemon so orphaned container processes reparent to daemon. `StartReaper()` goroutine handles SIGCHLD via `signal.Notify`, calls `reapChildren()` on each signal. `reapChildren()` loops `unix.Wait4` to collect all exited children, extracts exit code (Exited vs Signaled paths), detects OOM kills by reading `/sys/fs/cgroup/kogia/{id[:12]}/memory.events`, calls `HandleExit()` for each matched PID.
- `internal/runtime/defaults.go` — Constants: `DefaultStopTimeout=10s`, `DefaultStopSignal=SIGTERM`, `DefaultKillSignal=SIGKILL`, `RestartBackoffBase=100ms`, `RestartBackoffMultiplier=2`, `RestartBackoffMax=1min`, `WaitPollInterval=100ms`, `CrunOperationTimeout=30s`, `DefaultPathEnv`, `ContainerIDBytes=32`.
- `internal/runtime/names.go` — `generateName()` returns Docker-style "adjective_scientist##" names (e.g., "jolly_curie42"). Falls back to `container_{random_hex}` after 10 failed attempts.
- `internal/store/container.go` — Three bbolt buckets: `containers` (ID → JSON InspectResponse), `container_names` (name → ID), `container_bundles` (ID → bundle path). `CreateContainer()` checks name uniqueness. `GetContainer()` resolves by full ID, name, or ID prefix (cursor-based, detects ambiguous prefixes). `UpdateContainer()`, `DeleteContainer()` (removes from all buckets). `ContainerNameExists()`. `SetContainerBundle()`/`GetContainerBundle()`. `ContainerFilters` struct supports Docker-style filtering by ID, Name, Status, Label, Ancestor, with Limit and All controls. `ListContainers()` applies all filters.
- `internal/api/handlers/container.go` — All Phase 2 endpoints implemented:
  - `ContainerCreate` (POST /containers/create) — validates name/config/host config, calls runtime.Create(), returns ID + warnings
  - `ContainerStart` (POST /containers/{id}/start) — returns 204 on success, 304 if already running
  - `ContainerStop` (POST /containers/{id}/stop) — parses timeout query param
  - `ContainerKill` (POST /containers/{id}/kill) — parses and validates signal name
  - `ContainerRestart` (POST /containers/{id}/restart) — parses timeout
  - `ContainerWait` (POST /containers/{id}/wait) — flushes 200 OK immediately, blocks until exit, writes JSON body when done
  - `ContainerDelete` (DELETE /containers/{id}) — parses force param
  - `ContainerList` (GET /containers/json) — parses filters JSON, converts to Summary format
  - `ContainerInspect` (GET /containers/{id}/json) — returns full InspectResponse
  - `ContainerLogs` (GET /containers/{id}/logs) — streams in Docker stdcopy format (8-byte header: stream_type + 3 padding + 4-byte BE size + payload), flushes after each frame
  - `ContainerAttach` (POST /containers/{id}/attach) — minimal stub: hijacks connection, sends upgrade headers, closes immediately (full implementation in Phase 3)
  - Validation helpers: `validateContainerName()`, `validateContainerConfig()`, `validateHostConfig()`, `validateTimeout()`, `validateSignal()`
  - Error mapping: ErrNotFound→404, ErrNameInUse→409, ErrAlreadyRunning→304, ErrNotRunning→409, ErrContainerRunning→409
- `internal/api/handlers/respond.go` — `respondError()` maps errors to HTTP status codes via errdefs package. 500 errors don't leak messages (always "internal server error").
- `internal/daemon/daemon.go` — Startup sequence extended: extract embedded crun binary, set subreaper, create `runtime.Manager` (crun binary path, crun root dir, bundle root, store + images), call `RecoverOrphans()`, start reaper goroutine. Shutdown: graceful server shutdown (5s timeout), graceful container shutdown (10s timeout).

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

### Phase 6: Build + Remaining Image Endpoints ✅
**Goal:** `docker buildx build` works via docker-container driver. All image endpoints implemented.

**Architecture:** Modern Docker CLI (v28+) routes all builds through buildx. The docker-container driver creates and manages its own buildkitd container — no in-daemon build subprocess needed. This requires working container lifecycle (pull, create, start, exec) from earlier phases.

**Implemented:**
- `internal/api/handlers/build.go` — `ImageBuild`, `BuildPrune`, `Session` stubs return clear message directing users to the docker-container buildx driver.
- `internal/api/handlers/commit.go` — `ImageCommit` (`POST /commit`): full implementation. Creates new image from container's RW layer with config overrides (Cmd, Entrypoint, Env, ExposedPorts, Volumes, WorkingDir, Labels, User). Uses containers/storage `Container()` → `Layer()` → `CreateImage()` + manifest/config big data.
- `internal/image/commit.go` — `Store.Commit()`: clones base OCI config, applies overrides, appends new layer DiffID, creates image with manifest + config stored as big data.
- `internal/api/handlers/distribution.go` — `DistributionInspect` (`GET /distribution/{name}/json`): queries registry for manifest metadata without pulling. Supports auth via existing `ResolveAuth` infrastructure.
- `internal/image/inspect_remote.go` — `Store.DistributionInspect()`: uses containers/image to fetch manifest from registry, parses manifest lists for platform info.

**Verify:**
```bash
# Build (requires working container lifecycle for docker-container driver)
docker buildx create --driver docker-container --name kogia --use
echo 'FROM alpine\nRUN echo hi > /x\nCMD cat /x' | docker buildx build -t test --load -
docker run --rm test  # "hi"

# Commit
docker run -d --name myc alpine sleep 3600
docker commit myc myimage:v1
docker run --rm myimage:v1 cat /world.txt

# Distribution inspect
docker manifest inspect alpine
```

---

## Graceful Shutdown

```
SIGTERM/SIGINT received →
  1. Stop accepting connections (close Unix socket listener)
  2. Drain in-flight API requests (5s timeout)
  3. For each running container (parallel):
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
| **Docker API fidelity** — undocumented CLI expectations beyond swagger spec | High | Custom route generator covers all 107 spec operations; moby types ensure response format matches Docker exactly; use socat traffic capture for undocumented behavior; golden-file tests |
| **Streaming endpoints** — 4 protocols (logs, attach, pull progress, build session) | High | Import moby's stdcopy. Implement in order of difficulty. |
| **spec.go complexity** — Docker→OCI config translation is ~800 LOC of edge cases | High | Start minimal (hello-world), expand iteratively. Extensive unit tests. |
| **containers/storage edge cases** — whiteouts, opaque dirs, metacopy | Medium | Battle-tested library handles this. Trust it. |
| **bbolt single-writer** — concurrent container creates queue on DB writes | Medium | DB writes are <1ms. Real bottleneck is cgroup/namespace setup. |
| **crun fork/exec overhead** — ~15ms per operation | Low | Acceptable for target use case. In-process option (libcrun CGo) available for v2 if needed. |
