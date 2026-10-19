package cli

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/fetch"
	gh "github.com/fullsend-ai/fullsend/internal/forge/github"
	"github.com/fullsend-ai/fullsend/internal/harness"
	"github.com/fullsend-ai/fullsend/internal/ui"
)

func newLintCmd() *cobra.Command {
	var fullsendDir string
	var forgeFlag string
	var strict bool
	var offline bool

	cmd := &cobra.Command{
		Use:   "lint",
		Short: "Lint configuration and harness definitions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLint(cmd, fullsendDir, forgeFlag, strict, offline)
		},
	}

	addFullsendDirFlag(cmd, &fullsendDir)
	cmd.Flags().StringVar(&forgeFlag, "forge", "", `forge platform to use (e.g. "github", "gitlab"); auto-detected from config.yaml or CI env vars when omitted`)
	cmd.Flags().BoolVar(&strict, "strict", false, "exit non-zero on warning-level diagnostics (deprecations), not just error-level ones")
	cmd.Flags().BoolVar(&offline, "offline", false, "reject network fetches; only use cached remote resources")

	return cmd
}

// runLint resolves and composes every enabled agent's harness exactly as
// fullsend run does — resolveAgentSource for resolution (config entries,
// URL fetches, agents-repo fallback for override-only entries) and
// LoadWithBase for composition (base chains, forge overlays for the
// effective platform) — then applies run's path resolution and file
// validation (ResolveRelativeTo, ValidateFilesExist) so a harness run
// would reject fails lint too, and reports harness lint diagnostics per
// agent.
func runLint(cmd *cobra.Command, fullsendDir, forgeFlag string, strict, offline bool) error {
	absFullsendDir, err := filepath.Abs(fullsendDir)
	if err != nil {
		return err
	}

	printer := ui.New(cmd.OutOrStdout())

	// Layered config load (config.yaml over config.base.yaml), same as
	// run/lock. Lint requires a config: without one there are no
	// registered agents to lint.
	cfg, err := loadLockConfig(filepath.Join(absFullsendDir, "config.yaml"))
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("no config.yaml in %s: nothing to lint", absFullsendDir)
	}

	policy := fetch.DefaultPolicy
	policy.Offline = offline

	forgePlatform, err := detectForgePlatform(forgeFlag, cfg)
	if err != nil {
		return fmt.Errorf("invalid --forge flag: %w", err)
	}

	composeGitToken, tokenErr := resolveToken()
	if tokenErr != nil {
		printer.StepWarn("Git token not available; private repo fetches may fail")
	}

	// Same fetch machinery as run, so lint is read-only for config and
	// forge state but not for .fullsend-cache: URL resolution populates
	// the cache and appends to the audit log.
	composeOpts := harness.ComposeOpts{
		WorkspaceRoot: absFullsendDir,
		FetchPolicy:   policy,
		AuditLogPath:  filepath.Join(absFullsendDir, ".fullsend-cache", "fetch-audit.jsonl"),
		ForgePlatform: forgePlatform,
		OrgAllowlist:  cfg.AllowedResources(),
		Config:        harness.BuildConfigMap(cfg),
		GitToken:      composeGitToken,
	}

	// Always create a GitHub client for the agents-repo fallback —
	// fullsend-ai/agents is public, so unauthenticated requests work when
	// no GitHub token is available (same as fullsend run).
	fallbackForgeClient := gh.New(composeGitToken)

	var errs []error
	errDiags := 0
	warnDiags := 0
	// Semantic validation, the same checks config-writing commands
	// enforce: loadPerRepoLayers only unmarshals, so without this a
	// config.yaml with an unsupported version or an unknown runtime
	// lints clean. Reported as a diagnostic (not a load error) so the
	// agents still lint and every problem is reported together.
	if err := cfg.Validate(); err != nil {
		emitDiagnosticWithContext(printer, "config", harness.Diagnostic{
			Severity: harness.SeverityError,
			Field:    "config.yaml",
			Message:  err.Error(),
		})
		errDiags++
	}
	entries := cfg.AgentEntries()
	for _, entry := range entries {
		if !entry.IsEnabled() {
			continue
		}
		agentName := entry.DerivedName()
		// Last-writer-wins: an enabled entry followed by a later disable
		// entry for the same name is effectively disabled. Skip it like any
		// other disabled agent instead of failing resolution.
		if config.IsAgentExplicitlyDisabled(entries, agentName) {
			continue
		}
		harnessPath, fetchDeps, err := resolveAgentSource(cmd.Context(), absFullsendDir, agentName, fallbackForgeClient, cfg, composeOpts, printer)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolving harness for agent %q: %w", agentName, err))
			continue
		}
		agentOpts := composeOpts
		if len(fetchDeps) > 0 && fetchDeps[0].URL != "" {
			agentOpts.SourceURL = fetchDeps[0].URL
		}
		h, _, err := harness.LoadWithBase(cmd.Context(), harnessPath, agentOpts)
		if err != nil {
			errs = append(errs, fmt.Errorf("loading harness for agent %q: %w", agentName, err))
			continue
		}
		// Same path resolution as fullsend run: relative paths resolve
		// against the configuration directory, and paths escaping it are
		// rejected. ${VAR} paths are left alone — they expand at run time.
		if err := h.ResolveRelativeTo(absFullsendDir); err != nil {
			errs = append(errs, fmt.Errorf("resolving paths for agent %q: %w", agentName, err))
			continue
		}
		for _, diag := range h.Lint() {
			emitDiagnosticWithContext(printer, agentName, diag)
			if diag.Severity == harness.SeverityError {
				errDiags++
			} else {
				warnDiags++
			}
		}
		// Same file validation as fullsend run, so lint catches at commit
		// time what run would reject: a missing policy, or a referenced
		// file that does not exist. URL references are skipped — lint
		// validates what is on disk and does not fetch remotes.
		if err := h.ValidateFilesExist(); err != nil {
			errs = append(errs, fmt.Errorf("validating files for agent %q: %w", agentName, err))
		}
	}
	if errDiags > 0 {
		errs = append(errs, fmt.Errorf("lint found %d error-level diagnostic(s)", errDiags))
	}
	if strict && warnDiags > 0 {
		errs = append(errs, fmt.Errorf("lint found %d warning-level diagnostic(s) (--strict)", warnDiags))
	}
	return errors.Join(errs...)
}
