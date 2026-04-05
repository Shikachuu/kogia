package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRespondJSON(t *testing.T) {
	t.Parallel()

	t.Run("struct body", func(t *testing.T) {
		t.Parallel()

		type payload struct {
			Name string `json:"name"`
			Age  int    `json:"age"`
		}

		rec := httptest.NewRecorder()
		respondJSON(rec, http.StatusOK, payload{Name: "alice", Age: 30})

		res := rec.Result()
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want %d", res.StatusCode, http.StatusOK)
		}

		if ct := res.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want %q", ct, "application/json")
		}

		var got payload
		if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if got.Name != "alice" || got.Age != 30 {
			t.Errorf("body = %+v, want {alice 30}", got)
		}
	})

	t.Run("slice body", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		respondJSON(rec, http.StatusOK, []string{"a", "b"})

		res := rec.Result()
		defer res.Body.Close()

		var got []string
		if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Errorf("body = %v, want [a b]", got)
		}
	})

	t.Run("custom status code", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		respondJSON(rec, http.StatusCreated, map[string]string{"id": "abc"})

		if rec.Code != http.StatusCreated {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
		}
	})

	t.Run("empty slice not null", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		respondJSON(rec, http.StatusOK, []string{})

		res := rec.Result()
		defer res.Body.Close()

		body, _ := io.ReadAll(res.Body)

		// Should be "[]" not "null".
		if string(body) != "[]\n" {
			t.Errorf("body = %q, want %q", string(body), "[]\n")
		}
	})
}

func TestErrorJSON(t *testing.T) {
	t.Parallel()

	t.Run("error message in JSON body", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		errorJSON(rec, http.StatusNotFound, errors.New("no such image: alpine"))

		res := rec.Result()
		defer res.Body.Close()

		if res.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want %d", res.StatusCode, http.StatusNotFound)
		}

		if ct := res.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want %q", ct, "application/json")
		}

		var got map[string]string
		if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if got["message"] != "no such image: alpine" {
			t.Errorf("message = %q, want %q", got["message"], "no such image: alpine")
		}
	})

	t.Run("500 internal server error", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		errorJSON(rec, http.StatusInternalServerError, errors.New("database locked"))

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})

	t.Run("400 bad request", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		errorJSON(rec, http.StatusBadRequest, errors.New("fromImage is required"))

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}

		var got map[string]string

		res := rec.Result()
		defer res.Body.Close()

		if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if got["message"] != "fromImage is required" {
			t.Errorf("message = %q, want %q", got["message"], "fromImage is required")
		}
	})
}

func TestPathValue(t *testing.T) {
	t.Parallel()

	// To test pathValue we need a real ServeMux that sets path values.
	// We register a route with a {name} wildcard and make requests against it.
	mux := http.NewServeMux()

	var captured string

	mux.HandleFunc("GET /test/{name}", func(_ http.ResponseWriter, r *http.Request) {
		captured = pathValue(r, "name")
	})

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "simple name",
			path: "/test/alpine",
			want: "alpine",
		},
		{
			name: "URL-encoded slashes decoded",
			path: "/test/docker.io%2Flibrary%2Falpine",
			want: "docker.io/library/alpine",
		},
		{
			name: "name with colon",
			path: "/test/sha256:abcdef",
			want: "sha256:abcdef",
		},
		{
			name: "name with tag",
			path: "/test/alpine:latest",
			want: "alpine:latest",
		},
		{
			name: "double encoded stays single decoded",
			path: "/test/ghcr.io%2Fmyorg%2Fmyimage",
			want: "ghcr.io/myorg/myimage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, tt.path, http.NoBody)
			rec := httptest.NewRecorder()

			captured = ""

			mux.ServeHTTP(rec, req)

			if captured != tt.want {
				t.Errorf("pathValue = %q, want %q", captured, tt.want)
			}
		})
	}
}
