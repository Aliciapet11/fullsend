package agentnew

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fullsend-ai/fullsend/internal/harness"
)

// loadAndCheck mirrors what the command does after writing the tree.
func loadAndCheck(t *testing.T, dir, name string) ([]harness.Diagnostic, error) {
	t.Helper()
	h, err := harness.Load(filepath.Join(dir, "harness", name+".yaml"))
	if err != nil {
		return nil, err
	}
	diags, err := harness.CheckGenerated(h, dir)
	return ignoreIssueURLDeprecation(diags), err
}

// ignoreIssueURLDeprecation drops exactly the GITHUB_ISSUE_URL deprecation
// warning on the scaffold's env maps. The generator still emits
// ISSUE_URL: ${GITHUB_ISSUE_URL} until the template migrates to
// FULLSEND_WORK_ITEM_URL (follow-up to #6610); until then, generated-tree
// tests exempt this one diagnostic and nothing else — in particular,
// error-level diagnostics always survive.
func ignoreIssueURLDeprecation(diags []harness.Diagnostic) []harness.Diagnostic {
	kept := make([]harness.Diagnostic, 0, len(diags))
	for _, d := range diags {
		if d.Severity == harness.SeverityWarning &&
			(d.Field == "env.runner" || d.Field == "env.sandbox") &&
			strings.HasPrefix(d.Message, harness.DeprecatedIssueURLWarning) {
			continue
		}
		kept = append(kept, d)
	}
	return kept
}

func generateInto(t *testing.T, dir string, opts Options) {
	t.Helper()
	files, err := Render(opts)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, files)
}

func TestCheckGeneratedAcceptsAFreshTree(t *testing.T) {
	for _, role := range RoleNames() {
		t.Run(role, func(t *testing.T) {
			dir := t.TempDir()
			generateInto(t, dir, testOptions("lint-docs", role))
			diags, err := loadAndCheck(t, dir, "lint-docs")
			if err != nil {
				t.Fatalf("freshly generated tree failed CheckGenerated: %v", err)
			}
			if len(diags) != 0 {
				t.Errorf("unexpected lint diagnostics: %v", diags)
			}
		})
	}
}

// TestGeneratedTreeHasNoProviderOrProfileFiles pins the fix for the "no
// update path" problem (#7268, #6990): a generated harness's providers:
// entries are bare builtin names that resolve against fullsend's own
// embedded definitions and profiles at run time, so `agent new` writes no
// providers/ or profiles/ files for a role to go stale against, and
// validateResourceFilesExist has nothing on disk to check for them (bare
// names are not provider paths).
func TestGeneratedTreeHasNoProviderOrProfileFiles(t *testing.T) {
	for _, role := range RoleNames() {
		t.Run(role, func(t *testing.T) {
			dir := t.TempDir()
			generateInto(t, dir, testOptions("lint-docs", role))
			for _, sub := range []string{"providers", "profiles"} {
				if _, err := os.Stat(filepath.Join(dir, sub)); err == nil {
					t.Errorf("role %q: generated tree should not write a %s/ directory", role, sub)
				} else if !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			diags, err := loadAndCheck(t, dir, "lint-docs")
			if err != nil {
				t.Fatalf("freshly generated tree with no providers/profiles files failed CheckGenerated: %v", err)
			}
			if len(diags) != 0 {
				t.Errorf("unexpected lint diagnostics: %v", diags)
			}
		})
	}
}

// TestCheckGeneratedCatchesMissingOwnedFiles covers the #6834 class: a
// policy:, agent: or post_script: pointing at a file that is not there.
func TestCheckGeneratedCatchesMissingOwnedFiles(t *testing.T) {
	for _, victim := range []string{
		"policies/base.yaml",
		"agents/lint-docs.md",
		"scripts/post-lint-docs.sh",
	} {
		t.Run(victim, func(t *testing.T) {
			dir := t.TempDir()
			generateInto(t, dir, testOptions("lint-docs", "triage"))
			if err := os.Remove(filepath.Join(dir, victim)); err != nil {
				t.Fatal(err)
			}
			_, err := loadAndCheck(t, dir, "lint-docs")
			if err == nil {
				t.Fatalf("deleting %s was not detected", victim)
			}
		})
	}
}

// TestCheckGeneratedCatchesMissingSchema: with the validation loop on, the
// schema is a validated file too.
func TestCheckGeneratedCatchesMissingSchema(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions("lint-docs", "triage")
	opts.ValidationLoop = true
	generateInto(t, dir, opts)

	if _, err := loadAndCheck(t, dir, "lint-docs"); err != nil {
		t.Fatalf("tree with --validation-loop should be valid: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "schemas", "lint-docs-result.schema.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAndCheck(t, dir, "lint-docs"); err == nil {
		t.Fatal("deleting the schema was not detected")
	}
}

// TestIgnoreIssueURLDeprecationKeepsOtherDiagnostics pins the filter's
// narrowness: only the scaffold's known GITHUB_ISSUE_URL deprecation warning
// on env.runner/env.sandbox is dropped. Error-level diagnostics — even ones
// mentioning GITHUB_ISSUE_URL in their message — and warnings on other
// fields survive.
func TestIgnoreIssueURLDeprecationKeepsOtherDiagnostics(t *testing.T) {
	diags := []harness.Diagnostic{
		{Severity: harness.SeverityWarning, Field: "env.runner", Message: harness.DeprecatedIssueURLWarning + ` (entry "ISSUE_URL")`},
		{Severity: harness.SeverityWarning, Field: "env.sandbox", Message: harness.DeprecatedIssueURLWarning + ` (entry "GITHUB_ISSUE_URL")`},
		{Severity: harness.SeverityError, Field: "validation_loop.preflight_check", Message: `value "GITHUB_ISSUE_URL.sh" looks like a script path`},
		{Severity: harness.SeverityWarning, Field: "runner_env", Message: harness.DeprecatedIssueURLWarning + ` (entry "GITHUB_ISSUE_URL")`},
	}
	kept := ignoreIssueURLDeprecation(diags)
	if len(kept) != 2 {
		t.Fatalf("kept %d diagnostics, want 2: %v", len(kept), kept)
	}
	for _, d := range kept {
		if d.Severity != harness.SeverityError && d.Field != "runner_env" {
			t.Errorf("unexpectedly kept diagnostic: %v", d)
		}
	}
}
