package runtime

import (
	"testing"

	"github.com/moby/moby/api/types/container"
)

// shouldRestart extracts the restart decision logic for testability.
// It mirrors the logic in handleRestartPolicy.
func shouldRestart(policy container.RestartPolicy, exitCode, restartCount int, manuallyStopped bool) bool {
	switch policy.Name {
	case container.RestartPolicyAlways:
		return !manuallyStopped
	case container.RestartPolicyOnFailure:
		return exitCode != 0 && !manuallyStopped &&
			(policy.MaximumRetryCount <= 0 || restartCount < policy.MaximumRetryCount)
	case container.RestartPolicyUnlessStopped:
		return !manuallyStopped
	default:
		return false
	}
}

func TestShouldRestart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		policy          container.RestartPolicy
		exitCode        int
		restartCount    int
		manuallyStopped bool
		want            bool
	}{
		// -- no policy --
		{
			name:   "no policy, exit 0",
			policy: container.RestartPolicy{Name: ""},
			want:   false,
		},
		{
			name:   "no policy, exit 1",
			policy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
			want:   false,
		},

		// -- always --
		{
			name:     "always, exit 0",
			policy:   container.RestartPolicy{Name: container.RestartPolicyAlways},
			exitCode: 0,
			want:     true,
		},
		{
			name:     "always, exit 1",
			policy:   container.RestartPolicy{Name: container.RestartPolicyAlways},
			exitCode: 1,
			want:     true,
		},
		{
			name:            "always, manually stopped",
			policy:          container.RestartPolicy{Name: container.RestartPolicyAlways},
			exitCode:        0,
			manuallyStopped: true,
			want:            false,
		},

		// -- on-failure --
		{
			name:     "on-failure, exit 0 (success)",
			policy:   container.RestartPolicy{Name: container.RestartPolicyOnFailure},
			exitCode: 0,
			want:     false,
		},
		{
			name:     "on-failure, exit 1",
			policy:   container.RestartPolicy{Name: container.RestartPolicyOnFailure},
			exitCode: 1,
			want:     true,
		},
		{
			name:     "on-failure, exit 137 (killed)",
			policy:   container.RestartPolicy{Name: container.RestartPolicyOnFailure},
			exitCode: 137,
			want:     true,
		},
		{
			name:            "on-failure, exit 1, manually stopped",
			policy:          container.RestartPolicy{Name: container.RestartPolicyOnFailure},
			exitCode:        1,
			manuallyStopped: true,
			want:            false,
		},
		{
			name:         "on-failure, max retries not reached",
			policy:       container.RestartPolicy{Name: container.RestartPolicyOnFailure, MaximumRetryCount: 3},
			exitCode:     1,
			restartCount: 1,
			want:         true,
		},
		{
			name:         "on-failure, max retries reached",
			policy:       container.RestartPolicy{Name: container.RestartPolicyOnFailure, MaximumRetryCount: 3},
			exitCode:     1,
			restartCount: 3,
			want:         false,
		},
		{
			name:         "on-failure, max retries exceeded",
			policy:       container.RestartPolicy{Name: container.RestartPolicyOnFailure, MaximumRetryCount: 3},
			exitCode:     1,
			restartCount: 5,
			want:         false,
		},
		{
			name:     "on-failure, unlimited retries (max=0)",
			policy:   container.RestartPolicy{Name: container.RestartPolicyOnFailure, MaximumRetryCount: 0},
			exitCode: 1,
			want:     true,
		},

		// -- unless-stopped --
		{
			name:     "unless-stopped, exit 0",
			policy:   container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			exitCode: 0,
			want:     true,
		},
		{
			name:     "unless-stopped, exit 1",
			policy:   container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			exitCode: 1,
			want:     true,
		},
		{
			name:            "unless-stopped, manually stopped — MUST NOT restart",
			policy:          container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			exitCode:        0,
			manuallyStopped: true,
			want:            false,
		},
		{
			name:            "unless-stopped, exit 1, manually stopped",
			policy:          container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			exitCode:        1,
			manuallyStopped: true,
			want:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := shouldRestart(tt.policy, tt.exitCode, tt.restartCount, tt.manuallyStopped)
			if got != tt.want {
				t.Errorf("shouldRestart() = %v, want %v", got, tt.want)
			}
		})
	}
}
