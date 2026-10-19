package install

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/forge"
	gh "github.com/fullsend-ai/fullsend/internal/forge/github"
)

func singleSlotLayout(slots int) PoolLayout {
	return PoolLayout{FirstOrgIndex: 0, OrgCount: 1, SlotsPerOrg: slots}
}

func TestPoolLayoutForEnvironment_HonoursPoolSize(t *testing.T) {
	t.Setenv("BEHAVIOUR_POOL_SIZE", "5")
	dev := PoolLayoutForEnvironment("dev", nil)
	assert.Equal(t, 5, dev.SlotsPerOrg)
	assert.Len(t, dev.Slots(), DevPoolOrgCount*5)
	stage := PoolLayoutForEnvironment("stage", nil)
	assert.Equal(t, 5, stage.SlotsPerOrg)
	assert.Len(t, stage.Slots(), 5)

	// An unparsable override falls back to the default and warns.
	t.Setenv("BEHAVIOUR_POOL_SIZE", "many")
	var warned []string
	l := PoolLayoutForEnvironment("stage", func(format string, args ...any) {
		warned = append(warned, fmt.Sprintf(format, args...))
	})
	assert.Equal(t, DefaultPoolSize, l.SlotsPerOrg)
	require.Len(t, warned, 1)
	assert.Contains(t, warned[0], "BEHAVIOUR_POOL_SIZE")
}

func TestSlotAllocator_AdoptsOwnRepoAfterLostCreateResponse(t *testing.T) {
	layout := singleSlotLayout(2)
	p := newFakePool(layout)
	a := newTestAllocator(t, layout, p)
	a.shuffle = func([]Slot) {}

	// The first POST created the repo but its response was lost; the
	// client's retry then reported "already exists".
	p.clients["halfsend"].createLostErr = fmt.Errorf("create repo: %w", forge.ErrAlreadyExists)
	s, _, err := a.Claim(context.Background())
	require.NoError(t, err)
	assert.Equal(t, Slot{Org: "halfsend", Repo: "test-repo-01"}, s)
	assert.Equal(t, []Slot{s}, a.Held())

	repo, err := p.fc.GetRepo(context.Background(), s.Org, s.Repo)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(repo.Description, SlotRepoDescription+leaseDescriptionSep))

	// The adopted lease releases normally.
	require.NoError(t, a.Release(context.Background(), s))
	assert.False(t, p.exists(t, s))
}

func TestSlotAllocator_ForeignRepoOnAlreadyExistsIsNotAdopted(t *testing.T) {
	layout := singleSlotLayout(1)
	p := newFakePool(layout)
	// Not visible to the listing hint, so the create is what finds it.
	p.fc.CreatedRepos = append(p.fc.CreatedRepos, forge.Repository{
		ID: 7, Name: "test-repo-01", FullName: "halfsend/test-repo-01", Description: SlotRepoDescription + leaseDescriptionSep + "someone-else",
	})
	a := newTestAllocator(t, layout, p)

	_, _, err := a.Claim(context.Background())
	var capErr *CapacityError
	require.ErrorAs(t, err, &capErr)
	assert.Equal(t, 1, capErr.Occupied)
	assert.Empty(t, a.Held())
	assert.Empty(t, p.fc.DeletedRepos)
}

func TestSlotAllocator_ReleaseNeverDeletesRecreatedRepo(t *testing.T) {
	layout := singleSlotLayout(1)
	p := newFakePool(layout)
	a := newTestAllocator(t, layout, p)
	ctx := context.Background()

	s, _, err := a.Claim(ctx)
	require.NoError(t, err)

	// A's earlier delete landed with a lost response; B recreated the name.
	require.NoError(t, p.fc.DeleteRepo(ctx, s.Org, s.Repo))
	_, err = p.fc.CreateRepo(ctx, s.Org, s.Repo, newLeaseDescription(), false)
	require.NoError(t, err)

	require.NoError(t, a.Release(ctx, s))
	assert.Equal(t, []string{s.String()}, p.fc.DeletedRepos, "only the simulated first delete; B's repo is untouched")
	assert.True(t, p.exists(t, s))
}

func TestSlotAllocator_ReleaseAfterLostDeleteResponse(t *testing.T) {
	layout := singleSlotLayout(1)
	p := newFakePool(layout)
	a := newTestAllocator(t, layout, p)
	ctx := context.Background()

	s, _, err := a.Claim(ctx)
	require.NoError(t, err)

	p.clients["halfsend"].deleteLostErr = errors.New("response timeout")
	require.NoError(t, a.Release(ctx, s), "a re-check shows the repo is gone, so the delete is not re-sent or failed")
	assert.Equal(t, []string{s.String()}, p.fc.DeletedRepos)
	assert.False(t, p.exists(t, s))
}

func TestSlotAllocator_ReleaseVerifyFailureDeletesNothing(t *testing.T) {
	layout := singleSlotLayout(1)
	p := newFakePool(layout)
	a := newTestAllocator(t, layout, p)
	ctx := context.Background()

	s, _, err := a.Claim(ctx)
	require.NoError(t, err)

	p.fc.Errors["GetRepo"] = errors.New("api down")
	err = a.Release(ctx, s)
	require.ErrorContains(t, err, "api down")
	assert.Empty(t, p.fc.DeletedRepos)
}

// expiredTokenClient models a claim-time client whose installation token
// has since expired: reads and deletes are refused.
type expiredTokenClient struct{ *orgClient }

func (expiredTokenClient) GetRepo(context.Context, string, string) (*forge.Repository, error) {
	return nil, &gh.APIError{StatusCode: http.StatusUnauthorized, Message: "Bad credentials"}
}

func (expiredTokenClient) DeleteRepo(context.Context, string, string) error {
	return &gh.APIError{StatusCode: http.StatusUnauthorized, Message: "Bad credentials"}
}

func TestSlotAllocator_ReleaseReResolvesClient(t *testing.T) {
	layout := singleSlotLayout(1)
	p := newFakePool(layout)
	calls := 0
	a, err := newSlotAllocator(layout, func(_ context.Context, org string) (forge.Client, error) {
		calls++
		if calls == 1 {
			return expiredTokenClient{p.clients[org]}, nil
		}
		return p.clients[org], nil
	}, t.Logf)
	require.NoError(t, err)

	s, _, err := a.Claim(context.Background())
	require.NoError(t, err)
	require.NoError(t, a.Release(context.Background(), s), "release uses the freshly resolved client")
	assert.Equal(t, 2, calls)
	assert.False(t, p.exists(t, s))
}

func TestSlotAllocator_ReleaseFallsBackToClaimClient(t *testing.T) {
	layout := singleSlotLayout(1)
	p := newFakePool(layout)
	calls := 0
	a, err := newSlotAllocator(layout, func(_ context.Context, org string) (forge.Client, error) {
		calls++
		if calls > 1 {
			return nil, errors.New("mint unavailable")
		}
		return p.clients[org], nil
	}, t.Logf)
	require.NoError(t, err)

	s, _, err := a.Claim(context.Background())
	require.NoError(t, err)
	require.NoError(t, a.Release(context.Background(), s))
	assert.False(t, p.exists(t, s))
}

func TestSlotAllocator_ListingAvoidsCreateProbes(t *testing.T) {
	layout := singleSlotLayout(4)
	p := newFakePool(layout)
	for _, s := range layout.Slots()[:3] {
		p.fc.Repos = append(p.fc.Repos, forge.Repository{Name: s.Repo, FullName: s.String()})
	}
	a := newTestAllocator(t, layout, p)
	a.shuffle = func([]Slot) {}

	s, _, err := a.Claim(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "test-repo-04", s.Repo)
	assert.Equal(t, 1, p.clients["halfsend"].createCalls(), "occupied slots are skipped from the listing, not probed")

	_, _, err = a.Claim(context.Background())
	var capErr *CapacityError
	require.ErrorAs(t, err, &capErr)
	assert.Equal(t, 4, capErr.Occupied)
	assert.Equal(t, 1, p.clients["halfsend"].createCalls(), "a full pass makes no create calls")
}

func TestSlotAllocator_ListingFailureFallsBackToProbing(t *testing.T) {
	layout := singleSlotLayout(2)
	p := newFakePool(layout)
	p.fc.Repos = append(p.fc.Repos, forge.Repository{Name: "test-repo-01", FullName: "halfsend/test-repo-01"})
	p.fc.Errors["ListOrgRepos"] = errors.New("listing unavailable")
	a := newTestAllocator(t, layout, p)
	a.shuffle = func([]Slot) {}

	s, _, err := a.Claim(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "test-repo-02", s.Repo)
	assert.Equal(t, 2, p.clients["halfsend"].createCalls(), "without a listing every slot is probed by create")
}

func TestSlotAllocator_ListingRateLimitClosesOrg(t *testing.T) {
	layout := PoolLayout{FirstOrgIndex: 1, OrgCount: 2, SlotsPerOrg: 2}
	p := newFakePool(layout)
	p.fc.Errors["ListOrgRepos"] = &gh.APIError{StatusCode: http.StatusTooManyRequests, Message: "rate limit: too many"}
	a := newTestAllocator(t, layout, p)

	_, _, err := a.Claim(context.Background())
	var capErr *CapacityError
	require.ErrorAs(t, err, &capErr)
	assert.Contains(t, capErr.RateLimited, "halfsend-01")
	assert.Contains(t, capErr.RateLimited, "halfsend-02")
	assert.Zero(t, p.clients["halfsend-01"].createCalls())
	assert.Zero(t, p.clients["halfsend-02"].createCalls())
}

func shortCreateTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := claimCreateTimeout
	claimCreateTimeout = d
	t.Cleanup(func() { claimCreateTimeout = old })
}

func TestSlotAllocator_CreateTimeoutBacksOffOrg(t *testing.T) {
	shortCreateTimeout(t, 20*time.Millisecond)
	layout := PoolLayout{FirstOrgIndex: 1, OrgCount: 2, SlotsPerOrg: 1}
	p := newFakePool(layout)
	a := newTestAllocator(t, layout, p)
	a.shuffle = func([]Slot) {}
	p.clients["halfsend-01"].createBlocks = true

	s, _, err := a.Claim(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "halfsend-02", s.Org, "a create stuck in backoff moves the claim to another org")
	assert.Equal(t, 1, p.clients["halfsend-01"].createCalls())

	// The stuck org stays closed for the backoff window.
	_, _, err = a.Claim(context.Background())
	var capErr *CapacityError
	require.ErrorAs(t, err, &capErr)
	assert.Contains(t, capErr.RateLimited, "halfsend-01")
	assert.Equal(t, 1, p.clients["halfsend-01"].createCalls())
}

func TestSlotAllocator_CreateTimeoutAdoptsLandedRepo(t *testing.T) {
	shortCreateTimeout(t, 20*time.Millisecond)
	layout := singleSlotLayout(1)
	p := newFakePool(layout)
	a := newTestAllocator(t, layout, p)
	p.clients["halfsend"].createLandsThenBlocks = true

	s, _, err := a.Claim(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []Slot{s}, a.Held())
	require.NoError(t, a.Release(context.Background(), s))
	assert.False(t, p.exists(t, s))
}

// TestSlotAllocator_LiveClientRateLimitDoesNotPinClaim drives the real
// LiveClient, whose retry loop would otherwise sleep out the server's
// Retry-After inside CreateRepo.
func TestSlotAllocator_LiveClientRateLimitDoesNotPinClaim(t *testing.T) {
	shortCreateTimeout(t, 100*time.Millisecond)
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/orgs/halfsend/repos":
			_, _ = w.Write([]byte("[]"))
		case r.Method == http.MethodPost && r.URL.Path == "/orgs/halfsend/repos":
			posts.Add(1)
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	layout := singleSlotLayout(3)
	client := gh.New("test-token").WithBaseURL(srv.URL)
	a, err := newSlotAllocator(layout, func(context.Context, string) (forge.Client, error) { return client, nil }, t.Logf)
	require.NoError(t, err)

	start := time.Now()
	_, _, err = a.Claim(context.Background())
	elapsed := time.Since(start)

	var capErr *CapacityError
	require.ErrorAs(t, err, &capErr)
	assert.Contains(t, capErr.RateLimited, "halfsend")
	assert.Less(t, elapsed, 10*time.Second, "the claim must not wait out the client's retry backoff")
	assert.Equal(t, int32(1), posts.Load(), "the limited org is not hit again after the first create")
}
