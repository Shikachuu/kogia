// Package errdefs provides typed error wrappers that map to HTTP status codes.
//
// Each error type separates a client-safe message from the internal error cause.
// The .Error() method returns only the safe message; .InternalError() returns
// the full error chain for logging. Unwrap() returns nil to prevent callers
// from traversing past the trust boundary into library internals.
//
// All error types implement the [SafeError] interface, which embeds the
// standard error interface and adds InternalError() for structured logging.
//
// Inspired by Docker's moby/errdefs (type-based HTTP mapping) combined with
// JetBrains' SafeError pattern (public/internal message split).
package errdefs

import (
	"errors"
	"net/http"
)

// SafeError is the contract for all errdefs error types. It embeds the
// standard error interface (providing a client-safe message via Error())
// and adds InternalError() for extracting the full internal cause for logging.
type SafeError interface {
	error
	// InternalError returns the internal error cause for logging.
	// This MUST NOT be sent to clients.
	InternalError() error
}

// Compile-time assertions that all error types implement SafeError.
var (
	_ SafeError = (*InvalidParameterError)(nil)
	_ SafeError = (*NotFoundError)(nil)
	_ SafeError = (*ConflictError)(nil)
)

// InvalidParameterError represents a client input validation failure (→ 400).
type InvalidParameterError struct {
	Internal error
	Msg      string
}

func (e *InvalidParameterError) Error() string        { return e.Msg }
func (e *InvalidParameterError) Unwrap() error        { return nil }
func (e *InvalidParameterError) InternalError() error { return e.Internal }

// NotFoundError represents a missing resource (→ 404).
type NotFoundError struct {
	Internal error
	Msg      string
}

func (e *NotFoundError) Error() string        { return e.Msg }
func (e *NotFoundError) Unwrap() error        { return nil }
func (e *NotFoundError) InternalError() error { return e.Internal }

// ConflictError represents a state conflict (→ 409).
type ConflictError struct {
	Internal error
	Msg      string
}

func (e *ConflictError) Error() string        { return e.Msg }
func (e *ConflictError) Unwrap() error        { return nil }
func (e *ConflictError) InternalError() error { return e.Internal }

// InvalidParameter creates a 400 error with a safe message and an internal cause.
func InvalidParameter(msg string, internal error) error {
	return &InvalidParameterError{Msg: msg, Internal: internal}
}

// NotFound creates a 404 error with a safe message and an internal cause.
func NotFound(msg string, internal error) error {
	return &NotFoundError{Msg: msg, Internal: internal}
}

// Conflict creates a 409 error with a safe message and an internal cause.
func Conflict(msg string, internal error) error {
	return &ConflictError{Msg: msg, Internal: internal}
}

// IsInvalidParameter reports whether err is an InvalidParameterError.
func IsInvalidParameter(err error) bool {
	var target *InvalidParameterError

	return errors.As(err, &target)
}

// IsNotFound reports whether err is a NotFoundError.
func IsNotFound(err error) bool {
	var target *NotFoundError

	return errors.As(err, &target)
}

// IsConflict reports whether err is a ConflictError.
func IsConflict(err error) bool {
	var target *ConflictError

	return errors.As(err, &target)
}

// StatusCode maps an error to the appropriate HTTP status code.
// Unrecognized errors default to 500 Internal Server Error.
func StatusCode(err error) int {
	switch {
	case IsInvalidParameter(err):
		return http.StatusBadRequest
	case IsNotFound(err):
		return http.StatusNotFound
	case IsConflict(err):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// SafeMessage returns the client-safe message from a [SafeError], traversing
// any wrapping (e.g. fmt.Errorf). If the error does not implement SafeError,
// the raw err.Error() is returned — callers handling 500s should substitute
// a generic message before sending to clients.
func SafeMessage(err error) string {
	var se SafeError
	if errors.As(err, &se) {
		return se.Error()
	}

	return err.Error()
}

// InternalErr extracts the internal error from a [SafeError], if present.
// Returns nil if the error does not implement SafeError or has no internal cause.
func InternalErr(err error) error {
	var se SafeError
	if errors.As(err, &se) {
		return se.InternalError()
	}

	return nil
}
