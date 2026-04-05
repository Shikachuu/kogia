package image

import (
	"testing"
	"time"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	digest "github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestBuildCommitImageConfig_Basic(t *testing.T) {
	base := &dockerspec.DockerOCIImage{
		Image: imgspecv1.Image{
			Platform: imgspecv1.Platform{
				Architecture: "amd64",
				OS:           "linux",
			},
			RootFS: imgspecv1.RootFS{
				Type:    "layers",
				DiffIDs: []digest.Digest{"sha256:aaa"},
			},
			History: []imgspecv1.History{
				{CreatedBy: "base layer"},
			},
		},
		Config: dockerspec.DockerOCIImageConfig{
			ImageConfig: imgspecv1.ImageConfig{
				Cmd: []string{"/bin/sh"},
				Env: []string{"PATH=/usr/bin"},
			},
		},
	}

	now := time.Now().UTC()
	diffID := digest.Digest("sha256:bbb")

	result := buildCommitImageConfig(base, nil, "test commit", "tester", diffID, now)

	// Architecture preserved.
	if result.Architecture != "amd64" {
		t.Errorf("expected amd64, got %s", result.Architecture)
	}

	// New layer appended.
	if len(result.RootFS.DiffIDs) != 2 {
		t.Fatalf("expected 2 diff IDs, got %d", len(result.RootFS.DiffIDs))
	}

	if result.RootFS.DiffIDs[1] != diffID {
		t.Errorf("expected new diffID %s, got %s", diffID, result.RootFS.DiffIDs[1])
	}

	// History appended.
	if len(result.History) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(result.History))
	}

	if result.History[1].Comment != "test commit" {
		t.Errorf("expected comment 'test commit', got %q", result.History[1].Comment)
	}

	// Author set.
	if result.Author != "tester" {
		t.Errorf("expected author 'tester', got %q", result.Author)
	}

	// Config preserved.
	if len(result.Config.Cmd) != 1 || result.Config.Cmd[0] != "/bin/sh" {
		t.Errorf("expected Cmd [/bin/sh], got %v", result.Config.Cmd)
	}
}

func TestBuildCommitImageConfig_WithOverrides(t *testing.T) {
	base := &dockerspec.DockerOCIImage{
		Image: imgspecv1.Image{
			Platform: imgspecv1.Platform{
				Architecture: "arm64",
				OS:           "linux",
			},
			RootFS: imgspecv1.RootFS{Type: "layers"},
		},
		Config: dockerspec.DockerOCIImageConfig{
			ImageConfig: imgspecv1.ImageConfig{
				Cmd:        []string{"/bin/sh"},
				Env:        []string{"A=1"},
				WorkingDir: "/old",
				User:       "root",
			},
		},
	}

	cfg := &CommitConfig{
		Cmd:        []string{"/app"},
		Env:        []string{"A=1", "B=2"},
		WorkingDir: "/app",
		User:       "appuser",
		Labels:     map[string]string{"version": "1.0"},
	}

	now := time.Now().UTC()
	result := buildCommitImageConfig(base, cfg, "", "", digest.Digest("sha256:ccc"), now)

	if len(result.Config.Cmd) != 1 || result.Config.Cmd[0] != "/app" {
		t.Errorf("expected Cmd [/app], got %v", result.Config.Cmd)
	}

	if len(result.Config.Env) != 2 {
		t.Errorf("expected 2 env vars, got %d", len(result.Config.Env))
	}

	if result.Config.WorkingDir != "/app" {
		t.Errorf("expected WorkingDir /app, got %s", result.Config.WorkingDir)
	}

	if result.Config.User != "appuser" {
		t.Errorf("expected User appuser, got %s", result.Config.User)
	}

	if result.Config.Labels["version"] != "1.0" {
		t.Errorf("expected label version=1.0, got %v", result.Config.Labels)
	}
}

func TestBuildCommitImageConfig_BaseNotMutated(t *testing.T) {
	base := &dockerspec.DockerOCIImage{
		Image: imgspecv1.Image{
			RootFS: imgspecv1.RootFS{
				Type:    "layers",
				DiffIDs: []digest.Digest{"sha256:orig"},
			},
			History: []imgspecv1.History{
				{CreatedBy: "original"},
			},
		},
	}

	now := time.Now().UTC()
	_ = buildCommitImageConfig(base, nil, "commit", "", digest.Digest("sha256:new"), now)

	// Base should not be mutated.
	if len(base.RootFS.DiffIDs) != 1 {
		t.Errorf("base DiffIDs was mutated: %v", base.RootFS.DiffIDs)
	}

	if len(base.History) != 1 {
		t.Errorf("base History was mutated: %v", base.History)
	}
}

func TestApplyCommitOverrides_NilFields(t *testing.T) {
	cfg := &dockerspec.DockerOCIImageConfig{
		ImageConfig: imgspecv1.ImageConfig{
			Cmd:        []string{"/original"},
			WorkingDir: "/orig",
		},
	}

	// Empty CommitConfig should not change anything.
	applyCommitOverrides(cfg, &CommitConfig{})

	if cfg.Cmd[0] != "/original" {
		t.Errorf("Cmd should not change with nil override, got %v", cfg.Cmd)
	}

	if cfg.WorkingDir != "/orig" {
		t.Errorf("WorkingDir should not change with empty override, got %s", cfg.WorkingDir)
	}
}
