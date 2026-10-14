package repos

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/forge"
)

// grantClient wraps handoffClient so individual protected-branch calls can
// misbehave: a grant that is applied and then reports failure, a grant that
// silently does nothing or adds an entry without an ID, or a failing reread.
type grantClient struct {
	*handoffClient
	applyThenFail bool
	// failKeepsID makes an applyThenFail grant still report the entry it created.
	failKeepsID bool
	grantNoop   bool
	grantNoID   bool
	// adminRace makes the grant a no-op that reports mutated=false after an
	// administrator's entry for the user appeared in the window.
	adminRace bool
	// adminThenFail makes the grant add an administrator's entry for the user
	// and then fail with a timeout without applying anything of its own.
	adminThenFail bool
	// lateApply makes the grant fail with a timeout that reports no entry; the
	// request is committed only after the first reread that follows it.
	lateApply bool
	// refusedThenFailReread makes the grant a sent PATCH that the forge refuses
	// with 403 (nothing created), after which the confirming reread fails.
	refusedThenFailReread bool
	lateUser              int
	lateSent              bool
	// removedAfterGrant makes an administrator remove the entry the grant
	// created before the confirming reread.
	removedAfterGrant bool
	failGetFrom       int // fail GetProtectedBranch from this call on (1-based)
	gets              int
}

func (c *grantClient) GrantProtectedBranchMergeUserEntry(ctx context.Context, owner, repo, branch string, userID int) (int, bool, error) {
	switch {
	case c.refusedThenFailReread:
		c.failGetFrom = c.gets + 1
		return 0, true, fmt.Errorf("403: %w", forge.ErrForbidden)
	case c.adminThenFail:
		rule := c.ProtectedBranchRules[owner+"/"+repo+"/"+branch]
		rule.MergeAccessLevels = append(rule.MergeAccessLevels, forge.ProtectedBranchAccess{ID: 77, UserID: userID})
		return 0, true, errors.New("context deadline exceeded")
	case c.lateApply:
		c.lateSent = true
		c.lateUser = userID
		return 0, true, errors.New("context deadline exceeded")
	case c.grantNoop:
		return 0, true, nil
	case c.grantNoID:
		rule := c.ProtectedBranchRules[owner+"/"+repo+"/"+branch]
		rule.MergeAccessLevels = append(rule.MergeAccessLevels, forge.ProtectedBranchAccess{UserID: userID})
		return 0, true, nil
	case c.adminRace:
		rule := c.ProtectedBranchRules[owner+"/"+repo+"/"+branch]
		rule.MergeAccessLevels = append(rule.MergeAccessLevels, forge.ProtectedBranchAccess{ID: 77, UserID: userID})
		return 0, false, nil
	}
	id, mutated, err := c.handoffClient.GrantProtectedBranchMergeUserEntry(ctx, owner, repo, branch, userID)
	if err == nil && c.removedAfterGrant && id > 0 {
		rule := c.ProtectedBranchRules[owner+"/"+repo+"/"+branch]
		kept := rule.MergeAccessLevels[:0]
		for _, l := range rule.MergeAccessLevels {
			if l.ID != id {
				kept = append(kept, l)
			}
		}
		rule.MergeAccessLevels = kept
	}
	if err == nil && c.applyThenFail {
		if c.failKeepsID {
			return id, mutated, errors.New("502 bad gateway")
		}
		return 0, mutated, errors.New("502 bad gateway")
	}
	return id, mutated, err
}

func (c *grantClient) GetProtectedBranch(ctx context.Context, owner, repo, branch string) (*forge.ProtectedBranchRule, error) {
	c.gets++
	if c.failGetFrom != 0 && c.gets >= c.failGetFrom {
		return nil, errors.New("500 internal error")
	}
	rule, err := c.handoffClient.GetProtectedBranch(ctx, owner, repo, branch)
	if c.lateSent {
		// Answer this read from before the request committed, then commit it.
		c.lateSent = false
		live := c.ProtectedBranchRules[owner+"/"+repo+"/"+branch]
		live.MergeAccessLevels = append(live.MergeAccessLevels, forge.ProtectedBranchAccess{ID: 88, UserID: c.lateUser})
	}
	return rule, err
}

// grantHandoff is the handoff fake with the live adapter's read-only
// protected-branch admission check, so the policy must establish access
// itself.
type grantHandoff struct {
	*handoffFake
	client forge.Client
}

func (g grantHandoff) EnsureReplacementPollerAccess(ctx context.Context, owner, repo string, uid int64) error {
	g.prepareCalls++
	err := VerifyGitLabPollerProtectedPipelineAccess(ctx, g.client, owner, repo, int(uid))
	var denied *PollerPipelineAccessDeniedError
	if errors.As(err, &denied) {
		return &PollerGenerationUnsafeError{Reason: denied.Error()}
	}
	return err
}

const grantAdminEntryID = 2

func ruleKey() string { return handoffOwner + "/" + handoffRepo + "/main" }

// newGrantHandoff returns a handoff whose default branch has an exact-name rule
// allowing Maintainers and the current Poller user to merge, but not all
// Developers.
func newGrantHandoff() (grantHandoff, *grantClient) {
	f := newHandoffFake()
	f.c.ProtectedBranchRules[ruleKey()] = &forge.ProtectedBranchRule{
		Name: "main",
		MergeAccessLevels: []forge.ProtectedBranchAccess{
			{ID: 1, AccessLevel: forge.GitLabAccessLevelMaintainer},
			{ID: grantAdminEntryID, UserID: int(handoffCurrentUID)},
		},
	}
	c := &grantClient{handoffClient: f.c}
	return grantHandoff{handoffFake: f, client: c}, c
}

func (g grantHandoff) run(t *testing.T) (PollerHandoffResult, error) {
	t.Helper()
	ctx := context.Background()
	lease, release, err := LockGitLabProjectLease(ctx, g.client, handoffOwner, handoffRepo)
	require.NoError(t, err)
	defer release(nil)
	return handoffPollerGeneration(ctx, g.client, g, lease, handoffOwner, handoffRepo, &credentialRedactor{})
}

func (g grantHandoff) mergeEntries() []forge.ProtectedBranchAccess {
	return g.c.ProtectedBranchRules[ruleKey()].MergeAccessLevels
}

func originalEntries() []forge.ProtectedBranchAccess {
	return []forge.ProtectedBranchAccess{
		{ID: 1, AccessLevel: forge.GitLabAccessLevelMaintainer},
		{ID: grantAdminEntryID, UserID: int(handoffCurrentUID)},
	}
}

// A fresh handoff adds exactly one merge entry for the replacement, leaves the
// role and existing user entries alone, records the entry as installer-owned,
// and completes.
func TestHandoffPollerGeneration_GrantsReplacementBranchAccess(t *testing.T) {
	g, _ := newGrantHandoff()

	res, err := g.run(t)
	require.NoError(t, err)
	require.NotNil(t, res.Trigger, res.DeferReason)

	entries := g.mergeEntries()
	require.Len(t, entries, 3)
	assert.Equal(t, originalEntries(), entries[:2], "existing entries unchanged")
	added := entries[2]
	assert.Equal(t, int(handoffNewUID), added.UserID)
	assert.Zero(t, added.AccessLevel, "no role-based access is added")
	assert.Equal(t, []forge.ProtectedBranchMergeGrantRecord{{Owner: handoffOwner, Repo: handoffRepo, Branch: "main", UserID: int(handoffNewUID)}}, g.c.GrantedProtectedBranchMergeUsers)
	assert.Empty(t, g.c.RevokedProtectedBranchMergeEntries)

	st := g.state(t)
	require.NotNil(t, st.Pending)
	assert.Equal(t, PollerPhaseVerified, st.Pending.Phase)
	assert.Equal(t, &PollerBranchGrant{Branch: "main", EntryID: int64(added.ID)}, st.Pending.BranchGrant, "the grant survives every later phase write")
	assert.Contains(t, fmt.Sprint(res.Details), "Granted the replacement GitLab Poller")

	next, err := beginPollerCutover(st, handoffNewUID)
	require.NoError(t, err)
	assert.Equal(t, &PollerBranchGrant{Branch: "main", EntryID: int64(added.ID)}, next.CurrentBranchGrant)
}

// A failure after the grant quarantines the generation and revokes only the
// installer-added entry.
func TestHandoffPollerGeneration_QuarantineRevokesOnlyInstallerGrant(t *testing.T) {
	g, _ := newGrantHandoff()
	g.probeErr = errors.New("403 forbidden")

	res, err := g.run(t)
	require.NoError(t, err)
	assert.Nil(t, res.Trigger)
	assert.Equal(t, originalEntries(), g.mergeEntries(), "only the installer's entry is removed")
	require.Len(t, g.c.RevokedProtectedBranchMergeEntries, 1)
	assert.Equal(t, int(handoffNewUID), g.c.RevokedProtectedBranchMergeEntries[0].UserID)
	st := g.state(t)
	assert.Equal(t, PollerPhaseQuarantined, st.Pending.Phase)
	assert.Nil(t, st.Pending.BranchGrant)
}

// A quarantine whose grant revocation fails keeps the grant recorded and names
// the exact entry in the operator instructions.
func TestHandoffPollerGeneration_QuarantineKeepsUnrevokedGrant(t *testing.T) {
	g, _ := newGrantHandoff()
	g.probeErr = errors.New("403 forbidden")
	g.c.Errors = map[string]error{"RevokeProtectedBranchMergeUserEntry": errors.New("500 glpat-secret")}

	res, err := g.run(t)
	require.NoError(t, err)
	st := g.state(t)
	require.NotNil(t, st.Pending.BranchGrant)
	assert.Contains(t, res.DeferReason, fmt.Sprintf("merge-access entry ID %d", st.Pending.BranchGrant.EntryID))
	assert.Contains(t, st.Pending.Reason, fmt.Sprintf("merge-access entry ID %d", st.Pending.BranchGrant.EntryID))
	assert.NotContains(t, res.DeferReason, "glpat-secret")
}

// A branch protected only by a wildcard rule is never edited: the handoff
// defers with an explanation.
func TestHandoffPollerGeneration_WildcardRuleDefersWithoutGrant(t *testing.T) {
	g, _ := newGrantHandoff()
	g.c.ProtectedBranchRules[ruleKey()].Name = "main*"

	res, err := g.run(t)
	require.NoError(t, err)
	assert.Nil(t, res.Trigger)
	assert.Contains(t, res.DeferReason, "wildcard rule")
	assert.Contains(t, res.DeferReason, "polling schedules remain in effect")
	assert.Empty(t, g.c.GrantedProtectedBranchMergeUsers)
	assert.Equal(t, originalEntries(), g.mergeEntries())
	assert.Equal(t, []string{"create-account", "inventory"}, g.events, "the account is not raised")
	assert.Equal(t, PollerPhaseAccountRecorded, g.state(t).Pending.Phase)
}

// A refused grant defers with an explanation and leaves the rule unchanged.
func TestHandoffPollerGeneration_ForbiddenGrantDefers(t *testing.T) {
	g, _ := newGrantHandoff()
	g.c.Errors = map[string]error{"GrantProtectedBranchMergeUser": fmt.Errorf("403: %w", forge.ErrForbidden)}

	res, err := g.run(t)
	require.NoError(t, err)
	assert.Nil(t, res.Trigger)
	assert.Contains(t, res.DeferReason, "never widens access to Developers")
	assert.Equal(t, originalEntries(), g.mergeEntries())
	assert.Equal(t, []string{"create-account", "inventory"}, g.events)
	st := g.state(t)
	assert.Equal(t, PollerPhaseAccountRecorded, st.Pending.Phase)
	assert.Nil(t, st.Pending.BranchGrant)
}

// A definitively refused PATCH created nothing, so the attempt is cleared even
// when the follow-up reread fails; that read error is still returned, and a
// later install can retry and complete.
func TestHandoffPollerGeneration_RefusedGrantWithFailedRereadClearsAttempt(t *testing.T) {
	g, c := newGrantHandoff()
	c.refusedThenFailReread = true

	res, err := g.run(t)
	require.ErrorContains(t, err, "rereading protected branch")
	assert.Nil(t, res.Trigger)
	assert.Equal(t, originalEntries(), g.mergeEntries())
	assert.Empty(t, g.c.RevokedProtectedBranchMergeEntries)
	assert.Nil(t, g.state(t).Pending.BranchGrant, "an explicit refusal leaves no obligation")

	// The later install is not blocked by an unresolved attempt.
	c.refusedThenFailReread = false
	c.failGetFrom = 0
	res, err = g.run(t)
	require.NoError(t, err)
	assert.Empty(t, res.DeferReason)
	require.Len(t, g.mergeEntries(), 3)
	grant := g.state(t).Pending.BranchGrant
	if grant != nil {
		assert.False(t, grant.unresolved())
	}
}

// A grant that was applied but reported failure with the entry it created is
// undone by that exact entry and the run fails, leaving the account resumable.
func TestHandoffPollerGeneration_AppliedFailedGrantIsUndone(t *testing.T) {
	g, c := newGrantHandoff()
	c.applyThenFail = true
	c.failKeepsID = true

	res, err := g.run(t)
	require.Error(t, err)
	assert.Nil(t, res.Trigger)
	assert.Equal(t, originalEntries(), g.mergeEntries())
	require.Len(t, g.c.RevokedProtectedBranchMergeEntries, 1)
	assert.Nil(t, g.state(t).Pending.BranchGrant)
}

// A grant that was applied but failed without identifying its entry is not
// attributed by an entry-ID difference: nothing is revoked and the unresolved
// attempt stays recorded for the operator.
func TestHandoffPollerGeneration_AppliedFailedGrantWithoutIDStaysUnresolved(t *testing.T) {
	g, c := newGrantHandoff()
	c.applyThenFail = true

	res, err := g.run(t)
	require.Error(t, err)
	assert.Nil(t, res.Trigger)
	assert.Empty(t, g.c.RevokedProtectedBranchMergeEntries)
	assert.Len(t, g.mergeEntries(), 3)
	grant := g.state(t).Pending.BranchGrant
	require.NotNil(t, grant)
	assert.True(t, grant.unresolved())
}

// An administrator's existing entry for the replacement is neither duplicated
// nor recorded, and a later quarantine never removes it.
func TestHandoffPollerGeneration_ExistingUserEntryIsNotOwned(t *testing.T) {
	g, _ := newGrantHandoff()
	rule := g.c.ProtectedBranchRules[ruleKey()]
	rule.MergeAccessLevels = append(rule.MergeAccessLevels, forge.ProtectedBranchAccess{ID: 3, UserID: int(handoffNewUID)})
	g.probeErr = errors.New("403 forbidden")

	_, err := g.run(t)
	require.NoError(t, err)
	assert.Empty(t, g.c.GrantedProtectedBranchMergeUsers)
	assert.Empty(t, g.c.RevokedProtectedBranchMergeEntries)
	assert.Len(t, g.mergeEntries(), 3)
	assert.Nil(t, g.state(t).Pending.BranchGrant)
}

// An unprotected branch, or one where Developers may merge, needs no grant.
func TestHandoffPollerGeneration_NoGrantWhenAlreadyAdmitted(t *testing.T) {
	for name, rule := range map[string]*forge.ProtectedBranchRule{
		"unprotected":      nil,
		"developers merge": {Name: "main", MergeAccessLevels: []forge.ProtectedBranchAccess{{ID: 1, AccessLevel: forge.GitLabAccessLevelDeveloper}}},
		"developers push":  {Name: "main", PushAccessLevels: []forge.ProtectedBranchAccess{{ID: 1, AccessLevel: forge.GitLabAccessLevelDeveloper}}},
		"default preset":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			g, _ := newGrantHandoff()
			delete(g.c.ProtectedBranchRules, ruleKey())
			if name == "unprotected" {
				g.c.ProtectedBranches[ruleKey()] = false
			}
			if rule != nil {
				g.c.ProtectedBranchRules[ruleKey()] = rule
			}
			res, err := g.run(t)
			require.NoError(t, err)
			if name != "unprotected" {
				// An unprotected default branch fails the separate
				// trigger-safety invariants; only the grant matters here.
				require.NotNil(t, res.Trigger, res.DeferReason)
			}
			assert.Empty(t, g.c.GrantedProtectedBranchMergeUsers)
			assert.Nil(t, g.state(t).Pending.BranchGrant)
		})
	}
}

// The grant attempt is recorded before the rule is changed: when that record
// cannot be written, nothing is granted.
func TestHandoffPollerGeneration_UnrecordedAttemptMakesNoGrant(t *testing.T) {
	g, _ := newGrantHandoff()
	g.c.failWrite = 3 // account_requested, account_recorded, then the attempt record

	_, err := g.run(t)
	require.Error(t, err)
	assert.Empty(t, g.c.GrantedProtectedBranchMergeUsers)
	assert.Equal(t, originalEntries(), g.mergeEntries())
	assert.Empty(t, g.c.RevokedProtectedBranchMergeEntries)
}

// A grant whose identified entry cannot be recorded is undone by its exact
// entry, and the attempt record that remains blocks later runs: recovery never
// clears it by inference, whatever time has passed.
func TestHandoffPollerGeneration_UnrecordedEntryIsUndoneAndStaysUnresolved(t *testing.T) {
	g, _ := newGrantHandoff()
	g.c.failWrite = 4 // the write recording the identified entry

	_, err := g.run(t)
	require.Error(t, err)
	assert.Equal(t, originalEntries(), g.mergeEntries())
	require.Len(t, g.c.RevokedProtectedBranchMergeEntries, 1)
	require.True(t, g.state(t).Pending.BranchGrant.unresolved())

	for range 2 {
		res, err := g.run(t)
		require.ErrorContains(t, err, "is unresolved")
		assert.Nil(t, res.Trigger)
		require.True(t, g.state(t).Pending.BranchGrant.unresolved())
		assert.Len(t, g.c.GrantedProtectedBranchMergeUsers, 1, "no second grant is attempted")
	}
}

// A grant that was applied but reported failure, whose revocation also fails,
// keeps the known entry recorded so a later abandonment still removes it.
func TestHandoffPollerGeneration_AppliedFailedGrantWithFailedRevokeKeepsEntry(t *testing.T) {
	g, c := newGrantHandoff()
	c.applyThenFail = true
	c.failKeepsID = true
	g.c.Errors = map[string]error{"RevokeProtectedBranchMergeUserEntry": errors.New("500")}

	_, err := g.run(t)
	require.Error(t, err)
	grant := g.state(t).Pending.BranchGrant
	require.NotNil(t, grant, "the cleanup obligation is not lost")
	assert.Positive(t, grant.EntryID)
	assert.Len(t, g.mergeEntries(), 3)

	// The next install does not see an unowned, pre-existing grant: quarantining
	// or abandoning the generation removes the recorded entry.
	g.c.Errors = nil
	c.applyThenFail = false
	p, note := (&pollerGenerationRun{ctx: context.Background(), client: g.client, owner: handoffOwner, repo: handoffRepo}).releasePendingBranchGrant(*g.state(t).Pending)
	assert.Empty(t, note)
	assert.Nil(t, p.BranchGrant)
	assert.Equal(t, originalEntries(), g.mergeEntries())
}

// A successful grant whose forge-reported entry cannot be confirmed by the
// reread is persisted as the identified cleanup obligation, so a later
// quarantine or abandonment still removes exactly that entry.
func TestHandoffPollerGeneration_FailedRereadPersistsIdentifiedEntry(t *testing.T) {
	g, c := newGrantHandoff()
	c.failGetFrom = 3 // the read-only check, the grant's read, then the reread

	res, err := g.run(t)
	require.ErrorContains(t, err, "may remain")
	assert.Nil(t, res.Trigger, "elevation stays blocked")
	grant := g.state(t).Pending.BranchGrant
	require.NotNil(t, grant)
	require.False(t, grant.unresolved(), "the entry the grant reported is recorded")
	assert.Empty(t, grant.PriorEntryIDs)
	require.Len(t, g.mergeEntries(), 3)
	assert.Equal(t, int64(g.mergeEntries()[2].ID), grant.EntryID)

	// Quarantine or abandonment now removes exactly that entry.
	c.failGetFrom = 0
	p, note := (&pollerGenerationRun{ctx: context.Background(), client: g.client, owner: handoffOwner, repo: handoffRepo}).releasePendingBranchGrant(*g.state(t).Pending)
	assert.Empty(t, note)
	assert.Nil(t, p.BranchGrant)
	assert.Equal(t, originalEntries(), g.mergeEntries())
	require.Len(t, g.c.RevokedProtectedBranchMergeEntries, 1)
	assert.Equal(t, int(grant.EntryID), g.c.RevokedProtectedBranchMergeEntries[0].EntryID)
}

// When that identified entry cannot be recorded either, the exact entry is
// revoked immediately and the attempt stays recorded.
func TestHandoffPollerGeneration_FailedRereadAndRecordRevokesIdentifiedEntry(t *testing.T) {
	g, c := newGrantHandoff()
	c.failGetFrom = 3
	g.c.failWrite = 4 // the write recording the identified entry

	_, err := g.run(t)
	require.Error(t, err)
	require.Len(t, g.c.RevokedProtectedBranchMergeEntries, 1)
	assert.Equal(t, originalEntries(), g.mergeEntries())
	require.True(t, g.state(t).Pending.BranchGrant.unresolved(), "the attempt stays recorded")
}

// A grant that reported its entry keeps that identification when the reread
// succeeds but no longer admits the account (the entry was removed in the
// meantime): the identified entry is recorded as the cleanup obligation.
func TestHandoffPollerGeneration_KnownEntryNotAdmittedByRereadIsRecorded(t *testing.T) {
	g, c := newGrantHandoff()
	c.removedAfterGrant = true

	res, err := g.run(t)
	require.ErrorContains(t, err, "did not take effect")
	assert.Nil(t, res.Trigger, "elevation stays blocked")
	grant := g.state(t).Pending.BranchGrant
	require.NotNil(t, grant)
	require.False(t, grant.unresolved(), "the entry the grant reported is recorded")
	assert.Positive(t, grant.EntryID)
}

// When that identified entry cannot be recorded, the exact entry is revoked and
// the attempt stays recorded.
func TestHandoffPollerGeneration_KnownEntryNotAdmittedAndRecordFailsRevokes(t *testing.T) {
	g, c := newGrantHandoff()
	c.removedAfterGrant = true
	g.c.failWrite = 4 // the write recording the identified entry

	_, err := g.run(t)
	require.ErrorContains(t, err, "did not take effect")
	require.Len(t, g.c.RevokedProtectedBranchMergeEntries, 1)
	require.True(t, g.state(t).Pending.BranchGrant.unresolved(), "the attempt stays recorded")
}

// An unresolved attempt is never cleared because another entry for the account
// appeared: recovery neither claims nor revokes it, keeps the obligation
// recorded, blocks the handoff, and names the observed entry for the operator.
func TestHandoffPollerGeneration_AdminGrantAfterSnapshotKeepsObligation(t *testing.T) {
	seed := func(t *testing.T) grantHandoff {
		g, _ := newGrantHandoff()
		g.c.ProjectMemberAccess[handoffNewUID] = forge.GitLabAccessLevelDeveloper
		rule := g.c.ProtectedBranchRules[ruleKey()]
		rule.MergeAccessLevels = append(rule.MergeAccessLevels, forge.ProtectedBranchAccess{ID: 77, UserID: int(handoffNewUID)})
		g.seed(t, PollerGenerationState{CurrentUserID: handoffCurrentUID, Pending: &PendingPollerGeneration{
			UserID: handoffNewUID, Phase: PollerPhaseAccountRecorded,
			BranchGrant: &PollerBranchGrant{Branch: "main", PriorEntryIDs: []int{1, grantAdminEntryID}},
		}})
		return g
	}
	adminEntry := forge.ProtectedBranchAccess{ID: 77, UserID: int(handoffNewUID)}

	t.Run("handoff", func(t *testing.T) {
		g := seed(t)
		for range 2 {
			res, err := g.run(t)
			require.ErrorContains(t, err, "entry ID(s) [77]")
			require.ErrorContains(t, err, "remove the account")
			assert.Nil(t, res.Trigger)
			assert.Empty(t, g.c.RevokedProtectedBranchMergeEntries)
			assert.Contains(t, g.mergeEntries(), adminEntry)
			require.True(t, g.state(t).Pending.BranchGrant.unresolved(), "the obligation stays recorded")
		}
	})

	t.Run("quarantine", func(t *testing.T) {
		g := seed(t)
		p, note := (&pollerGenerationRun{ctx: context.Background(), client: g.client, owner: handoffOwner, repo: handoffRepo}).releasePendingBranchGrant(*g.state(t).Pending)
		assert.Contains(t, note, "entry ID(s) [77]", "the observed entry is named in the recovery text")
		require.NotNil(t, p.BranchGrant)
		assert.True(t, p.BranchGrant.unresolved())
		assert.Empty(t, g.c.RevokedProtectedBranchMergeEntries)
		assert.Contains(t, g.mergeEntries(), adminEntry)
	})
}

// A timed-out grant whose entry appears only after the reread finds nothing
// keeps the unresolved attempt recorded, so a later release does not clear the
// obligation and orphan the entry.
func TestHandoffPollerGeneration_LateAppliedGrantKeepsUnresolvedAttempt(t *testing.T) {
	g, c := newGrantHandoff()
	c.lateApply = true

	res, err := g.run(t)
	require.Error(t, err)
	assert.Nil(t, res.Trigger)
	assert.Empty(t, g.c.RevokedProtectedBranchMergeEntries, "nothing was visible to revoke at the reread")
	grant := g.state(t).Pending.BranchGrant
	require.NotNil(t, grant, "the cleanup obligation is not lost")
	require.True(t, grant.unresolved())
	assert.Equal(t, []int{1, grantAdminEntryID}, grant.PriorEntryIDs)
	assert.Len(t, g.mergeEntries(), 3, "the request committed after the reread")

	// The entry is not attributable, so releasing the generation neither claims
	// nor removes it, and keeps the obligation recorded with the entry named.
	run := &pollerGenerationRun{ctx: context.Background(), client: g.client, owner: handoffOwner, repo: handoffRepo}
	p, note := run.releasePendingBranchGrant(*g.state(t).Pending)
	assert.Contains(t, note, "entry ID(s) [88]")
	require.NotNil(t, p.BranchGrant)
	assert.True(t, p.BranchGrant.unresolved())
	assert.Empty(t, g.c.RevokedProtectedBranchMergeEntries)
	assert.Len(t, g.mergeEntries(), 3)
}

// A timed-out PATCH can commit after recovery's own read finds nothing, however
// long the attempt has been recorded: no client-side timer proves the request
// can no longer commit, so an empty read never clears the obligation.
func TestHandoffPollerGeneration_PatchCommitsAfterRecoveryRead(t *testing.T) {
	g, c := newGrantHandoff()
	c.lateApply = true
	_, err := g.run(t) // the request times out; the first reread sees nothing
	require.Error(t, err)

	run := func() *pollerGenerationRun {
		return &pollerGenerationRun{ctx: context.Background(), client: g.client, owner: handoffOwner, repo: handoffRepo}
	}
	// Recovery's read is empty (the late entry is committed only after it).
	pending := *g.state(t).Pending
	g.c.ProtectedBranchRules[ruleKey()].MergeAccessLevels = originalEntries()
	p, note := run().releasePendingBranchGrant(pending)
	require.NotEmpty(t, note, "an empty read must not clear the obligation")
	require.NotNil(t, p.BranchGrant)
	assert.True(t, p.BranchGrant.unresolved())

	// The delayed request now commits; the obligation is still recorded, so the
	// entry is found, reported and left alone rather than orphaned.
	rule := g.c.ProtectedBranchRules[ruleKey()]
	rule.MergeAccessLevels = append(rule.MergeAccessLevels, forge.ProtectedBranchAccess{ID: 88, UserID: int(handoffNewUID)})
	p, note = run().releasePendingBranchGrant(p)
	assert.Contains(t, note, "entry ID(s) [88]")
	require.NotNil(t, p.BranchGrant)
	assert.Empty(t, g.c.RevokedProtectedBranchMergeEntries)

	// Even with nothing visible, recovery keeps the attempt recorded.
	g.c.ProtectedBranchRules[ruleKey()].MergeAccessLevels = originalEntries()
	grant := &PollerBranchGrant{Branch: "main", PriorEntryIDs: []int{1, grantAdminEntryID}}
	require.ErrorContains(t, run().revokeBranchGrant(handoffNewUID, grant), "is unresolved")

	// A failing reread is reported without clearing it either.
	g.c.Errors = map[string]error{"GetProtectedBranch": errors.New("500")}
	require.ErrorContains(t, run().revokeBranchGrant(handoffNewUID, grant), "is unresolved")
}

// A grant that fails after an administrator's entry for the account appeared is
// never attributed to fullsend: the entry is neither claimed nor revoked, and
// the unresolved attempt stays recorded.
func TestHandoffPollerGeneration_AdminEntryBesideFailedGrantIsNotClaimed(t *testing.T) {
	g, c := newGrantHandoff()
	c.adminThenFail = true

	res, err := g.run(t)
	require.Error(t, err)
	assert.Nil(t, res.Trigger)
	assert.Empty(t, g.c.RevokedProtectedBranchMergeEntries)
	assert.Contains(t, g.mergeEntries(), forge.ProtectedBranchAccess{ID: 77, UserID: int(handoffNewUID)})
	grant := g.state(t).Pending.BranchGrant
	require.NotNil(t, grant)
	assert.True(t, grant.unresolved())
	assert.Zero(t, grant.EntryID)
}

// An administrator's entry that appears while the forge grant runs is reported
// as not created by fullsend and is never recorded or removed.
func TestHandoffPollerGeneration_AdminEntryInGrantWindowIsNotOwned(t *testing.T) {
	g, c := newGrantHandoff()
	c.adminRace = true
	g.probeErr = errors.New("403 forbidden")

	res, err := g.run(t)
	require.NoError(t, err)
	assert.Nil(t, res.Trigger)
	assert.Empty(t, g.c.RevokedProtectedBranchMergeEntries)
	assert.Len(t, g.mergeEntries(), 3, "the administrator's entry stays")
	assert.Nil(t, g.state(t).Pending.BranchGrant)
}

// A resumed account whose credential inventory is unsafe is not given a merge
// grant.
func TestHandoffPollerGeneration_UnsafeCredentialsGetNoGrant(t *testing.T) {
	g, _ := newGrantHandoff()
	g.c.ProjectMemberAccess[handoffNewUID] = forge.GitLabAccessLevelDeveloper
	g.inventoryErrs = []error{errors.New("active personal access token")}
	g.seed(t, PollerGenerationState{CurrentUserID: handoffCurrentUID, Pending: &PendingPollerGeneration{UserID: handoffNewUID, Phase: PollerPhaseAccountRecorded}})

	res, err := g.run(t)
	require.NoError(t, err)
	assert.Nil(t, res.Trigger)
	assert.Empty(t, g.c.GrantedProtectedBranchMergeUsers)
	assert.Equal(t, originalEntries(), g.mergeEntries())
	assert.Nil(t, g.state(t).Pending.BranchGrant)
}

// A cutover is refused while the grant attempt is unresolved.
func TestBeginPollerCutover_RefusesUnresolvedBranchGrant(t *testing.T) {
	st := PollerGenerationState{CurrentUserID: handoffCurrentUID, Pending: &PendingPollerGeneration{UserID: handoffNewUID, Phase: PollerPhaseVerified, TriggerAttempted: true, TriggerID: 3, BranchGrant: &PollerBranchGrant{Branch: "main", PriorEntryIDs: []int{1}}}}
	_, err := beginPollerCutover(st, handoffNewUID)
	require.ErrorContains(t, err, "unresolved")
}

// A recorded grant that no longer admits the account is removed before its
// replacement is added and recorded.
func TestHandoffPollerGeneration_StaleRecordedGrantIsReplaced(t *testing.T) {
	g, _ := newGrantHandoff()
	g.c.ProjectMemberAccess[handoffNewUID] = forge.GitLabAccessLevelDeveloper
	g.seed(t, PollerGenerationState{CurrentUserID: handoffCurrentUID, Pending: &PendingPollerGeneration{UserID: handoffNewUID, Phase: PollerPhaseAccountRecorded, BranchGrant: &PollerBranchGrant{Branch: "old", EntryID: 777}}})

	res, err := g.run(t)
	require.NoError(t, err)
	require.NotNil(t, res.Trigger, res.DeferReason)
	require.Len(t, g.c.RevokedProtectedBranchMergeEntries, 1)
	assert.Equal(t, "old", g.c.RevokedProtectedBranchMergeEntries[0].Branch)
	assert.Equal(t, 777, g.c.RevokedProtectedBranchMergeEntries[0].EntryID)
	grant := g.state(t).Pending.BranchGrant
	require.NotNil(t, grant)
	assert.Equal(t, "main", grant.Branch)
	assert.NotEqual(t, int64(777), grant.EntryID)

	t.Run("stale revoke failure", func(t *testing.T) {
		g, _ := newGrantHandoff()
		g.c.ProjectMemberAccess[handoffNewUID] = forge.GitLabAccessLevelDeveloper
		g.seed(t, PollerGenerationState{CurrentUserID: handoffCurrentUID, Pending: &PendingPollerGeneration{UserID: handoffNewUID, Phase: PollerPhaseAccountRecorded, BranchGrant: &PollerBranchGrant{Branch: "old", EntryID: 777}}})
		g.c.Errors = map[string]error{"RevokeProtectedBranchMergeUserEntry": errors.New("500")}
		_, err := g.run(t)
		require.Error(t, err)
		assert.Empty(t, g.c.GrantedProtectedBranchMergeUsers)
		assert.Equal(t, int64(777), g.state(t).Pending.BranchGrant.EntryID)
	})
}

// Misbehaving grant and reread responses fail closed or leave nothing
// recorded as installer-owned.
func TestHandoffPollerGeneration_GrantResponseEdgeCases(t *testing.T) {
	t.Run("no effect", func(t *testing.T) {
		g, c := newGrantHandoff()
		c.grantNoop = true
		_, err := g.run(t)
		require.ErrorContains(t, err, "did not take effect")
	})
	t.Run("entry without ID", func(t *testing.T) {
		g, c := newGrantHandoff()
		c.grantNoID = true
		_, err := g.run(t)
		require.ErrorContains(t, err, "could not be identified")
		assert.True(t, g.state(t).Pending.BranchGrant.unresolved(), "the attempt stays recorded")
	})
	t.Run("initial read fails", func(t *testing.T) {
		g, c := newGrantHandoff()
		c.failGetFrom = 2
		_, err := g.run(t)
		require.Error(t, err)
		assert.Empty(t, g.c.GrantedProtectedBranchMergeUsers)
	})
	t.Run("record cleared before regrant fails", func(t *testing.T) {
		g, _ := newGrantHandoff()
		g.c.ProjectMemberAccess[handoffNewUID] = forge.GitLabAccessLevelDeveloper
		g.seed(t, PollerGenerationState{CurrentUserID: handoffCurrentUID, Pending: &PendingPollerGeneration{UserID: handoffNewUID, Phase: PollerPhaseAccountRecorded, BranchGrant: &PollerBranchGrant{Branch: "old", EntryID: 777}}})
		g.c.failWrite = 1
		_, err := g.run(t)
		require.Error(t, err)
		assert.Empty(t, g.c.GrantedProtectedBranchMergeUsers)
	})
}

// Abandoning a replacement whose trigger may have been distributed removes the
// merge entry fullsend granted it before its record is dropped; a failed
// removal keeps the generation for the next run.
func TestResetUnwiredVerifiedPollerGeneration_AbandonRevokesGrant(t *testing.T) {
	seed := func(t *testing.T) grantHandoff {
		g, _ := newGrantHandoff()
		g.c.ProjectMemberAccess[handoffNewUID] = forge.GitLabAccessLevelDeveloper
		rule := g.c.ProtectedBranchRules[ruleKey()]
		rule.MergeAccessLevels = append(rule.MergeAccessLevels, forge.ProtectedBranchAccess{ID: 50, UserID: int(handoffNewUID)})
		g.seed(t, PollerGenerationState{CurrentUserID: handoffCurrentUID, Pending: &PendingPollerGeneration{
			UserID: handoffNewUID, Phase: PollerPhaseVerified, TriggerAttempted: true, TriggerID: 9, TriggerDistributed: true,
			BranchGrant: &PollerBranchGrant{Branch: "main", EntryID: 50},
		}})
		return g
	}

	g := seed(t)
	next, details, ok, err := resetUnwiredVerifiedPollerGeneration(context.Background(), g.client, g, handoffOwner, handoffRepo, g.state(t), handoffCurrentUID, 0)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Nil(t, next.Pending)
	assert.Equal(t, []int64{handoffNewUID}, next.AbandonedUserIDs)
	assert.Equal(t, originalEntries(), g.mergeEntries())
	assert.NotEmpty(t, details)

	g = seed(t)
	g.c.Errors = map[string]error{"RevokeProtectedBranchMergeUserEntry": errors.New("500")}
	_, _, ok, err = resetUnwiredVerifiedPollerGeneration(context.Background(), g.client, g, handoffOwner, handoffRepo, g.state(t), handoffCurrentUID, 0)
	require.Error(t, err)
	assert.False(t, ok)
	st := g.state(t)
	require.NotNil(t, st.Pending)
	assert.Equal(t, int64(50), st.Pending.BranchGrant.EntryID)
	assert.Empty(t, st.AbandonedUserIDs)
}

// Grant ownership moves with each account through cutover and retirement.
func TestPollerCutoverTransitions_BranchGrants(t *testing.T) {
	oldGrant := &PollerBranchGrant{Branch: "main", EntryID: 11}
	newGrant := &PollerBranchGrant{Branch: "main", EntryID: 22}
	st := PollerGenerationState{CurrentUserID: handoffCurrentUID, CurrentBranchGrant: oldGrant, Pending: &PendingPollerGeneration{UserID: handoffNewUID, Phase: PollerPhaseVerified, TriggerAttempted: true, TriggerID: 3, BranchGrant: newGrant}}

	next, err := beginPollerCutover(st, handoffNewUID)
	require.NoError(t, err)
	assert.Equal(t, PollerGenerationState{CurrentUserID: handoffNewUID, CurrentBranchGrant: newGrant, RetiringUserID: handoffCurrentUID, RetiringBranchGrant: oldGrant}, next)

	done, err := finishPollerRetirement(next, handoffCurrentUID)
	require.NoError(t, err)
	assert.Equal(t, PollerGenerationState{CurrentUserID: handoffNewUID, CurrentBranchGrant: newGrant}, done)
}

func TestLoadPollerGenerations_BranchGrantValidation(t *testing.T) {
	for name, raw := range map[string]string{
		"current grant without current": `{"version":1,"current_branch_grant":{"branch":"main","entry_id":3}}`,
		"retiring grant without user":   `{"version":1,"current_user_id":6,"retiring_branch_grant":{"branch":"main","entry_id":3}}`,
		"pending grant without user":    `{"version":1,"pending":{"phase":"account_requested","branch_grant":{"branch":"main","entry_id":3}}}`,
		"grant without branch":          `{"version":1,"current_user_id":6,"current_branch_grant":{"entry_id":3}}`,
		"grant without entry":           `{"version":1,"current_user_id":6,"current_branch_grant":{"branch":"main"}}`,
		"grant unknown field":           `{"version":1,"current_user_id":6,"current_branch_grant":{"branch":"main","entry_id":3,"user_id":6}}`,
		"unresolved current grant":      `{"version":1,"current_user_id":6,"current_branch_grant":{"branch":"main","prior_entry_ids":[1]}}`,
		"negative entry":                `{"version":1,"pending":{"user_id":7,"phase":"account_recorded","branch_grant":{"branch":"main","entry_id":-1}}}`,
		"prior IDs on identified entry": `{"version":1,"pending":{"user_id":7,"phase":"account_recorded","branch_grant":{"branch":"main","entry_id":4,"prior_entry_ids":[1]}}}`,
		"removed attempt time field":    `{"version":1,"pending":{"user_id":7,"phase":"account_recorded","branch_grant":{"branch":"main","attempted_at":100}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodePollerGenerations(raw)
			require.Error(t, err)
		})
	}
	st, err := decodePollerGenerations(`{"version":1,"current_user_id":6,"current_branch_grant":{"branch":"main","entry_id":3},"retiring_user_id":5,"retiring_branch_grant":{"branch":"main","entry_id":2},"pending":{"user_id":7,"phase":"account_recorded","branch_grant":{"branch":"main","entry_id":4}}}`)
	require.NoError(t, err)
	assert.Equal(t, &PollerBranchGrant{Branch: "main", EntryID: 3}, st.CurrentBranchGrant)
	assert.Equal(t, &PollerBranchGrant{Branch: "main", EntryID: 2}, st.RetiringBranchGrant)
	assert.Equal(t, &PollerBranchGrant{Branch: "main", EntryID: 4}, st.Pending.BranchGrant)

	st, err = decodePollerGenerations(`{"version":1,"pending":{"user_id":7,"phase":"account_recorded","branch_grant":{"branch":"main","prior_entry_ids":[1,2]}}}`)
	require.NoError(t, err)
	assert.Equal(t, &PollerBranchGrant{Branch: "main", PriorEntryIDs: []int{1, 2}}, st.Pending.BranchGrant, "an unresolved attempt is valid on a pending generation")
}
