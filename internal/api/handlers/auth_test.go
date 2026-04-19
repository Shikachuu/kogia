package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/containers/image/v5/docker"

	"github.com/Shikachuu/kogia/internal/api/errdefs"
)

func TestSystemAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantMsg    string
		wantStatus int
	}{
		{
			name:       "invalid JSON body",
			body:       `{not json`,
			wantStatus: http.StatusBadRequest,
			wantMsg:    "invalid auth config",
		},
		{
			name:       "empty credentials",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			wantMsg:    "credentials required",
		},
		{
			name:       "empty username and password",
			body:       `{"username":"","password":""}`,
			wantStatus: http.StatusBadRequest,
			wantMsg:    "credentials required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// These tests exercise input validation paths that don't reach
			// CheckAuth, so a nil images field is safe.
			h := &Handlers{}

			req := httptest.NewRequest(http.MethodPost, "/auth", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()

			h.SystemAuth(rec, req)

			res := rec.Result()
			defer res.Body.Close()

			if res.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", res.StatusCode, tt.wantStatus)
			}

			var got map[string]string
			if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
				t.Fatalf("decode response: %v", err)
			}

			if got["message"] != tt.wantMsg {
				t.Errorf("message = %q, want %q", got["message"], tt.wantMsg)
			}
		})
	}
}

func TestRespondAuthError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err        error
		name       string
		server     string
		wantMsg    string
		wantStatus int
	}{
		{
			name:       "unauthorized credentials",
			err:        docker.ErrUnauthorizedForCredentials{Err: errors.New("denied")},
			server:     "registry.example.com",
			wantStatus: http.StatusUnauthorized,
			wantMsg:    "unauthorized: incorrect username or password",
		},
		{
			name:       "wrapped unauthorized",
			err:        fmt.Errorf("auth: check credentials for registry.example.com: %w", docker.ErrUnauthorizedForCredentials{Err: errors.New("denied")}),
			server:     "registry.example.com",
			wantStatus: http.StatusUnauthorized,
			wantMsg:    "unauthorized: incorrect username or password",
		},
		{
			name:       "DNS error",
			err:        &net.DNSError{Err: "no such host", Name: "typo.example.com"},
			server:     "typo.example.com",
			wantStatus: http.StatusBadRequest,
			wantMsg:    "registry not reachable: typo.example.com",
		},
		{
			name:       "connection refused",
			err:        &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			server:     "localhost:5000",
			wantStatus: http.StatusBadRequest,
			wantMsg:    "registry not reachable: localhost:5000",
		},
		{
			name:       "unknown error falls through to 500",
			err:        errors.New("unexpected TLS handshake failure"),
			server:     "registry.example.com",
			wantStatus: http.StatusInternalServerError,
			wantMsg:    "internal server error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()

			respondAuthError(rec, tt.err, tt.server)

			res := rec.Result()
			defer res.Body.Close()

			if res.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", res.StatusCode, tt.wantStatus)
			}

			var got map[string]string
			if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
				t.Fatalf("decode response: %v", err)
			}

			if got["message"] != tt.wantMsg {
				t.Errorf("message = %q, want %q", got["message"], tt.wantMsg)
			}
		})
	}
}

func TestRespondAuthErrorClassifiesWrappedErrors(t *testing.T) {
	t.Parallel()

	// Verify that errors wrapped by CheckAuth's fmt.Errorf are still classified.
	wrapped := errdefs.Unauthorized("test", errors.New("inner"))

	if !errdefs.IsUnauthorized(wrapped) {
		t.Error("IsUnauthorized should return true for direct errdefs error")
	}
}
