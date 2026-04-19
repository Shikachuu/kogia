package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParseTypeFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		query    string
		wantNil  bool
		wantIncl []string
		wantExcl []string
	}{
		{
			name:     "no type param returns nil filter",
			query:    "",
			wantNil:  true,
			wantIncl: []string{"container", "image", "volume", "build-cache"},
		},
		{
			name:     "single type",
			query:    "type=image",
			wantIncl: []string{"image"},
			wantExcl: []string{"container", "volume", "build-cache"},
		},
		{
			name:     "multiple types",
			query:    "type=container&type=volume",
			wantIncl: []string{"container", "volume"},
			wantExcl: []string{"image", "build-cache"},
		},
		{
			name:     "all four types",
			query:    "type=container&type=image&type=volume&type=build-cache",
			wantIncl: []string{"container", "image", "volume", "build-cache"},
		},
		{
			name:     "other query params ignored",
			query:    "foo=bar&type=image&baz=qux",
			wantIncl: []string{"image"},
			wantExcl: []string{"container", "volume"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/system/df?"+tt.query, nil)
			got := parseTypeFilter(req)

			if tt.wantNil && got != nil {
				t.Errorf("filter = %v, want nil", got)
			}

			if !tt.wantNil && got == nil {
				t.Fatal("filter is nil, want non-nil")
			}

			for _, typ := range tt.wantIncl {
				if !got.includes(typ) {
					t.Errorf("includes(%q) = false, want true", typ)
				}
			}

			for _, typ := range tt.wantExcl {
				if got.includes(typ) {
					t.Errorf("includes(%q) = true, want false", typ)
				}
			}
		})
	}
}

func TestDirSizeBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  int64
	}{
		{
			name:  "empty directory",
			setup: func(_ *testing.T, _ string) {},
			want:  0,
		},
		{
			name: "single file",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeFile(t, filepath.Join(dir, "a.txt"), 100)
			},
			want: 100,
		},
		{
			name: "multiple files",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				writeFile(t, filepath.Join(dir, "a.txt"), 100)
				writeFile(t, filepath.Join(dir, "b.txt"), 200)
			},
			want: 300,
		},
		{
			name: "nested directories",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				sub := filepath.Join(dir, "sub", "deep")

				if err := os.MkdirAll(sub, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}

				writeFile(t, filepath.Join(dir, "root.txt"), 50)
				writeFile(t, filepath.Join(sub, "nested.txt"), 75)
			},
			want: 125,
		},
		{
			name: "nonexistent path returns zero",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				// Remove the directory so the walk finds nothing.
				if err := os.Remove(dir); err != nil {
					t.Fatalf("remove: %v", err)
				}
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			tt.setup(t, dir)

			got := dirSizeBytes(dir)
			if got != tt.want {
				t.Errorf("dirSizeBytes() = %d, want %d", got, tt.want)
			}
		})
	}
}

func writeFile(t *testing.T, path string, size int) {
	t.Helper()

	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}
