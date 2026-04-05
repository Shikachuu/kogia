# kogia

`kogia` is a minimal, memory-efficient Docker Engine API-compatible container runtime daemon in Go.

Instead of reimplementing compose/build logic, kogia exposes the Docker REST API on a Unix socket so the official `docker` CLI, `docker compose`, and `docker buildx` work unmodified.

Theoretically it has a steady around ~50MB RSS baseline and with 50ish running containers.

It is named after the smallest whale species `kogia sima`.
