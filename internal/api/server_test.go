package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestEncodeSlashyPathParams(t *testing.T) {
	t.Parallel()

	basePath := "/v1.54"

	tests := []struct {
		name     string
		path     string
		wantPath string
	}{
		{
			name:     "simple image name unchanged",
			path:     "/v1.54/images/alpine/json",
			wantPath: "/v1.54/images/alpine/json",
		},
		{
			name:     "image name with registry and namespace",
			path:     "/v1.54/images/docker.io/library/alpine/json",
			wantPath: "/v1.54/images/docker.io%2Flibrary%2Falpine/json",
		},
		{
			name:     "image delete with slashes",
			path:     "/v1.54/images/docker.io/library/alpine",
			wantPath: "/v1.54/images/docker.io%2Flibrary%2Falpine",
		},
		{
			name:     "image tag with slashes",
			path:     "/v1.54/images/docker.io/library/alpine/tag",
			wantPath: "/v1.54/images/docker.io%2Flibrary%2Falpine/tag",
		},
		{
			name:     "image history with slashes",
			path:     "/v1.54/images/docker.io/library/alpine/history",
			wantPath: "/v1.54/images/docker.io%2Flibrary%2Falpine/history",
		},
		{
			name:     "image push with slashes",
			path:     "/v1.54/images/ghcr.io/myorg/myimage/push",
			wantPath: "/v1.54/images/ghcr.io%2Fmyorg%2Fmyimage/push",
		},
		{
			name:     "image get (export) with slashes",
			path:     "/v1.54/images/docker.io/library/alpine/get",
			wantPath: "/v1.54/images/docker.io%2Flibrary%2Falpine/get",
		},
		{
			name:     "short image name no slashes",
			path:     "/v1.54/images/alpine",
			wantPath: "/v1.54/images/alpine",
		},
		{
			name:     "image list unaffected",
			path:     "/v1.54/images/json",
			wantPath: "/v1.54/images/json",
		},
		{
			name:     "container path unaffected",
			path:     "/v1.54/containers/abc123/json",
			wantPath: "/v1.54/containers/abc123/json",
		},
		{
			name:     "distribution with slashes",
			path:     "/v1.54/distribution/docker.io/library/alpine/json",
			wantPath: "/v1.54/distribution/docker.io%2Flibrary%2Falpine/json",
		},
		{
			name:     "plugin with slashes",
			path:     "/v1.54/plugins/registry.example.com/myplugin/json",
			wantPath: "/v1.54/plugins/registry.example.com%2Fmyplugin/json",
		},
		{
			name:     "plugin enable with slashes",
			path:     "/v1.54/plugins/registry.example.com/myplugin/enable",
			wantPath: "/v1.54/plugins/registry.example.com%2Fmyplugin/enable",
		},
		{
			name:     "sha256 digest as name",
			path:     "/v1.54/images/sha256:abcdef123456/json",
			wantPath: "/v1.54/images/sha256:abcdef123456/json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := &http.Request{URL: mustParseURL(tt.path)}
			encodeSlashyPathParams(r, basePath)

			if r.URL.Path != tt.wantPath {
				t.Errorf("path = %q, want %q", r.URL.Path, tt.wantPath)
			}
		})
	}
}

func TestRewriteVersionPrefix(t *testing.T) {
	t.Parallel()

	basePath := "/v1.54"

	tests := []struct {
		name     string
		path     string
		wantPath string
	}{
		{
			name:     "rewrite older version",
			path:     "/v1.45/containers/json",
			wantPath: "/v1.54/containers/json",
		},
		{
			name:     "rewrite newer version",
			path:     "/v1.99/containers/json",
			wantPath: "/v1.54/containers/json",
		},
		{
			name:     "bare ping",
			path:     "/_ping",
			wantPath: "/v1.54/_ping",
		},
		{
			name:     "same version unchanged",
			path:     "/v1.54/images/json",
			wantPath: "/v1.54/images/json",
		},
		{
			name:     "no version prefix unchanged",
			path:     "/other/path",
			wantPath: "/other/path",
		},
		{
			name:     "version only no trailing path",
			path:     "/v1.45",
			wantPath: "/v1.54/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := &http.Request{URL: mustParseURL(tt.path)}
			rewriteVersionPrefix(r, basePath)

			if r.URL.Path != tt.wantPath {
				t.Errorf("path = %q, want %q", r.URL.Path, tt.wantPath)
			}
		})
	}
}

func TestWithMiddleware(t *testing.T) {
	t.Parallel()

	basePath := "/v1.54"

	t.Run("request logging and status capture", func(t *testing.T) {
		t.Parallel()

		var capturedPath string

		inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			capturedPath = r.URL.Path
		})

		handler := withMiddleware(inner, basePath)
		req := httptest.NewRequest(http.MethodGet, "/v1.45/containers/json", http.NoBody)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		// Version prefix should be rewritten before reaching the inner handler.
		if capturedPath != "/v1.54/containers/json" {
			t.Errorf("inner handler saw path %q, want %q", capturedPath, "/v1.54/containers/json")
		}

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
		}
	})

	t.Run("version rewrite and slashy encoding combined", func(t *testing.T) {
		t.Parallel()

		var capturedPath string

		inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			capturedPath = r.URL.Path
		})

		handler := withMiddleware(inner, basePath)
		req := httptest.NewRequest(http.MethodDelete, "/v1.47/images/docker.io/library/alpine", http.NoBody)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		want := "/v1.54/images/docker.io%2Flibrary%2Falpine"
		if capturedPath != want {
			t.Errorf("inner handler saw path %q, want %q", capturedPath, want)
		}
	})

	t.Run("panic recovery returns 500 JSON", func(t *testing.T) {
		t.Parallel()

		inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			panic("test panic")
		})

		handler := withMiddleware(inner, basePath)
		req := httptest.NewRequest(http.MethodGet, "/v1.54/_ping", http.NoBody)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}

		body, _ := io.ReadAll(rec.Body)

		var errResp map[string]string
		if err := json.Unmarshal(body, &errResp); err != nil {
			t.Fatalf("response is not valid JSON: %v (body: %s)", err, body)
		}

		if errResp["message"] != "internal server error" {
			t.Errorf("message = %q, want %q", errResp["message"], "internal server error")
		}
	})

	t.Run("handler status code is captured", func(t *testing.T) {
		t.Parallel()

		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})

		handler := withMiddleware(inner, basePath)
		req := httptest.NewRequest(http.MethodGet, "/v1.54/nonexistent", http.NoBody)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})
}

func mustParseURL(path string) *url.URL {
	return &url.URL{Path: path}
}
