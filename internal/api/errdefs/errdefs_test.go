package errdefs

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestInvalidParameter(t *testing.T) {
	t.Parallel()

	internal := fmt.Errorf("column users.email violates constraint: %w", errors.New("db error"))
	err := InvalidParameter("invalid email format", internal)

	if err.Error() != "invalid email format" {
		t.Errorf("Error() = %q, want %q", err.Error(), "invalid email format")
	}

	if !IsInvalidParameter(err) {
		t.Error("IsInvalidParameter should return true")
	}

	if IsNotFound(err) || IsConflict(err) {
		t.Error("should not match other types")
	}

	if StatusCode(err) != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want %d", StatusCode(err), http.StatusBadRequest)
	}

	if !errors.Is(InternalErr(err), internal) {
		t.Errorf("InternalError = %v, want %v", InternalErr(err), internal)
	}
}

func TestNotFound(t *testing.T) {
	t.Parallel()

	internal := errors.New("store: key not found")
	err := NotFound("no such container: abc123", internal)

	if err.Error() != "no such container: abc123" {
		t.Errorf("Error() = %q", err.Error())
	}

	if !IsNotFound(err) {
		t.Error("IsNotFound should return true")
	}

	if StatusCode(err) != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", StatusCode(err), http.StatusNotFound)
	}
}

func TestConflict(t *testing.T) {
	t.Parallel()

	err := Conflict("container name already in use", errors.New("bbolt: key exists"))

	if err.Error() != "container name already in use" {
		t.Errorf("Error() = %q", err.Error())
	}

	if !IsConflict(err) {
		t.Error("IsConflict should return true")
	}

	if StatusCode(err) != http.StatusConflict {
		t.Errorf("StatusCode = %d, want %d", StatusCode(err), http.StatusConflict)
	}
}

func TestUnauthorized(t *testing.T) {
	t.Parallel()

	internal := errors.New("registry: invalid credentials")
	err := Unauthorized("unauthorized: incorrect username or password", internal)

	if err.Error() != "unauthorized: incorrect username or password" {
		t.Errorf("Error() = %q, want %q", err.Error(), "unauthorized: incorrect username or password")
	}

	if !IsUnauthorized(err) {
		t.Error("IsUnauthorized should return true")
	}

	if IsInvalidParameter(err) || IsNotFound(err) || IsConflict(err) {
		t.Error("should not match other types")
	}

	if StatusCode(err) != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", StatusCode(err), http.StatusUnauthorized)
	}

	if !errors.Is(InternalErr(err), internal) {
		t.Errorf("InternalError = %v, want %v", InternalErr(err), internal)
	}
}

func TestOpaqueBoundary(t *testing.T) {
	t.Parallel()

	dbErr := errors.New("database: connection refused")
	err := NotFound("resource not found", dbErr)

	// Unwrap returns nil — callers cannot traverse past the boundary.
	if errors.Unwrap(err) != nil {
		t.Error("Unwrap should return nil (opaque boundary)")
	}

	// errors.Is should NOT find the internal error through the chain.
	if errors.Is(err, dbErr) {
		t.Error("errors.Is should not traverse past opaque boundary")
	}
}

func TestNilInternal(t *testing.T) {
	t.Parallel()

	err := InvalidParameter("bad input", nil)

	if err.Error() != "bad input" {
		t.Errorf("Error() = %q", err.Error())
	}

	if InternalErr(err) != nil {
		t.Error("InternalError should be nil when no internal cause")
	}
}

func TestStatusCodeDefault(t *testing.T) {
	t.Parallel()

	// Plain errors default to 500.
	err := errors.New("something broke")
	if StatusCode(err) != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want %d", StatusCode(err), http.StatusInternalServerError)
	}
}

func TestInternalErrOnPlainError(t *testing.T) {
	t.Parallel()

	err := errors.New("plain error")
	if InternalErr(err) != nil {
		t.Error("InternalErr should return nil for plain errors")
	}
}

func TestSafeMessage(t *testing.T) {
	t.Parallel()

	t.Run("errdefs error", func(t *testing.T) {
		t.Parallel()

		err := InvalidParameter("bad input", errors.New("db: constraint"))
		if SafeMessage(err) != "bad input" {
			t.Errorf("SafeMessage = %q, want %q", SafeMessage(err), "bad input")
		}
	})

	t.Run("wrapped errdefs error", func(t *testing.T) {
		t.Parallel()

		inner := NotFound("no such container: abc", errors.New("store: key not found"))
		err := fmt.Errorf("validate: %w", inner)

		if SafeMessage(err) != "no such container: abc" {
			t.Errorf("SafeMessage = %q, want %q", SafeMessage(err), "no such container: abc")
		}
	})

	t.Run("plain error", func(t *testing.T) {
		t.Parallel()

		err := errors.New("something broke")
		if SafeMessage(err) != "something broke" {
			t.Errorf("SafeMessage = %q, want %q", SafeMessage(err), "something broke")
		}
	})
}

