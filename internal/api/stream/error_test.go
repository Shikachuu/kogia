package stream

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Shikachuu/kogia/internal/api/errdefs"
)

func TestWriteError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		wantMsg string
	}{
		{
			name:    "errdefs not found",
			err:     errdefs.NotFound("container xyz not found", nil),
			wantMsg: "container xyz not found",
		},
		{
			name:    "errdefs invalid parameter",
			err:     errdefs.InvalidParameter("bad input", nil),
			wantMsg: "bad input",
		},
		{
			name:    "plain error becomes generic",
			err:     errors.New("secret database password leaked"),
			wantMsg: "internal server error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			nw := NewNDJSON(rec)

			WriteError(nw, tt.err)

			body := strings.TrimSpace(rec.Body.String())
			if body == "" {
				t.Fatal("empty response body")
			}

			var got ProgressMsg
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("invalid NDJSON: %v (body: %q)", err, body)
			}

			if got.Error != tt.wantMsg {
				t.Errorf("error = %q, want %q", got.Error, tt.wantMsg)
			}

			if got.ErrorDetail == nil {
				t.Fatal("expected non-nil errorDetail")
			}

			if got.ErrorDetail.Message != tt.wantMsg {
				t.Errorf("errorDetail.message = %q, want %q", got.ErrorDetail.Message, tt.wantMsg)
			}
		})
	}
}
