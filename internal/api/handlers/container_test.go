package handlers

import (
	"testing"
	"time"
)

func TestParseSince(t *testing.T) {
	t.Parallel()

	t.Run("unix timestamp integer", func(t *testing.T) {
		t.Parallel()

		got, err := parseSince("1700000000")
		assertNoErr(t, err)
		assertUnix(t, got, 1700000000)
	})

	t.Run("unix timestamp float", func(t *testing.T) {
		t.Parallel()

		got, err := parseSince("1700000000.123456789")
		assertNoErr(t, err)
		assertUnix(t, got, 1700000000)

		if got.Nanosecond() == 0 {
			t.Error("expected non-zero nanoseconds")
		}
	})

	t.Run("duration string", func(t *testing.T) {
		t.Parallel()

		now := time.Now()

		got, err := parseSince("5m")
		assertNoErr(t, err)

		diff := now.Sub(got)
		if diff < 4*time.Minute || diff > 6*time.Minute {
			t.Errorf("duration offset = %v, want ~5m", diff)
		}
	})

	t.Run("RFC3339", func(t *testing.T) {
		t.Parallel()

		got, err := parseSince("2024-01-15T10:30:00Z")
		assertNoErr(t, err)

		want, _ := time.Parse(time.RFC3339, "2024-01-15T10:30:00Z")
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("RFC3339Nano", func(t *testing.T) {
		t.Parallel()

		got, err := parseSince("2024-01-15T10:30:00.123456789Z")
		assertNoErr(t, err)

		if got.Nanosecond() != 123456789 {
			t.Errorf("nanosecond = %d, want 123456789", got.Nanosecond())
		}
	})

	t.Run("invalid input", func(t *testing.T) {
		t.Parallel()

		_, err := parseSince("not-a-time")
		if err == nil {
			t.Fatal("expected error for invalid input")
		}
	})
}

func assertNoErr(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertUnix(t *testing.T, got time.Time, wantSec int64) {
	t.Helper()

	if got.Unix() != wantSec {
		t.Errorf("unix = %d, want %d", got.Unix(), wantSec)
	}
}
