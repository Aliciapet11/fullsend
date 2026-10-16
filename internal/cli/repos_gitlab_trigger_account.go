package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/fullsend-ai/fullsend/internal/forge"
	"github.com/fullsend-ai/fullsend/internal/forge/gitlab"
	"github.com/fullsend-ai/fullsend/internal/gitlablifecycle"
	"github.com/fullsend-ai/fullsend/internal/repos"
	"github.com/fullsend-ai/fullsend/internal/ui"
)

// maybeConvergeGitLabTriggerAccount installs, repairs, or (with rotate set and
// a ready account) rotates the trigger-only service account and its
// protected-branch resources (#8437). It holds the project lease for the whole
// transaction and passes it to the lifecycle. A deferral is reported and is not
// a failure: existing polling stays the dispatch path, and the new dispatch
// route stays inactive until #8438. Clients that are not the live GitLab client
// (tests) have no trigger-account API and are skipped.
func maybeConvergeGitLabTriggerAccount(ctx context.Context, opts *reposInstallConfig, client forge.Client, printer *ui.Printer, owner, repo string, rotate bool) (err error) {
	if opts.testConvergeTriggerAccount != nil {
		return opts.testConvergeTriggerAccount(ctx, client, owner, repo)
	}
	glClient, ok := client.(*gitlab.LiveClient)
	if !ok {
		return nil
	}
	repoFullName := owner + "/" + repo
	if opts.dryRun {
		printer.StepInfo(fmt.Sprintf("[%s] Would converge the GitLab trigger account", repoFullName))
		return nil
	}
	info, err := client.GetRepo(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("reading the default branch for the GitLab trigger account: %w", repos.SafeAPIError("reading the repository", err))
	}
	printer.StepStart(fmt.Sprintf("[%s] Converging the GitLab trigger account", repoFullName))
	lease, release, err := repos.LockGitLabProjectLease(ctx, client, owner, repo)
	if err != nil {
		printer.StepFail(fmt.Sprintf("[%s] GitLab trigger account lease unavailable", repoFullName))
		return fmt.Errorf("serializing the GitLab trigger account with other installers: %w", err)
	}
	defer release(&err)

	triggerOwner := gitlablifecycle.NewTriggerOwner(glClient)
	lifecycleOpts, err := triggerOwner.TriggerAccountOptions(ctx, owner, repo, info.DefaultBranch, lease)
	if err != nil {
		printer.StepFail(fmt.Sprintf("[%s] GitLab trigger account inventory failed", repoFullName))
		return err
	}
	var result repos.GitLabTriggerAccountResult
	diag := repos.DiagnoseGitLabTriggerAccount(ctx, client, nil, owner, repo, "")
	if rotate && diag.State == repos.GitLabTriggerStateReady {
		result, err = repos.RotateGitLabTriggerAccount(ctx, lifecycleOpts)
	} else {
		result, err = repos.ConvergeGitLabTriggerAccount(ctx, lifecycleOpts)
	}
	if err != nil {
		printer.StepFail(fmt.Sprintf("[%s] GitLab trigger account failed", repoFullName))
		return err
	}
	switch {
	case result.Deferred != "":
		printer.StepInfo(fmt.Sprintf("[%s] GitLab trigger account deferred: %s. Polling stays the dispatch path", repoFullName, result.Deferred))
	case result.Ready:
		printer.StepDone(fmt.Sprintf("[%s] GitLab trigger account ready (the dispatch route stays inactive until it is activated separately)", repoFullName))
		reportGitLabTriggerDispatchGate(ctx, client, lifecycleOpts.API, printer, owner, repo, info.DefaultBranch)
	}
	return nil
}

// reportGitLabTriggerDispatchGate rechecks the trigger-dispatch activation
// gate (#8438) and reports the result. Every install path that prepares the
// trigger account reaches it, including the one taken after GitLab refuses the
// Poller protected-ref grant. A deferral is informational: it changes nothing
// and polling stays the dispatch path.
func reportGitLabTriggerDispatchGate(ctx context.Context, client forge.Client, api repos.GitLabTriggerAccountAPI, printer *ui.Printer, owner, repo, branch string) {
	printGitLabTriggerDispatchGate(printer, owner+"/"+repo, repos.CheckGitLabTriggerDispatchGate(ctx, client, api, owner, repo, branch))
}

// printGitLabTriggerDispatchGate prints a dispatch-gate result.
func printGitLabTriggerDispatchGate(printer *ui.Printer, repoFullName string, gate repos.GitLabTriggerDispatchGate) {
	if !gate.Ready {
		printer.StepInfo(fmt.Sprintf("[%s] GitLab trigger dispatch activation deferred: %s. Polling stays the dispatch path", repoFullName, strings.Join(gate.Deferred, "; ")))
		return
	}
	printer.StepInfo(fmt.Sprintf("[%s] GitLab trigger dispatch prerequisites verified (schedules migrated off pipeline variables, override role no_one_allowed, job-token repository push disabled, trigger account ready); the dispatch route is not yet activated", repoFullName))
}

// containFailedKeys returns the owner/repo names of repos whose trigger-account
// containment failed, for repos.ConvergeConfig.ExcludeRepos.
func containFailedKeys(failed []repos.ConvergeResult) []string {
	keys := make([]string, 0, len(failed))
	for _, r := range failed {
		keys = append(keys, r.Owner+"/"+r.Repo)
	}
	return keys
}

// pollerMergeGrantRejected reports whether err is GitLab refusing the legacy
// Poller per-user protected-branch merge grant (GitLab.com Free), as opposed
// to an unreadable project or a transient failure.
func pollerMergeGrantRejected(err error) bool {
	var grantErr *repos.PollerMergeGrantError
	if !errors.As(err, &grantErr) {
		return false
	}
	var apiErr *gitlab.APIError
	if !errors.As(grantErr.Err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusBadRequest, http.StatusPaymentRequired, http.StatusForbidden, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// containGitLabTriggerAccount is the containment-only recovery that install
// runs for every targeted GitLab repository before the Poller inventory is
// read and before any other install stage can fail or skip it. Under the
// project lease it revokes recorded bootstrap tokens and restores Developer
// access for an interrupted trigger-account bootstrap, and it creates nothing.
// It does not touch the webhook fast path or any other credential. Clients
// that are not the live GitLab client (tests) have no trigger-account API and
// are skipped.
func containGitLabTriggerAccount(ctx context.Context, opts *reposInstallConfig, client forge.Client, printer *ui.Printer, owner, repo string) (err error) {
	glClient, ok := client.(*gitlab.LiveClient)
	if !ok {
		return nil
	}
	repoFullName := owner + "/" + repo
	_, exists, err := client.GetRepoVariable(ctx, owner, repo, forge.VarGitLabTriggerAccount)
	if err != nil {
		return fmt.Errorf("reading %s: %w", forge.VarGitLabTriggerAccount, repos.SafeAPIError("reading the trigger account ledger", err))
	}
	if !exists {
		return nil
	}
	if opts.dryRun {
		printer.StepInfo(fmt.Sprintf("[%s] Would contain an unfinished GitLab trigger account bootstrap", repoFullName))
		return nil
	}
	lease, release, err := repos.LockGitLabProjectLease(ctx, client, owner, repo)
	if err != nil {
		return fmt.Errorf("serializing GitLab trigger account recovery with other installers: %w", err)
	}
	defer release(&err)
	triggerOwner := gitlablifecycle.NewTriggerOwner(glClient)
	if err := repos.ContainGitLabTriggerAccount(ctx, triggerOwner.ContainmentOptions(owner, repo, lease)); err != nil {
		return fmt.Errorf("containing the GitLab trigger account: %w", err)
	}
	return nil
}

// appendGitLabTriggerAccountStatus replaces the ledger-only trigger account
// diagnosis with one that also checks the recorded resources against GitLab.
// It reports whether the trigger account now shows drift; the caller counts a
// repo once, only on its no-drift to drift transition.
func appendGitLabTriggerAccountStatus(ctx context.Context, client forge.Client, st *repos.RepoStatus) bool {
	glClient, ok := client.(*gitlab.LiveClient)
	if !ok {
		return false
	}
	info, err := client.GetRepo(ctx, st.Owner, st.Repo)
	if err != nil {
		// Without the default branch the live checks cannot run. A ledger-only
		// "ready" must not stand as if it had been verified against GitLab.
		if st.GitLabTriggerAccount != nil && st.GitLabTriggerAccount.State == repos.GitLabTriggerStateReady {
			repos.ApplyGitLabTriggerAccountStatus(st, repos.GitLabTriggerAccountStatus{
				State:   repos.GitLabTriggerStateUnreadable,
				Reasons: []string{"could not read the repository to verify the trigger account against GitLab; the ledger alone is not proof it is ready"},
			})
		}
		return hasGitLabTriggerAccountDrift(st)
	}
	api := gitlablifecycle.NewTriggerAccountAPI(glClient)
	repos.ApplyGitLabTriggerAccountStatus(st, repos.DiagnoseGitLabTriggerAccount(ctx, client, api, st.Owner, st.Repo, info.DefaultBranch))
	return hasGitLabTriggerAccountDrift(st)
}

func hasGitLabTriggerAccountDrift(st *repos.RepoStatus) bool {
	for _, d := range st.Drifts {
		if d.Field == "gitlab-trigger-account" {
			return true
		}
	}
	return false
}
