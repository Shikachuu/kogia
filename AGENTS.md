# Kogia

Lightweight Docker-compatible container runtime daemon in Go. Exposes the Docker REST API on a Unix socket so `docker` CLI, `docker compose`, and `docker buildx` work unmodified. Target: ~30-50 MB steady-state RSS.

Full specification: [`docs/SPEC.md`](docs/SPEC.md)

## Tooling

[mise](https://mise.jdx.dev) manages tools and tasks (`mise.toml`).

| Task | Command |
|------|---------|
| Build | `mise run build` |
| Test | `mise run test` |
| Lint | `mise run lint` |
| Regenerate API code | `mise run gogen` |
| Security scan | `mise run secu-scan` |

- **golangci-lint** — config at `.golangci-lint.yml`
- **goreleaser** — release builds
- **trivy** — vulnerability and secret scanning

## Architecture

```
docker CLI → REST/JSON over Unix socket → kogia daemon → crun (fork/exec) → containers
```

| Package | Responsibility |
|---------|---------------|
| `internal/daemon/` | Startup orchestrator, signal handling, shutdown sequence |
| `internal/api/gen/` | **GENERATED** — go-swagger from Docker's Swagger 2.0 spec. Never edit manually. |
| `internal/api/handlers/` | Handler implementations wired to generated interfaces |
| `internal/api/server.go` | HTTP server, middleware, Unix socket listener |
| `internal/runtime/` | crun lifecycle (create/start/stop/kill/remove), OCI spec generation, stdio, supervision |
| `internal/image/` | Pull, store, auth — via containers/image + containers/storage |
| `internal/network/` | Bridge networking (netlink), NAT (nftables), DNS (miekg/dns), IPAM, CNI |
| `internal/store/` | bbolt state persistence |
| `internal/volume/` | Named volume management |
| `internal/events/` | Event fan-out bus (container/image/network/volume lifecycle) |
| `internal/metrics/` | Opt-in Prometheus metrics |
| `cmd/kogia/` | CLI entry point (cobra) |
| `cmd/kogia-cni/` | Standalone CNI plugin binary |

## Key Architectural Decisions

- **OCI runtime:** crun via fork/exec — no daemon, no per-container shim overhead
- **API types:** go-swagger generated from Docker's Swagger 2.0 spec — compile-time handler contracts; missing endpoints won't compile
- **Streaming:** moby stdcopy imported for attach/logs/exec wire format (not captured in Swagger)
- **Image stack:** containers/image + containers/storage — Podman-grade, handles all registry/layer/overlayfs complexity
- **State:** bbolt — pure Go, single-writer, no CGo
- **Networking:** in-process netlink + nftables + miekg/dns — no external CNI daemon
- **Error handling:** errdefs + SafeError pattern — see [`docs/error-handling.md`](docs/error-handling.md)
- **Logging:** `log/slog` (stdlib structured JSON)
- **No CGo:** entire codebase builds with `CGO_ENABLED=0`

## Validation Rules

Before committing, these must pass:

```bash
mise run lint   # golangci-lint with .golangci-lint.yml
mise run test   # go test ./... -cover
```

- **Never edit** `internal/api/gen/` — it is generated. To regenerate: `mise run gogen`
- **No CGo** — all code must compile with `CGO_ENABLED=0`
- Lint rules are enforced by `.golangci-lint.yml`; lint must pass clean
