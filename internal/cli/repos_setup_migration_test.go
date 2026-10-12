package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/forge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file follow the `github setup` → `repos install`
// migration steps documented in
// docs/guides/getting-started/migrating-from-github-setup.md (#8219).

// setupMigrationPreset sets runtime and keep_history, so the tests can check
// that a value declared in repos.yaml wins while the other preset value is
// still inherited. keep_history differs from its code default (true).
const setupMigrationPreset = "version: \"1\"\nruntime: claude\nkeep_history: false\n"

const setupMigrationManifestHeader = `version: 1
defaults:
  config_base:
    source: preset.yaml
github:
  mint_url: https://mint.example.com
  fullsend_ref: v1.0.0
  inference:
    auth: vertex-wif
  repos:
    - name: acme/api
`

func basePath(repo string) string {
	return repo + "/.fullsend/config.base.yaml"
}

// writeSetupMigrationManifest writes repos.yaml (header plus entry) and the
// preset it declares, next to each other, and returns the manifest path.
func writeSetupMigrationManifest(t *testing.T, entry string) string {
	t.Helper()
	manifestPath := writeTestManifest(t, setupMigrationManifestHeader+entry)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(manifestPath), "preset.yaml"), []byte(setupMigrationPreset), 0o644))
	return manifestPath
}

func readManifestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// assertSetupMigrationLayers checks the installed layers: the base is the
// declared preset byte for byte, and the overlay is the sparse managed file
// carrying only what repos.yaml declares. The effective runtime is the
// declared one, and keep_history is still inherited from the preset.
func assertSetupMigrationLayers(t *testing.T, fc *forge.FakeClient, wantRuntimeLine, wantRuntime string) {
	t.Helper()
	base := fc.FileContents[basePath("acme/api")]
	assert.Equal(t, setupMigrationPreset, string(base), "the base layer must be the declared preset")

	overlay := string(fc.FileContents[overlayPath("acme/api")])
	assert.True(t, strings.HasPrefix(overlay, managedConfigMarker), "ownership marker missing:\n%s", overlay)
	assert.Contains(t, overlay, wantRuntimeLine)
	assert.NotContains(t, overlay, "keep_history", "inherited preset values must not be materialized")

	effective, err := config.ParsePerRepoConfigWriterLayered([]byte(overlay), base)
	require.NoError(t, err)
	assert.Equal(t, wantRuntime, effective.ConfigRuntime(), "the declared runtime must win over the preset")
	assert.False(t, effective.ConfigKeepHistory(), "an undeclared key must keep inheriting the preset value")
}

// A fresh repository: runtime declared on the repository entry resolves
// above the declared preset, repeated installs keep repos.yaml as the
// source of truth, and a later manifest edit is what changes the value.
func TestRunReposInstall_SetupMigrationFreshRepoDeclaredValueAbovePreset(t *testing.T) {
	manifestPath := writeSetupMigrationManifest(t, "      config:\n        runtime: pi\n")
	fc := newInstallFakeClient("acme/api")

	require.NoError(t, runReposInstall(context.Background(), githubManagedInstallOpts(manifestPath, fc)))
	assertSetupMigrationLayers(t, fc, "runtime: pi", "pi")

	manifestAfterInstall := readManifestFile(t, manifestPath)
	overlayAfterInstall := fc.FileContents[overlayPath("acme/api")]

	// Rerun: nothing changes in the manifest or in the managed files.
	require.NoError(t, runReposInstall(context.Background(), githubManagedInstallOpts(manifestPath, fc)))
	assert.Equal(t, manifestAfterInstall, readManifestFile(t, manifestPath), "a rerun must not edit repos.yaml")
	assert.Equal(t, overlayAfterInstall, fc.FileContents[overlayPath("acme/api")], "a rerun must not change the managed file")
	assertSetupMigrationLayers(t, fc, "runtime: pi", "pi")

	result, err := statusJSON(t, manifestPath, fc)
	require.NoError(t, err)
	if d, found := overlayDrift(t, result, "acme/api"); found {
		t.Fatalf("status reported managed-configuration drift after a rerun: %+v", d)
	}

	// A later change is a repos.yaml edit. A value equal to the preset's
	// is still written, so the repository keeps it if the preset changes.
	require.NoError(t, os.WriteFile(manifestPath, []byte(setupMigrationManifestHeader+"      config:\n        runtime: claude\n"), 0o644))
	require.NoError(t, runReposInstall(context.Background(), githubManagedInstallOpts(manifestPath, fc)))
	assertSetupMigrationLayers(t, fc, "runtime: claude", "claude")
}

// A repository installed by `github setup` with --config and --runtime: an
// existing preset base and a markerless overlay. Install stops until the
// overlay is adopted. After the setting is copied into repos.yaml and the
// marker is added, install rewrites the overlay from the manifest and leaves
// the matching base alone. Reruns change nothing.
func TestRunReposInstall_SetupMigrationAdoptsSetupOverlayAndPreset(t *testing.T) {
	manifestPath := writeSetupMigrationManifest(t, "")
	manifestBefore := readManifestFile(t, manifestPath)

	fc := newInstalledFakeClientCLI("acme/api")
	setupOverlay := []byte("version: \"1\"\nruntime: pi\nkeep_history: true\n")
	fc.FileContents[basePath("acme/api")] = []byte(setupMigrationPreset)
	fc.FileContents[overlayPath("acme/api")] = setupOverlay

	err := runReposInstall(context.Background(), githubManagedInstallOpts(manifestPath, fc))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "adoption required")
	assert.Equal(t, setupOverlay, fc.FileContents[overlayPath("acme/api")], "the markerless overlay must be untouched")
	assert.Equal(t, manifestBefore, readManifestFile(t, manifestPath), "no manifest write before adoption")

	result, _ := statusJSON(t, manifestPath, fc)
	d, found := overlayDrift(t, result, "acme/api")
	require.True(t, found, "status must report adoption required, got %s", mustEncode(t, result))
	assert.Equal(t, "managed configuration (adoption required)", d.Expected)
	for _, want := range []string{"proposed repos.yaml entry for acme/api", "runtime: pi", "keep_history: true"} {
		assert.Contains(t, d.Detail, want)
	}

	// Adopt: keep runtime in repos.yaml, let keep_history fall back to the
	// preset, and mark the existing file as managed.
	require.NoError(t, os.WriteFile(manifestPath, []byte(setupMigrationManifestHeader+"      config:\n        runtime: pi\n"), 0o644))
	fc.FileContents[overlayPath("acme/api")] = append([]byte(managedConfigMarker), setupOverlay...)

	require.NoError(t, runReposInstall(context.Background(), githubManagedInstallOpts(manifestPath, fc)))
	assertSetupMigrationLayers(t, fc, "runtime: pi", "pi")

	manifestAfterInstall := readManifestFile(t, manifestPath)
	overlayAfterInstall := fc.FileContents[overlayPath("acme/api")]
	require.NoError(t, runReposInstall(context.Background(), githubManagedInstallOpts(manifestPath, fc)))
	assert.Equal(t, manifestAfterInstall, readManifestFile(t, manifestPath), "a rerun must not edit repos.yaml")
	assert.Equal(t, overlayAfterInstall, fc.FileContents[overlayPath("acme/api")], "a rerun must not change the managed file")

	result, _ = statusJSON(t, manifestPath, fc)
	if d, found := overlayDrift(t, result, "acme/api"); found {
		t.Fatalf("status reported managed-configuration drift after adoption: %+v", d)
	}
}
