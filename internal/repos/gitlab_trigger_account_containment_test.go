package repos

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/forge"
)

// containWithSinkOpts is containOpts plus the sink remover that the live
// containment options carry.
func (e *trigEnv) containWithSinkOpts() GitLabTriggerAccountOptions {
	return e.unpublishClearsAttributes(e.containOpts())
}

// trigRevokeFailAPI fails the revocation of the listed trigger tokens only.
type trigRevokeFailAPI struct {
	*trigFakeGitLab
	failIDs map[int64]bool
}

func (a trigRevokeFailAPI) RevokePipelineTriggerToken(ctx context.Context, o, r string, id int64) error {
	if a.failIDs[id] {
		return errTrigInjected
	}
	return a.trigFakeGitLab.RevokePipelineTriggerToken(ctx, o, r, id)
}

// Containment-only recovery removes a published credential, and revokes its
// owned trigger, when the override restriction is weakened or unreadable,
// without any convergence, deferral or create path. A healthy project keeps it.
func TestContainGitLabTriggerAccount_ContainsPublishedCredentialWhenOverrideRoleUnsafe(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(e *trigEnv){
		"weakened role": func(e *trigEnv) {
			e.state.PipelineVarOverrideRoles[trigOverrideKey] = forge.PipelineVarOverrideDeveloper
		},
		"unreadable role": func(e *trigEnv) {
			e.state.Errors = map[string]error{"GetPipelineVariablesMinimumOverrideRole": errors.New("boom")}
		},
	}
	for name, weaken := range cases {
		t.Run(name, func(t *testing.T) {
			e := newTrigEnv()
			e.useRecreatingSink()
			res, err := ConvergeGitLabTriggerAccount(ctx, e.unpublishClearsAttributes(e.opts()))
			require.NoError(t, err)
			require.True(t, e.sinkPresent())
			require.Contains(t, e.f.triggers, res.TriggerTokenID)

			weaken(e)
			require.NoError(t, ContainGitLabTriggerAccount(ctx, e.containWithSinkOpts()))

			assert.False(t, e.sinkPresent(), "the published credential is removed")
			assert.NotContains(t, e.f.triggers, res.TriggerTokenID, "the owned trigger is revoked")
			st, ok := e.ledger(t)
			require.True(t, ok, "the ledger keeps the cleanup obligation")
			assert.True(t, st.PublishStarted)
			assert.False(t, st.TriggerPublished)
		})
	}

	t.Run("healthy project keeps the credential", func(t *testing.T) {
		e := newTrigEnv()
		e.useRecreatingSink()
		res, err := ConvergeGitLabTriggerAccount(ctx, e.unpublishClearsAttributes(e.opts()))
		require.NoError(t, err)
		require.NoError(t, ContainGitLabTriggerAccount(ctx, e.containWithSinkOpts()))
		assert.True(t, e.sinkPresent())
		assert.Contains(t, e.f.triggers, res.TriggerTokenID)
	})
}

// A failed replacement leaves the retired generation's live token in the sink
// while the current generation never published. Ordinary convergence on a
// healthy project must contain that sink when it becomes unsafe, before the
// replacement is created, even when replacement creation fails again.
func TestConvergeGitLabTriggerAccount_UnsafeSinkOwnedByRetiredGenerationIsContainedBeforeReplacement(t *testing.T) {
	ctx := context.Background()
	e := newTrigEnv()
	e.useRecreatingSink()
	res, err := ConvergeGitLabTriggerAccount(ctx, e.unpublishClearsAttributes(e.opts()))
	require.NoError(t, err)

	e.f.fail["CreateProjectServiceAccount"] = true
	_, err = RotateGitLabTriggerAccount(ctx, e.unpublishClearsAttributes(e.opts()))
	require.Error(t, err)

	st, ok := e.ledger(t)
	require.True(t, ok)
	require.False(t, st.PublishStarted, "the replacement never published")
	require.Len(t, st.Retired, 1)
	require.True(t, st.Retired[0].PublishStarted)
	require.True(t, e.sinkPresent())
	require.Contains(t, e.f.triggers, res.TriggerTokenID)

	e.state.SecretProtections[trigSinkKey] = forge.SecretProtection{Exists: true, Protected: true} // unmasked
	e.f.fail["CreateProjectServiceAccount"] = true                                                 // injected failures are one-shot
	_, err = ConvergeGitLabTriggerAccount(ctx, e.unpublishClearsAttributes(e.opts()))
	require.Error(t, err, "the replacement still fails")

	assert.False(t, e.sinkPresent(), "the retired generation's unsafe sink is removed")
	assert.NotContains(t, e.f.triggers, res.TriggerTokenID, "the retired generation's trigger is revoked")
	st, ok = e.ledger(t)
	require.True(t, ok)
	require.Len(t, st.Retired, 1, "retirement stays recorded")
	assert.False(t, st.Retired[0].TriggerPublished)
}

// One generation's failed revocation does not suppress the others, and the
// ledger records exactly the revocations that completed.
func TestContainGitLabTriggerAccount_RevocationFailureInOneGenerationDoesNotSuppressOthers(t *testing.T) {
	ctx := context.Background()
	// Two publishing generations: the rotation's retirement fails, so the
	// previous generation and its live trigger stay recorded.
	setup := func(t *testing.T) (e *trigEnv, previous, current GitLabTriggerAccountResult) {
		e = newTrigEnv()
		e.useRecreatingSink()
		var err error
		previous, err = ConvergeGitLabTriggerAccount(ctx, e.unpublishClearsAttributes(e.opts()))
		require.NoError(t, err)
		e.f.fail["RevokePipelineTriggerToken"] = true
		_, err = RotateGitLabTriggerAccount(ctx, e.unpublishClearsAttributes(e.opts()))
		require.Error(t, err)
		delete(e.f.fail, "RevokePipelineTriggerToken")
		st, ok := e.ledger(t)
		require.True(t, ok)
		require.Len(t, st.Retired, 1)
		require.True(t, st.PublishStarted)
		require.True(t, st.Retired[0].PublishStarted)
		current = GitLabTriggerAccountResult{TriggerTokenID: st.TriggerTokenID}
		require.Contains(t, e.f.triggers, previous.TriggerTokenID)
		require.Contains(t, e.f.triggers, current.TriggerTokenID)
		e.state.PipelineVarOverrideRoles[trigOverrideKey] = forge.PipelineVarOverrideDeveloper
		return e, previous, current
	}

	t.Run("current generation fails", func(t *testing.T) {
		e, previous, current := setup(t)
		o := e.containWithSinkOpts()
		o.API = trigRevokeFailAPI{trigFakeGitLab: e.f, failIDs: map[int64]bool{current.TriggerTokenID: true}}
		err := ContainGitLabTriggerAccount(ctx, o)
		require.ErrorIs(t, err, errTrigInjected)

		assert.False(t, e.sinkPresent())
		assert.Contains(t, e.f.triggers, current.TriggerTokenID, "the failed revocation stays live")
		assert.NotContains(t, e.f.triggers, previous.TriggerTokenID, "the retired generation is still revoked")
		st, _ := e.ledger(t)
		assert.True(t, st.TriggerPublished, "the unresolved obligation is persisted")
		assert.False(t, st.Retired[0].TriggerPublished, "the completed revocation is persisted")

		require.NoError(t, ContainGitLabTriggerAccount(ctx, e.containWithSinkOpts()))
		assert.NotContains(t, e.f.triggers, current.TriggerTokenID, "a retry finishes the revocation")
	})

	t.Run("retired generation fails", func(t *testing.T) {
		e, previous, current := setup(t)
		o := e.containWithSinkOpts()
		o.API = trigRevokeFailAPI{trigFakeGitLab: e.f, failIDs: map[int64]bool{previous.TriggerTokenID: true}}
		err := ContainGitLabTriggerAccount(ctx, o)
		require.ErrorIs(t, err, errTrigInjected)

		assert.NotContains(t, e.f.triggers, current.TriggerTokenID, "the current generation is still revoked")
		assert.Contains(t, e.f.triggers, previous.TriggerTokenID)
		st, _ := e.ledger(t)
		assert.False(t, st.TriggerPublished)
		assert.True(t, st.Retired[0].TriggerPublished, "the unresolved obligation is persisted")
	})
}

// A failed variable deletion does not suppress revocation.
func TestContainGitLabTriggerAccount_VariableDeletionFailureDoesNotSuppressRevocation(t *testing.T) {
	ctx := context.Background()
	e := newTrigEnv()
	e.useRecreatingSink()
	res, err := ConvergeGitLabTriggerAccount(ctx, e.unpublishClearsAttributes(e.opts()))
	require.NoError(t, err)
	e.state.PipelineVarOverrideRoles[trigOverrideKey] = forge.PipelineVarOverrideDeveloper

	o := e.containOpts()
	o.UnpublishTriggerToken = func(context.Context) error { return errTrigInjected }
	require.ErrorIs(t, ContainGitLabTriggerAccount(ctx, o), errTrigInjected)

	assert.True(t, e.sinkPresent(), "the variable that could not be removed stays")
	assert.NotContains(t, e.f.triggers, res.TriggerTokenID, "the trigger is revoked anyway")
}
