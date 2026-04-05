# Error Handling

Kogia uses a combined errdefs + SafeError pattern for API error handling. This separates client-safe messages from internal error details, preventing accidental leakage of database schemas, file paths, or other internals.

## The SafeError interface

All API-boundary errors implement `errdefs.SafeError`:

```go
type SafeError interface {
    error
    InternalError() error
}
```

- `Error()` — returns the **client-safe message** (sent in HTTP response)
- `InternalError()` — returns the **internal cause** (logged, never sent to clients)
- `Unwrap()` — returns `nil` (opaque boundary — `errors.Is`/`errors.As` cannot traverse past it)

## Error types

| Type | HTTP Status | When to use |
|------|-------------|-------------|
| `errdefs.InvalidParameter` | 400 | Validation failures, bad input |
| `errdefs.NotFound` | 404 | Missing containers, images, networks |
| `errdefs.Conflict` | 409 | Name collisions, state conflicts |
| Plain `error` | 500 | Internal failures (message replaced with "internal server error") |

## Creating errors

```go
// Validation error — client sees "memory limit must be at least 6MB"
errdefs.InvalidParameter("memory limit must be at least 6MB", err)

// Not found — client sees "no such container: abc123"
errdefs.NotFound("no such container: abc123", err)

// Conflict — client sees "container name already in use"
errdefs.Conflict("container name already in use", err)
```

The second argument is the internal cause for logging. Pass `nil` if there's no internal error.

## Compile-time enforcement

Every error type has a compile-time assertion:

```go
var _ SafeError = (*InvalidParameterError)(nil)
var _ SafeError = (*NotFoundError)(nil)
var _ SafeError = (*ConflictError)(nil)
```

Adding a new error type without implementing `SafeError` will fail compilation.

## Handler pattern

Handlers use `respondError(w, err)` which automatically:
1. Maps the error type to the correct HTTP status code
2. Logs the internal cause via `slog`
3. Sends only the safe message to the client
4. For plain errors (500), replaces the message with "internal server error"

```go
func (h *Handlers) ContainerCreate(w http.ResponseWriter, r *http.Request) {
    // Validate input — returns errdefs.InvalidParameter on failure.
    if err := validateContainerConfig(req.Config); err != nil {
        respondError(w, err)
        return
    }

    // Call runtime — wrap errors at the boundary.
    id, err := h.runtime.Create(ctx, cfg, hostCfg, name)
    if err != nil {
        switch {
        case errors.Is(err, store.ErrNameInUse):
            respondError(w, errdefs.Conflict("container name already in use", err))
        case errors.Is(err, image.ErrNotFound):
            respondError(w, errdefs.NotFound("no such image: "+cfg.Image, err))
        default:
            respondError(w, err) // 500 — safe message, internal logged
        }
        return
    }
}
```

## Rules

1. **Never send internal errors to clients.** Always wrap with `errdefs.*` at the handler boundary.
2. **Validate early.** Use `validate*.go` functions before calling runtime/store methods.
3. **One wrap per boundary.** Don't double-wrap — wrap once at the handler level.
4. **Use sentinel errors in runtime/store.** Handlers check with `errors.Is()` and translate to errdefs.
5. **Plain errors = 500.** If an error reaches `respondError` without an errdefs wrapper, the client gets "internal server error" and the real error is logged.
