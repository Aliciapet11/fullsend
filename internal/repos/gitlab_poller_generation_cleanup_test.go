package repos

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/forge"
	"github.com/fullsend-ai/fullsend/internal/gitlabroles"
)

// generationCleanupFixture records a managed Poller in rotation state plus a
// retiring, a pending, and an abandoned generation account in the generation
// document, all live project service accounts.
func generationCleanupFixture(t *testing.T) (*forge.FakeClient, *fakeSAAPI) {
	t.Helper()
	ctx := context.Background()
	fc := forge.NewFakeClient()
	const current, retiring, pending, abandoned = 20, 10, 30, 40
	require.NoError(t, writeRotationState(ctx, fc, "g", "p", rotationStateFile{Roles: map[string]rotationRoleState{
		string(gitlabroles.RolePoller): {ManagedUserID: current, Phase: rotationPhaseIdle},
	}}))
	require.NoError(t, writePollerGenerations(ctx, fc, "g", "p", PollerGenerationState{
		CurrentUserID:    current,
		RetiringUserID:   retiring,
		AbandonedUserIDs: []int64{abandoned},
		Pending:          &PendingPollerGeneration{UserID: pending, Phase: PollerPhaseAccountRecorded},
	}))
	sa := newFakeSAAPI()
	for _, id := range []int{current, retiring, pending, abandoned} {
		sa.accounts = append(sa.accounts, GitLabServiceAccount{ID: id, Name: gitlabroles.PollerTokenName})
	}
	return fc, sa
}

// Uninstall verifies and deletes every owned Poller generation account, not
// only the one rotation state records.
func TestDeleteManagedServiceAccounts_DeletesRetiringPendingAndAbandonedGenerations(t *testing.T) {
	ctx := context.Background()
	fc, sa := generationCleanupFixture(t)
	var verified []int
	c := ServiceAccountTokenClient{
		SA: sa,
		VerifyAccountDeletion: func(_ context.Context, _, _ string, id int) error {
			verified = append(verified, id)
			return nil
		},
	}

	n, err := c.DeleteManagedServiceAccounts(ctx, fc, "g", "p")
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.ElementsMatch(t, []int{10, 20, 30, 40}, fc.DeletedProjectServiceAccounts)
	assert.ElementsMatch(t, []int{10, 20, 30, 40}, verified, "resources of each owned account are verified before deletion")
}

// Supplied-account exclusions apply to generation accounts like any other.
func TestDeleteManagedServiceAccounts_GenerationAccountsHonorExclusions(t *testing.T) {
	ctx := context.Background()
	fc, sa := generationCleanupFixture(t)
	c := ServiceAccountTokenClient{
		SA:                    sa,
		VerifyAccountDeletion: func(context.Context, string, string, int) error { return nil },
		SuppliedAccountIDs:    func(context.Context, string, string) ([]int, error) { return []int{10, 30}, nil },
	}

	n, err := c.DeleteManagedServiceAccounts(ctx, fc, "g", "p")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.ElementsMatch(t, []int{20, 40}, fc.DeletedProjectServiceAccounts)
}

// A generation account whose resources cannot be verified is not deleted and
// the failure is returned, so uninstall retains ownership state.
func TestDeleteManagedServiceAccounts_GenerationAccountVerificationFailureRetainsOwnership(t *testing.T) {
	ctx := context.Background()
	fc, sa := generationCleanupFixture(t)
	c := ServiceAccountTokenClient{
		SA: sa,
		VerifyAccountDeletion: func(_ context.Context, _, _ string, id int) error {
			if id == 10 || id == 40 {
				return errors.New("500 verifying resources")
			}
			return nil
		},
	}

	n, err := c.DeleteManagedServiceAccounts(ctx, fc, "g", "p")
	require.Error(t, err)
	assert.Equal(t, 2, n)
	assert.ElementsMatch(t, []int{20, 30}, fc.DeletedProjectServiceAccounts)
	assert.NotContains(t, fc.DeletedProjectServiceAccounts, 10)
	assert.NotContains(t, fc.DeletedProjectServiceAccounts, 40)
}

// An unreadable generation document fails closed rather than reading as no
// generation accounts.
func TestDeleteManagedServiceAccounts_UnreadableGenerationDocumentFailsClosed(t *testing.T) {
	ctx := context.Background()
	fc, sa := generationCleanupFixture(t)
	fc.VariableValues["g/p/"+forge.VarGitLabPollerGenerations] = "{not json"
	c := ServiceAccountTokenClient{SA: sa, VerifyAccountDeletion: func(context.Context, string, string, int) error { return nil }}

	n, err := c.DeleteManagedServiceAccounts(ctx, fc, "g", "p")
	require.Error(t, err)
	assert.Zero(t, n)
	assert.Empty(t, fc.DeletedProjectServiceAccounts)
}
