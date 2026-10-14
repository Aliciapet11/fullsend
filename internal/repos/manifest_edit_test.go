package repos

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/forge"
)

func TestAddToManifest_Basic(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "repos.yaml")

	manifest := testManifest("acme/existing")
	data, err := MarshalWithHeader(manifest)
	if err != nil {
		t.Fatalf("MarshalWithHeader() error = %v", err)
	}
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	result, updated, err := AddToManifest(context.Background(), ManifestEditConfig{
		Manifest:     manifest,
		ManifestPath: manifestPath,
	}, ForgeGitHub, []RepoEntry{{Name: "acme/new-repo"}}, nil, nil)

	if err != nil {
		t.Fatalf("AddToManifest() error = %v", err)
	}
	if len(result.Added) != 1 || result.Added[0] != "acme/new-repo" {
		t.Errorf("Added = %v, want [acme/new-repo]", result.Added)
	}
	if len(result.Skipped) != 0 {
		t.Errorf("Skipped = %v, want []", result.Skipped)
	}
	if len(updated.AllRepos()) != 2 {
		t.Errorf("manifest has %d repos, want 2", len(updated.AllRepos()))
	}

	// Verify file was written.
	reloaded, err := LoadManifest(context.Background(), manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	if len(reloaded.AllRepos()) != 2 {
		t.Errorf("reloaded manifest has %d repos, want 2", len(reloaded.AllRepos()))
	}
}

func TestUpdateAppSet_ExistingAndGlobEntries(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "repos.yaml")
	manifest := &Manifest{
		Version: 1,
		GitHub: &PlatformConfig{Repos: []RepoEntry{
			{Name: "acme/existing"},
			{Name: "acme/*", AppSet: "glob-set"},
		}},
	}
	data, err := MarshalWithHeader(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, data, 0o644))

	updated, err := UpdateAppSet(ManifestEditConfig{
		Manifest:     manifest,
		ManifestPath: manifestPath,
	}, []string{"acme/api"}, "custom-set")
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/api"}, updated)
	assert.Equal(t, "glob-set", manifest.GitHub.Repos[1].AppSet)

	reloaded, err := LoadManifest(context.Background(), manifestPath)
	require.NoError(t, err)
	var found bool
	for _, entry := range reloaded.GitHub.Repos {
		if entry.Name == "acme/api" {
			found = true
			assert.Equal(t, "custom-set", entry.AppSet)
		}
	}
	assert.True(t, found)
}

func TestUpdateAppSet_ExactGlobAndAllFilters(t *testing.T) {
	tests := []struct {
		name       string
		filters    []string
		wantExact  string
		wantGlob   string
		wantSecond string
	}{
		{name: "exact", filters: []string{"acme/existing"}, wantExact: "custom-set", wantGlob: "glob-set"},
		{name: "glob", filters: []string{"acme/*"}, wantExact: "custom-set", wantGlob: "custom-set"},
		{name: "all", wantExact: "custom-set", wantGlob: "custom-set", wantSecond: "custom-set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := &Manifest{
				Version: 1,
				GitHub: &PlatformConfig{Repos: []RepoEntry{
					{Name: "acme/existing"},
					{Name: "acme/*", AppSet: "glob-set"},
					{Name: "other/repo"},
				}},
			}
			updated, err := UpdateAppSet(ManifestEditConfig{Manifest: manifest}, tt.filters, "custom-set")
			require.NoError(t, err)
			assert.NotEmpty(t, updated)
			assert.Equal(t, tt.wantExact, manifest.GitHub.Repos[0].AppSet)
			assert.Equal(t, tt.wantGlob, manifest.GitHub.Repos[1].AppSet)
			assert.Equal(t, tt.wantSecond, manifest.GitHub.Repos[2].AppSet)
		})
	}
}

func TestUpdateAppSet_InvalidFilter(t *testing.T) {
	manifest := &Manifest{Version: 1, GitHub: &PlatformConfig{Repos: []RepoEntry{{Name: "acme/repo"}}}}
	_, err := UpdateAppSet(ManifestEditConfig{Manifest: manifest}, []string{"["}, "custom-set")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid repo filter")
}

// TestAddToManifest_LocalConfigSourceStaysRelativeOnWriteBack guards
// against Validate's local-path resolution leaking into the manifest
// written back to disk. AddToManifest (like RemoveFromManifest) calls
// LoadManifest, Validate, then marshals the same *Manifest back to
// repos.yaml; the committed defaults.config_base.source must stay the relative path
// the operator wrote, not a machine-local absolute path.
func TestAddToManifest_LocalConfigSourceStaysRelativeOnWriteBack(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "repos.yaml")
	presetPath := filepath.Join(dir, "preset.yaml")

	require.NoError(t, os.WriteFile(presetPath, []byte("version: \"1\"\n"), 0o644))
	require.NoError(t, os.WriteFile(manifestPath, []byte(`version: 1
defaults:
  config_base:
    source: ./preset.yaml
github:
  mint_url: https://mint.example.com
  repos:
    - name: acme/existing
`), 0o644))

	manifest, err := LoadManifest(context.Background(), manifestPath)
	require.NoError(t, err)
	require.NoError(t, manifest.Validate())

	_, _, err = AddToManifest(context.Background(), ManifestEditConfig{
		Manifest:     manifest,
		ManifestPath: manifestPath,
	}, ForgeGitHub, []RepoEntry{{Name: "acme/new-repo"}}, nil, nil)
	require.NoError(t, err)

	reloaded, err := LoadManifest(context.Background(), manifestPath)
	require.NoError(t, err)
	assert.Equal(t, "./preset.yaml", reloaded.Defaults.ConfigBase.Source,
		"defaults.config_base.source must remain the relative path on disk, not the resolved absolute path")
}

func TestAddToManifest_Duplicate(t *testing.T) {
	manifest := testManifest("acme/api")

	result, _, err := AddToManifest(context.Background(), ManifestEditConfig{
		Manifest: manifest,
	}, ForgeGitHub, []RepoEntry{{Name: "acme/api"}}, nil, nil)

	if err != nil {
		t.Fatalf("AddToManifest() error = %v", err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "acme/api" {
		t.Errorf("Skipped = %v, want [acme/api]", result.Skipped)
	}
	if len(result.Added) != 0 {
		t.Errorf("Added = %v, want []", result.Added)
	}
}

func TestAddToManifest_DryRun(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "repos.yaml")

	manifest := testManifest("acme/existing")
	data, err := MarshalWithHeader(manifest)
	if err != nil {
		t.Fatalf("MarshalWithHeader() error = %v", err)
	}
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var phases []string
	progress := func(_, phase, _ string) {
		phases = append(phases, phase)
	}

	result, _, err := AddToManifest(context.Background(), ManifestEditConfig{
		Manifest:     manifest,
		ManifestPath: manifestPath,
		DryRun:       true,
	}, ForgeGitHub, []RepoEntry{{Name: "acme/new"}}, nil, progress)

	if err != nil {
		t.Fatalf("AddToManifest() error = %v", err)
	}
	if len(result.Added) != 1 {
		t.Errorf("Added = %v, want [acme/new]", result.Added)
	}

	// File should be unchanged.
	reloaded, err := LoadManifest(context.Background(), manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	if len(reloaded.AllRepos()) != 1 {
		t.Errorf("reloaded manifest has %d repos, want 1 (dry-run)", len(reloaded.AllRepos()))
	}

	hasDryRun := false
	for _, p := range phases {
		if p == "dry-run" {
			hasDryRun = true
		}
	}
	if !hasDryRun {
		t.Error("missing 'dry-run' phase callback")
	}
}

func TestAddToManifest_Multiple(t *testing.T) {
	manifest := testManifest("acme/existing")

	result, updated, err := AddToManifest(context.Background(), ManifestEditConfig{
		Manifest: manifest,
	}, ForgeGitHub, []RepoEntry{
		{Name: "acme/new-a"},
		{Name: "acme/existing"},
		{Name: "acme/new-b"},
	}, nil, nil)

	if err != nil {
		t.Fatalf("AddToManifest() error = %v", err)
	}
	if len(result.Added) != 2 {
		t.Errorf("Added = %v, want 2 entries", result.Added)
	}
	if len(result.Skipped) != 1 {
		t.Errorf("Skipped = %v, want [acme/existing]", result.Skipped)
	}
	if len(updated.AllRepos()) != 3 {
		t.Errorf("manifest has %d repos, want 3", len(updated.AllRepos()))
	}
}

func TestAddToManifest_NoManifest(t *testing.T) {
	_, _, err := AddToManifest(context.Background(), ManifestEditConfig{}, ForgeGitHub, []RepoEntry{{Name: "acme/api"}}, nil, nil)
	if err == nil {
		t.Fatal("AddToManifest() error = nil, want error for nil manifest")
	}
}

func TestAddToManifest_EmptyRepos(t *testing.T) {
	_, _, err := AddToManifest(context.Background(), ManifestEditConfig{
		Manifest: testManifest(),
	}, ForgeGitHub, nil, nil, nil)
	if err == nil {
		t.Fatal("AddToManifest() error = nil, want error for empty repos")
	}
}

func TestAddToManifest_InvalidRepoName(t *testing.T) {
	_, _, err := AddToManifest(context.Background(), ManifestEditConfig{
		Manifest: testManifest(),
	}, ForgeGitHub, []RepoEntry{{Name: "invalid-no-slash"}}, nil, nil)
	if err == nil {
		t.Fatal("AddToManifest() error = nil, want error for invalid repo name")
	}
	if !strings.Contains(err.Error(), "invalid repo name") {
		t.Errorf("error = %q, want to contain 'invalid repo name'", err.Error())
	}
}

func TestAddToManifest_GlobRepoAllowed(t *testing.T) {
	manifest := testManifest()
	result, _, err := AddToManifest(context.Background(), ManifestEditConfig{
		Manifest: manifest,
	}, ForgeGitHub, []RepoEntry{{Name: "acme/*"}}, nil, nil)
	if err != nil {
		t.Fatalf("AddToManifest() should allow glob entries: %v", err)
	}
	if len(result.Added) != 1 {
		t.Errorf("Added = %v, want [acme/*]", result.Added)
	}
}

func TestAddToManifest_GitLabNestedPaths(t *testing.T) {
	tests := []struct {
		name      string
		forge     string
		repoName  string
		wantErr   bool
		errSubstr string
	}{
		{
			name:     "gitlab 3-segment path accepted",
			forge:    ForgeGitLab,
			repoName: "group/subgroup/project",
		},
		{
			name:     "gitlab 4-segment path accepted",
			forge:    ForgeGitLab,
			repoName: "a/b/c/d",
		},
		{
			name:     "gitlab 2-segment path accepted",
			forge:    ForgeGitLab,
			repoName: "owner/project",
		},
		{
			name:      "single-segment rejected for gitlab",
			forge:     ForgeGitLab,
			repoName:  "project",
			wantErr:   true,
			errSubstr: "group[/subgroup]/project format",
		},
		{
			name:      "github rejects 3-segment path",
			forge:     ForgeGitHub,
			repoName:  "a/b/c",
			wantErr:   true,
			errSubstr: "owner/repo format",
		},
		{
			name:     "github 2-segment path accepted",
			forge:    ForgeGitHub,
			repoName: "owner/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := &Manifest{Version: 1}
			_, _, err := AddToManifest(context.Background(), ManifestEditConfig{
				Manifest: manifest,
			}, tt.forge, []RepoEntry{{Name: tt.repoName}}, nil, nil)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("error = %q, want to contain %q", err.Error(), tt.errSubstr)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestAddToManifest_DiscoverInstalled(t *testing.T) {
	fc := forge.NewFakeClient()
	fc.VariableValues["acme/api/FULLSEND_PER_REPO_INSTALL"] = "true"
	fc.VariableValues["acme/api/FULLSEND_MINT_URL"] = "https://mint.example.com"
	fc.VariableValues["acme/api/FULLSEND_GCP_REGION"] = "us-east1"
	fc.FileContents["acme/api/.github/workflows/fullsend.yml"] = []byte(
		`uses: fullsend-ai/fullsend/.github/workflows/reusable-dispatch.yml@v2.1.0`)

	manifest := testManifest()

	result, updated, err := AddToManifest(context.Background(), ManifestEditConfig{
		Manifest: manifest,
	}, ForgeGitHub, []RepoEntry{{Name: "acme/api"}}, newTestClientFactory(fc), nil)

	if err != nil {
		t.Fatalf("AddToManifest() error = %v", err)
	}
	if len(result.Added) != 1 {
		t.Fatalf("Added = %v, want [acme/api]", result.Added)
	}
	entry := updated.AllRepos()[len(updated.AllRepos())-1]
	if entry.Name != "acme/api" {
		t.Errorf("Repo = %q, want acme/api", entry.Name)
	}
}

func TestAddToManifest_DiscoverInstalledMatchesDefaults(t *testing.T) {
	fc := forge.NewFakeClient()
	fc.VariableValues["acme/api/FULLSEND_PER_REPO_INSTALL"] = "true"
	fc.VariableValues["acme/api/FULLSEND_MINT_URL"] = "https://mint.example.com"
	fc.VariableValues["acme/api/FULLSEND_GCP_REGION"] = "us-central1"
	fc.FileContents["acme/api/.github/workflows/fullsend.yml"] = []byte(
		`uses: fullsend-ai/fullsend/.github/workflows/reusable-dispatch.yml@v2.3.0`)

	manifest := testManifest()

	_, updated, err := AddToManifest(context.Background(), ManifestEditConfig{
		Manifest: manifest,
	}, ForgeGitHub, []RepoEntry{{Name: "acme/api"}}, newTestClientFactory(fc), nil)

	if err != nil {
		t.Fatalf("AddToManifest() error = %v", err)
	}
	entry := updated.AllRepos()[len(updated.AllRepos())-1]
	if entry.Name != "acme/api" {
		t.Errorf("Repo = %q, want acme/api", entry.Name)
	}
}

func TestAddToManifest_DiscoverNotInstalled(t *testing.T) {
	fc := forge.NewFakeClient()

	manifest := testManifest()

	_, updated, err := AddToManifest(context.Background(), ManifestEditConfig{
		Manifest: manifest,
	}, ForgeGitHub, []RepoEntry{{Name: "acme/api"}}, newTestClientFactory(fc), nil)

	if err != nil {
		t.Fatalf("AddToManifest() error = %v", err)
	}
	entry := updated.AllRepos()[len(updated.AllRepos())-1]
	if entry.Name != "acme/api" {
		t.Errorf("Repo = %q, want acme/api", entry.Name)
	}
}

func TestAddToManifest_DiscoverGlobSkipped(t *testing.T) {
	fc := forge.NewFakeClient()

	manifest := testManifest()
	result, _, err := AddToManifest(context.Background(), ManifestEditConfig{
		Manifest: manifest,
	}, ForgeGitHub, []RepoEntry{{Name: "acme/*"}}, newTestClientFactory(fc), nil)

	if err != nil {
		t.Fatalf("AddToManifest() error = %v", err)
	}
	if len(result.Added) != 1 || result.Added[0] != "acme/*" {
		t.Errorf("Added = %v, want [acme/*]", result.Added)
	}
}

func TestAddToManifest_DiscoverProbeError(t *testing.T) {
	fc := forge.NewFakeClient()
	fc.Errors["ListRepoVariables"] = fmt.Errorf("api error")

	manifest := testManifest()
	manifest.GitHub.FullsendRef = "v2.3.0"

	result, _, err := AddToManifest(context.Background(), ManifestEditConfig{
		Manifest: manifest,
	}, ForgeGitHub, []RepoEntry{{Name: "acme/api"}}, newTestClientFactory(fc), nil)

	if err != nil {
		t.Fatalf("AddToManifest() error = %v, want graceful skip on probe error", err)
	}
	if len(result.Added) != 1 {
		t.Errorf("Added = %v, want [acme/api] even on probe error", result.Added)
	}
}

func TestAddToManifest_DiscoverGitLabFullsendRef(t *testing.T) {
	fc := forge.NewFakeClient()
	fc.Secrets["acme/api/"+forge.SecretForgeToken] = true
	fc.FileContents["acme/api/"+fullsendPipelineInclude] = []byte(
		"# fullsend-ref: v3.2.0\ninclude:\n  - project: fullsend-ai/fullsend\n    ref: v3.2.0\n    file: .gitlab/ci/dispatch.yml\n")

	manifest := &Manifest{
		Version: 1,
		GitLab: &PlatformConfig{
			URL:         "https://gitlab.example.com",
			FullsendRef: "v3.0.0",
		},
	}

	_, updated, err := AddToManifest(context.Background(), ManifestEditConfig{
		Manifest: manifest,
	}, ForgeGitLab, []RepoEntry{{Name: "acme/api"}}, newTestClientFactory(fc), nil)

	if err != nil {
		t.Fatalf("AddToManifest() error = %v", err)
	}
	entry := updated.AllRepos()[len(updated.AllRepos())-1]
	if entry.FullsendRef != "v3.2.0" {
		t.Errorf("FullsendRef = %q, want v3.2.0", entry.FullsendRef)
	}
}

func TestAddToManifest_DiscoverGitLabFullsendRefMatchesDefault(t *testing.T) {
	fc := forge.NewFakeClient()
	fc.VariableValues["acme/api/FULLSEND_PER_REPO_INSTALL"] = "true"
	fc.FileContents["acme/api/"+fullsendPipelineInclude] = []byte(
		"# fullsend-ref: v3.0.0\ninclude:\n  - project: fullsend-ai/fullsend\n    ref: v3.0.0\n    file: .gitlab/ci/dispatch.yml\n")

	manifest := &Manifest{
		Version: 1,
		GitLab: &PlatformConfig{
			URL:         "https://gitlab.example.com",
			FullsendRef: "v3.0.0",
		},
	}

	_, updated, err := AddToManifest(context.Background(), ManifestEditConfig{
		Manifest: manifest,
	}, ForgeGitLab, []RepoEntry{{Name: "acme/api"}}, newTestClientFactory(fc), nil)

	if err != nil {
		t.Fatalf("AddToManifest() error = %v", err)
	}
	entry := updated.AllRepos()[len(updated.AllRepos())-1]
	if entry.FullsendRef != "" {
		t.Errorf("FullsendRef = %q, want empty (matches platform default)", entry.FullsendRef)
	}
}

func TestRemoveFromManifest_Basic(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "repos.yaml")

	manifest := testManifest("acme/api", "acme/web", "acme/docs")
	data, err := MarshalWithHeader(manifest)
	if err != nil {
		t.Fatalf("MarshalWithHeader() error = %v", err)
	}
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	result, updated, err := RemoveFromManifest(ManifestEditConfig{
		Manifest:     manifest,
		ManifestPath: manifestPath,
	}, []string{"acme/api", "acme/docs"}, nil)

	if err != nil {
		t.Fatalf("RemoveFromManifest() error = %v", err)
	}
	if len(result.Removed) != 2 {
		t.Errorf("Removed = %v, want 2 entries", result.Removed)
	}
	if len(result.Skipped) != 0 {
		t.Errorf("Skipped = %v, want []", result.Skipped)
	}
	if len(updated.AllRepos()) != 1 {
		t.Errorf("manifest has %d repos, want 1", len(updated.AllRepos()))
	}
	if updated.AllRepos()[0].Name != "acme/web" {
		t.Errorf("remaining repo = %q, want acme/web", updated.AllRepos()[0].Name)
	}

	reloaded, err := LoadManifest(context.Background(), manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	if len(reloaded.AllRepos()) != 1 {
		t.Errorf("reloaded manifest has %d repos, want 1", len(reloaded.AllRepos()))
	}
}

func TestRemoveFromManifest_Glob(t *testing.T) {
	manifest := testManifest("acme/api", "acme/web", "other/docs")

	result, updated, err := RemoveFromManifest(ManifestEditConfig{
		Manifest: manifest,
	}, []string{"acme/*"}, nil)

	if err != nil {
		t.Fatalf("RemoveFromManifest() error = %v", err)
	}
	if len(result.Removed) != 2 {
		t.Errorf("Removed = %v, want [acme/api, acme/web]", result.Removed)
	}
	if len(updated.AllRepos()) != 1 {
		t.Errorf("manifest has %d repos, want 1", len(updated.AllRepos()))
	}
	if updated.AllRepos()[0].Name != "other/docs" {
		t.Errorf("remaining repo = %q, want other/docs", updated.AllRepos()[0].Name)
	}
}

func TestRemoveFromManifest_NotFound(t *testing.T) {
	manifest := testManifest("acme/web")

	var msgs []string
	progress := func(_, _, msg string) { msgs = append(msgs, msg) }

	result, _, err := RemoveFromManifest(ManifestEditConfig{
		Manifest: manifest,
	}, []string{"acme/missing"}, progress)

	if err != nil {
		t.Fatalf("RemoveFromManifest() error = %v", err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "acme/missing" {
		t.Errorf("Skipped = %v, want [acme/missing]", result.Skipped)
	}
	if len(result.Removed) != 0 {
		t.Errorf("Removed = %v, want []", result.Removed)
	}
}

func TestRemoveFromManifest_DryRun(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "repos.yaml")

	manifest := testManifest("acme/api", "acme/web")
	data, err := MarshalWithHeader(manifest)
	if err != nil {
		t.Fatalf("MarshalWithHeader() error = %v", err)
	}
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var phases []string
	progress := func(_, phase, _ string) { phases = append(phases, phase) }

	result, _, err := RemoveFromManifest(ManifestEditConfig{
		Manifest:     manifest,
		ManifestPath: manifestPath,
		DryRun:       true,
	}, []string{"acme/api"}, progress)

	if err != nil {
		t.Fatalf("RemoveFromManifest() error = %v", err)
	}
	if len(result.Removed) != 1 {
		t.Errorf("Removed = %v, want [acme/api]", result.Removed)
	}

	reloaded, err := LoadManifest(context.Background(), manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	if len(reloaded.AllRepos()) != 2 {
		t.Errorf("reloaded manifest has %d repos, want 2 (dry-run)", len(reloaded.AllRepos()))
	}

	hasDryRun := false
	for _, p := range phases {
		if p == "dry-run" {
			hasDryRun = true
		}
	}
	if !hasDryRun {
		t.Error("missing 'dry-run' phase callback")
	}
}

func TestRemoveFromManifest_NoManifest(t *testing.T) {
	_, _, err := RemoveFromManifest(ManifestEditConfig{}, []string{"acme/api"}, nil)
	if err == nil {
		t.Fatal("RemoveFromManifest() error = nil, want error for nil manifest")
	}
}

func TestRemoveFromManifest_EmptyRepos(t *testing.T) {
	_, _, err := RemoveFromManifest(ManifestEditConfig{
		Manifest: testManifest(),
	}, nil, nil)
	if err == nil {
		t.Fatal("RemoveFromManifest() error = nil, want error for empty repos")
	}
}

func TestMatchManifestRepos_Exact(t *testing.T) {
	manifest := testManifest("acme/api", "acme/web", "other/docs")
	matched, err := MatchManifestRepos(manifest, []string{"acme/api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 1 || matched[0] != "acme/api" {
		t.Errorf("MatchManifestRepos() = %v, want [acme/api]", matched)
	}
}

func TestMatchManifestRepos_Glob(t *testing.T) {
	manifest := testManifest("acme/api", "acme/web", "other/docs")
	matched, err := MatchManifestRepos(manifest, []string{"acme/*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 2 {
		t.Errorf("MatchManifestRepos() = %v, want [acme/api, acme/web]", matched)
	}
}

func TestMatchManifestRepos_CaseInsensitive(t *testing.T) {
	manifest := testManifest("Acme/API")
	matched, err := MatchManifestRepos(manifest, []string{"acme/api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 1 {
		t.Errorf("MatchManifestRepos() = %v, want [Acme/API]", matched)
	}
}

func TestMatchManifestRepos_NoMatch(t *testing.T) {
	manifest := testManifest("acme/api")
	matched, err := MatchManifestRepos(manifest, []string{"other/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 0 {
		t.Errorf("MatchManifestRepos() = %v, want []", matched)
	}
}

func TestMatchManifestRepos_BadPattern(t *testing.T) {
	manifest := testManifest("acme/api")
	_, err := MatchManifestRepos(manifest, []string{"acme/[invalid"})
	if err == nil {
		t.Error("expected error for malformed glob pattern")
	}
}

func TestUpdateAppSet_ConcreteFilterCopiesGlobEntry(t *testing.T) {
	manifest := &Manifest{
		Version: 1,
		GitHub: &PlatformConfig{Repos: []RepoEntry{{
			Name:        "acme/*",
			FullsendRef: "v2.0.0",
			Inference:   InferenceSettings{Auth: InferenceAuthOpenAIAPIKey},
		}}},
	}
	updated, err := UpdateAppSet(ManifestEditConfig{Manifest: manifest}, []string{"acme/api"}, "custom-set")
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/api"}, updated)
	require.Len(t, manifest.GitHub.Repos, 2)
	assert.Empty(t, manifest.GitHub.Repos[0].AppSet)
	api := manifest.GitHub.Repos[1]
	assert.Equal(t, "acme/api", api.Name)
	assert.Equal(t, "custom-set", api.AppSet)
	assert.Equal(t, InferenceAuthOpenAIAPIKey, api.Inference.Auth)
	assert.Equal(t, "v2.0.0", api.FullsendRef)
}
