// slotpool.go implements repo-level slot leasing across pool orgs
// (#8578). The pool is one set of (org, repo-name) slots whose names
// are derived from numeric indices rather than kept in a list. Creating
// a slot's repository claims it; an existing repository means another
// allocator (in this process or another suite) holds it, so it is
// skipped and never reset or deleted. Each org installation's GitHub
// rate-limit headers gate further claims there until the reported reset.
//
// This file provides the allocation layer only. Wiring it into the
// behaviour and playback suite runners (replacing exclusive org
// acquisition) and in-driver GC of crashed leases are follow-ups.
//
// The existing ensurer (ensure.go: ensureRepoExists → deleteBlockingRepo
// and resetRepo) deletes or resets existing test-repo-NN repos under the
// same names. It must not run against orgs shared with this allocator: it
// would delete actively leased repos and break the "occupied is never
// deleted" rule. Removing that reset/delete path must happen in the same
// change that wires the allocator in.
package install

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fullsend-ai/fullsend/internal/forge"
	gh "github.com/fullsend-ai/fullsend/internal/forge/github"
)

const (
	// poolOrgPrefix is the base name of every pool org. Index 0 is the
	// unsuffixed org (STAGE); indices >= 1 are halfsend-NN (DEV).
	poolOrgPrefix = StageOrg

	// DevPoolOrgCount is the number of numbered DEV pool orgs
	// (halfsend-01 … halfsend-12).
	DevPoolOrgCount = 12

	// SlotRepoDescription is the description prefix every claimed slot
	// repo is created with. It is the test-ownership marker: only repos
	// carrying it may ever be reclaimed by GC. Each claim appends a
	// unique " lease=<nonce>" suffix so the claimer can recognise its own
	// repo (see Claim and Release).
	SlotRepoDescription = "fullsend behaviour test slot"

	// leaseDescriptionSep separates SlotRepoDescription from the
	// per-claim nonce.
	leaseDescriptionSep = " lease="

	// rateLimitReserve is the remaining primary-quota count at or below
	// which an org stops receiving new claims until its reset. A claim
	// is followed by the whole install flow and the scenario itself, so
	// claiming on an org with only a handful of requests left would just
	// move the exhaustion into the scenario.
	rateLimitReserve = 100

	// defaultRateLimitWindow is how long an org is avoided after a
	// rate-limit error when no reset time is known. GitHub documents
	// waiting at least one minute before retrying a secondary limit that
	// carries no retry-after.
	defaultRateLimitWindow = time.Minute
)

// slotClaimPoll is how long Acquire waits between claim passes while the
// pool has no eligible capacity. Overridden in tests.
var slotClaimPoll = 15 * time.Second

// claimCreateTimeout bounds each API request a claim makes: the create,
// the org listing, and the ownership lookup. The live client retries 429
// and rate-limit 403 responses with backoff for minutes before surfacing a
// rate-limit error, which would keep a claim pinned to a limited org; a
// request that has not finished by then marks the org as backed off
// instead, for the Retry-After wait the client was honouring when known.
// Overridden in tests.
var claimCreateTimeout = 30 * time.Second

// ErrPoolCapacityExhausted reports that no slot could be claimed in a
// claim pass: every eligible slot was occupied, or its org was
// rate-limited, denied, or failing. Use errors.Is; the concrete error is
// a *CapacityError carrying the per-reason breakdown.
var ErrPoolCapacityExhausted = errors.New("behaviour pool capacity exhausted")

// PoolLayout describes the pool's orgs and slots by numeric index.
// Names are derived by PoolOrgName and PoolSlotName.
type PoolLayout struct {
	// FirstOrgIndex is the index of the first org. 0 selects the
	// unsuffixed STAGE org; DEV starts at 1.
	FirstOrgIndex int
	// OrgCount is the number of consecutive org indices in the pool.
	OrgCount int
	// SlotsPerOrg is the number of test-repo-NN slots in each org.
	SlotsPerOrg int
}

// DevPoolLayout is the DEV pool: halfsend-01 … halfsend-12, each with
// test-repo-01 … test-repo-<slotsPerOrg> (DefaultPoolSize by default).
func DevPoolLayout(slotsPerOrg int) PoolLayout {
	return PoolLayout{FirstOrgIndex: 1, OrgCount: DevPoolOrgCount, SlotsPerOrg: slotsPerOrg}
}

// StagePoolLayout is the STAGE pool: the single unsuffixed halfsend org
// with test-repo-01 … test-repo-<slotsPerOrg>.
func StagePoolLayout(slotsPerOrg int) PoolLayout {
	return PoolLayout{FirstOrgIndex: 0, OrgCount: 1, SlotsPerOrg: slotsPerOrg}
}

// PoolLayoutForEnvironment returns StagePoolLayout for "stage" and
// DevPoolLayout otherwise, matching the install factory selection. The
// slot count honours BEHAVIOUR_POOL_SIZE through envPoolSize, the same
// source as the mint and WIF repo lists, so the allocator never claims a
// test-repo-NN name that has no mint/WIF configuration. logf receives
// the warning for an unparsable override and may be nil.
func PoolLayoutForEnvironment(environment string, logf func(string, ...any)) PoolLayout {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	slotsPerOrg := envPoolSize(logf)
	if environment == "stage" {
		return StagePoolLayout(slotsPerOrg)
	}
	return DevPoolLayout(slotsPerOrg)
}

// PoolOrgName derives an org name from its index: 0 is "halfsend",
// n >= 1 is "halfsend-NN".
func PoolOrgName(index int) string {
	if index == 0 {
		return poolOrgPrefix
	}
	return fmt.Sprintf("%s-%02d", poolOrgPrefix, index)
}

// PoolSlotName derives a slot repo name from its 1-based index.
func PoolSlotName(index int) string {
	return fmt.Sprintf("test-repo-%02d", index)
}

// Orgs returns the layout's org names in index order.
func (l PoolLayout) Orgs() []string {
	orgs := make([]string, 0, l.OrgCount)
	for i := range l.OrgCount {
		orgs = append(orgs, PoolOrgName(l.FirstOrgIndex+i))
	}
	return orgs
}

// Slots returns every (org, repo) slot of the layout in index order.
func (l PoolLayout) Slots() []Slot {
	slots := make([]Slot, 0, l.OrgCount*l.SlotsPerOrg)
	for _, org := range l.Orgs() {
		for j := 1; j <= l.SlotsPerOrg; j++ {
			slots = append(slots, Slot{Org: org, Repo: PoolSlotName(j)})
		}
	}
	return slots
}

func (l PoolLayout) validate() error {
	if l.FirstOrgIndex < 0 {
		return fmt.Errorf("pool layout: first org index must not be negative, got %d", l.FirstOrgIndex)
	}
	if l.OrgCount <= 0 {
		return fmt.Errorf("pool layout: org count must be positive, got %d", l.OrgCount)
	}
	if l.SlotsPerOrg <= 0 {
		return fmt.Errorf("pool layout: slots per org must be positive, got %d", l.SlotsPerOrg)
	}
	return nil
}

// Slot is one (org, repo-name) pool position.
type Slot struct {
	Org  string
	Repo string
}

// String renders the slot as org/repo.
func (s Slot) String() string { return s.Org + "/" + s.Repo }

// OrgClientFunc returns the forge client holding org's credentials
// (its own installation token). Clients that implement
// forge.RateLimitReporter feed the per-org rate-limit gate. It is called
// again by Release, because installation tokens expire after an hour and
// a lease can outlive the claim-time token; it must return a client whose
// credentials are valid at call time (refreshing or re-minting as needed).
type OrgClientFunc func(ctx context.Context, org string) (forge.Client, error)

// CapacityError is returned by a claim pass that leased nothing. It
// separates occupied slots from orgs skipped for quota, permission, or
// transient failures so the suite log says why capacity ran out.
type CapacityError struct {
	// Occupied counts slots whose repository already existed.
	Occupied int
	// RateLimited maps each quota-gated org to when it may be retried.
	RateLimited map[string]time.Time
	// Denied maps orgs whose credentials or create calls were refused
	// (forbidden, no client) to the last such error.
	Denied map[string]error
	// Failed maps orgs that hit transient or unclassified API failures
	// to the last such error.
	Failed map[string]error
}

func (e *CapacityError) Error() string {
	parts := []string{fmt.Sprintf("%d occupied slot(s)", e.Occupied)}
	if len(e.RateLimited) > 0 {
		orgs := sortedKeys(e.RateLimited)
		items := make([]string, 0, len(orgs))
		for _, org := range orgs {
			items = append(items, fmt.Sprintf("%s until %s", org, e.RateLimited[org].UTC().Format(time.RFC3339)))
		}
		parts = append(parts, "rate-limited: "+strings.Join(items, ", "))
	}
	if len(e.Denied) > 0 {
		parts = append(parts, "denied: "+joinOrgErrors(e.Denied))
	}
	if len(e.Failed) > 0 {
		parts = append(parts, "failed: "+joinOrgErrors(e.Failed))
	}
	return fmt.Sprintf("%v: %s", ErrPoolCapacityExhausted, strings.Join(parts, "; "))
}

// Is lets errors.Is match ErrPoolCapacityExhausted.
func (e *CapacityError) Is(target error) bool { return target == ErrPoolCapacityExhausted }

// slotAllocator leases pool slots across orgs. It is safe for
// concurrent use. Exclusivity between allocators (including other
// suites) comes from the forge: CreateRepo succeeds for exactly one
// caller per name, and an already-existing repository is never touched.
type slotAllocator struct {
	layout    PoolLayout
	clientFor OrgClientFunc
	logf      func(string, ...any)
	now       func() time.Time
	shuffle   func([]Slot)

	mu           sync.Mutex
	held         map[Slot]lease
	blockedUntil map[string]time.Time
	quota        map[string]forge.RateLimit
}

// lease is one held slot: the client that created (or adopted) the repo
// and the identity that proves a repo under the slot's name is still ours.
type lease struct {
	client forge.Client
	// repoID is the repository ID reported at creation; 0 when unknown.
	repoID int64
	// description is the per-claim description carrying the nonce.
	description string
}

// owns reports whether repo is the repository this lease created. GitHub
// addresses repos by name, so after an ambiguous delete another
// allocator may already have recreated the name; the ID (or, when the
// ID is unavailable, the unique description nonce) tells them apart.
func (l lease) owns(repo *forge.Repository) bool {
	if repo == nil {
		return false
	}
	if l.repoID != 0 && repo.ID != 0 {
		return repo.ID == l.repoID
	}
	return l.description != "" && repo.Description == l.description
}

// newLeaseDescription returns SlotRepoDescription with a unique nonce.
func newLeaseDescription() string {
	return fmt.Sprintf("%s%s%016x%016x", SlotRepoDescription, leaseDescriptionSep, rand.Uint64(), rand.Uint64())
}

// newSlotAllocator returns an allocator over layout that resolves
// per-org credentials through clientFor.
func newSlotAllocator(layout PoolLayout, clientFor OrgClientFunc, logf func(string, ...any)) (*slotAllocator, error) {
	if err := layout.validate(); err != nil {
		return nil, err
	}
	if clientFor == nil {
		return nil, errors.New("slot allocator: org client func must not be nil")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &slotAllocator{
		layout:    layout,
		clientFor: clientFor,
		logf:      logf,
		now:       time.Now,
		shuffle: func(s []Slot) {
			rand.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
		},
		held:         make(map[Slot]lease),
		blockedUntil: make(map[string]time.Time),
		quota:        make(map[string]forge.RateLimit),
	}, nil
}

// Capacity is the total number of slots in the layout.
func (a *slotAllocator) Capacity() int {
	return a.layout.OrgCount * a.layout.SlotsPerOrg
}

// Claim makes one pass over every slot in randomized order and leases
// the first one whose repository it creates. It returns the slot and
// the org-specific client that created it. When nothing can be leased
// it returns a *CapacityError (errors.Is ErrPoolCapacityExhausted);
// context cancellation is returned as the context error.
func (a *slotAllocator) Claim(ctx context.Context) (Slot, forge.Client, error) {
	slots := a.layout.Slots()
	a.shuffle(slots)

	capErr := &CapacityError{
		RateLimited: map[string]time.Time{},
		Denied:      map[string]error{},
		Failed:      map[string]error{},
	}
	clients := map[string]forge.Client{}
	listed := map[string]map[string]bool{}

	for _, slot := range slots {
		if err := ctx.Err(); err != nil {
			return Slot{}, nil, fmt.Errorf("claiming pool slot: %w", err)
		}
		if _, denied := capErr.Denied[slot.Org]; denied {
			continue
		}
		if until, limited := a.orgBlockedUntil(slot.Org); limited {
			capErr.RateLimited[slot.Org] = until
			continue
		}
		if a.isHeld(slot) {
			capErr.Occupied++
			continue
		}

		client, ok := clients[slot.Org]
		if !ok {
			var err error
			client, err = a.clientFor(ctx, slot.Org)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return Slot{}, nil, fmt.Errorf("claiming pool slot: %w", ctxErr)
				}
				a.logf("[slots] no credentials for %s, skipping org: %v", slot.Org, err)
				capErr.Denied[slot.Org] = err
				continue
			}
			clients[slot.Org] = client
		}

		// Fold in quota the client has used since the last observation
		// (before its first claim, or by scenario operations) so an
		// exhausted org is skipped without a create call that would run
		// through the client's backoff retries.
		a.observeQuota(slot.Org, client)
		if until, limited := a.orgBlockedUntil(slot.Org); limited {
			capErr.RateLimited[slot.Org] = until
			continue
		}

		// Read the org's repositories once per pass and create only names
		// that are absent. Probing every slot by create would send up to
		// one POST per slot per pass from every waiting Acquire, which can
		// trip GitHub's content-creation secondary limit. A failed listing
		// falls back to probing by create.
		occupied, listedOrg := listed[slot.Org]
		if !listedOrg {
			names, err := a.listOccupied(ctx, client, slot.Org)
			a.observeQuota(slot.Org, client)
			switch {
			case err == nil:
			case ctx.Err() != nil:
				return Slot{}, nil, fmt.Errorf("claiming pool slot: %w", ctx.Err())
			case isBackoffError(err) || isDeniedError(err):
				a.recordOrgError(slot, client, err, capErr)
				continue
			default:
				a.logf("[slots] listing %s failed, probing by create: %v", slot.Org, err)
			}
			listed[slot.Org] = names
			occupied = names
			if until, limited := a.orgBlockedUntil(slot.Org); limited {
				capErr.RateLimited[slot.Org] = until
				continue
			}
		}
		if occupied[strings.ToLower(slot.String())] {
			capErr.Occupied++
			continue
		}

		// Each claim carries a unique description so that, if the create
		// succeeds server-side but the response is lost (the client
		// retries and then sees "already exists"), the claimer can tell
		// its own repo from a competitor's and adopt it instead of
		// leaking the slot.
		desc := newLeaseDescription()
		createCtx, cancelCreate := context.WithTimeout(ctx, claimCreateTimeout)
		repo, err := client.CreateRepo(createCtx, slot.Org, slot.Repo, desc, false)
		// A create cut off by our own deadline (not the caller's) means
		// the client was backing off inside its retry loop.
		timedOut := err != nil && ctx.Err() == nil && createCtx.Err() != nil
		cancelCreate()
		a.observeQuota(slot.Org, client)
		if err == nil {
			l := lease{client: client, description: desc}
			if repo != nil {
				l.repoID = repo.ID
			}
			a.recordLease(slot, l)
			return slot, client, nil
		}

		switch {
		case forge.IsAlreadyExists(err):
			l, ours, ownErr := a.findOwn(ctx, client, slot, desc)
			if ours {
				a.logf("[slots] %s already existed after our create; adopting it", slot)
				a.recordLease(slot, l)
				return slot, client, nil
			}
			if ownErr != nil && isBackoffError(ownErr) {
				// The ownership lookup was throttled or timed out: move on
				// to other orgs instead of probing this one again.
				a.recordOrgError(slot, client, ownErr, capErr)
				break
			}
			capErr.Occupied++
		case ctx.Err() != nil:
			// The create may have succeeded server-side; the repo then
			// looks occupied to everyone and is left for GC.
			return Slot{}, nil, fmt.Errorf("claiming pool slot %s: %w", slot, ctx.Err())
		case timedOut:
			// The create may have landed before the deadline cut it off.
			l, ours, ownErr := a.findOwn(ctx, client, slot, desc)
			if ours {
				a.logf("[slots] %s was created despite create timeout; adopting it", slot)
				a.recordLease(slot, l)
				return slot, client, nil
			}
			if ownErr != nil && isBackoffError(ownErr) {
				// Honour a rate-limit deadline the lookup observed too;
				// the longer window wins.
				a.blockOrg(slot.Org, client, ownErr)
			}
			until := a.blockOrg(slot.Org, client, err)
			a.logf("[slots] creating %s did not finish within %v, backing off %s until %s: %v",
				slot, claimCreateTimeout, slot.Org, until.UTC().Format(time.RFC3339), err)
			capErr.RateLimited[slot.Org] = until
		default:
			a.recordOrgError(slot, client, err, capErr)
		}
	}
	return Slot{}, nil, capErr
}

// recordLease stores the lease for slot and logs the claim.
func (a *slotAllocator) recordLease(slot Slot, l lease) {
	a.mu.Lock()
	a.held[slot] = l
	a.mu.Unlock()
	a.logf("[slots] claimed %s (%s)", slot, a.quotaSummary(slot.Org))
}

// recordOrgError files a failed API call for slot's org under the
// matching CapacityError reason.
func (a *slotAllocator) recordOrgError(slot Slot, client forge.Client, err error, capErr *CapacityError) {
	switch {
	case gh.IsRateLimitError(err):
		until := a.blockOrg(slot.Org, client, err)
		a.logf("[slots] %s rate-limited until %s: %v", slot.Org, until.UTC().Format(time.RFC3339), err)
		capErr.RateLimited[slot.Org] = until
	case errors.Is(err, context.DeadlineExceeded):
		// A request bounded by claimCreateTimeout did not finish: the org
		// is slow or throttling with no deadline we could read.
		until := a.blockOrg(slot.Org, client, err)
		a.logf("[slots] %s request did not finish within %v, backing off until %s: %v",
			slot.Org, claimCreateTimeout, until.UTC().Format(time.RFC3339), err)
		capErr.RateLimited[slot.Org] = until
	case isDeniedError(err):
		a.logf("[slots] %s denied, skipping org: %v", slot, err)
		capErr.Denied[slot.Org] = err
	default:
		a.logf("[slots] %s failed: %v", slot, err)
		capErr.Failed[slot.Org] = err
	}
}

// listOccupied returns the lower-cased org/repo full names of the
// repositories currently in org. It is a hint only: archived repos and
// forks are not listed, so a create can still report "already exists".
func (a *slotAllocator) listOccupied(ctx context.Context, client forge.Client, org string) (map[string]bool, error) {
	reqCtx, cancel := context.WithTimeout(ctx, claimCreateTimeout)
	defer cancel()
	repos, err := client.ListOrgRepos(reqCtx, org, true)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(repos))
	for _, r := range repos {
		names[strings.ToLower(r.FullName)] = true
	}
	return names, nil
}

// findOwn looks the slot's repository up by name and reports whether it
// carries desc, in which case the returned lease adopts it. The lookup is
// bounded by claimCreateTimeout. A lookup failure is returned so the caller
// can back the org off when it is throttling; the repo is then treated as
// not ours.
func (a *slotAllocator) findOwn(ctx context.Context, client forge.Client, slot Slot, desc string) (lease, bool, error) {
	reqCtx, cancel := context.WithTimeout(ctx, claimCreateTimeout)
	defer cancel()
	repo, err := client.GetRepo(reqCtx, slot.Org, slot.Repo)
	a.observeQuota(slot.Org, client)
	if err != nil {
		return lease{}, false, err
	}
	if repo == nil || repo.Description != desc {
		return lease{}, false, nil
	}
	return lease{client: client, repoID: repo.ID, description: desc}, true, nil
}

// Acquire repeats Claim until a slot is leased or ctx ends, waiting
// slotClaimPoll between passes that found no capacity. Errors other
// than capacity exhaustion are returned immediately. On cancellation
// the last capacity error is joined to the context error.
func (a *slotAllocator) Acquire(ctx context.Context) (Slot, forge.Client, error) {
	for {
		slot, client, err := a.Claim(ctx)
		if err == nil || !errors.Is(err, ErrPoolCapacityExhausted) {
			return slot, client, err
		}
		a.logf("[slots] no capacity, retrying in %v: %v", slotClaimPoll, err)
		select {
		case <-ctx.Done():
			return Slot{}, nil, fmt.Errorf("claiming pool slot: %w", errors.Join(ctx.Err(), err))
		case <-time.After(slotClaimPoll):
		}
	}
}

// Release deletes the leased slot's repository, then forgets the lease.
// GitHub deletes repositories by name only, so Release first checks that
// the repository now under the name is still the one this lease created
// (by ID, else by the claim's description nonce): after an ambiguous
// failure another allocator may already have recreated the name, and its
// repository is never deleted. The check and the delete are not atomic;
// the window is one API round trip. A deletion failure is returned unless
// a re-check shows the repository is gone or no longer ours; the
// repository then still exists, so other allocators keep treating the
// slot as occupied (it is left for GC) rather than reusing it. The
// client is re-resolved through the OrgClientFunc, since the claim-time
// installation token may have expired; the claim-time client is the
// fallback when that fails. Releasing a slot this allocator does not hold
// is an error and deletes nothing.
func (a *slotAllocator) Release(ctx context.Context, slot Slot) error {
	a.mu.Lock()
	l, ok := a.held[slot]
	if ok {
		delete(a.held, slot)
	}
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("releasing pool slot %s: not held by this allocator (possible double-release)", slot)
	}

	client := l.client
	if fresh, err := a.clientFor(ctx, slot.Org); err != nil {
		a.logf("[slots] re-resolving client for %s failed, using the claim-time client: %v", slot.Org, err)
	} else if fresh != nil {
		client = fresh
	}

	gone, err := a.repoGoneOrForeign(ctx, client, slot, l)
	if err != nil {
		return fmt.Errorf("releasing pool slot %s: verifying repo before delete: %w", slot, err)
	}
	if gone {
		a.logf("[slots] released %s (repo already gone or replaced; nothing deleted)", slot)
		return nil
	}

	// The live client must not replay this DELETE after a timeout or 5xx:
	// the first attempt may have landed and the name may since have been
	// recreated by another allocator. The identity re-check below decides
	// what to do after such a failure.
	err = client.DeleteRepo(gh.WithoutAmbiguousRetry(ctx), slot.Org, slot.Repo)
	a.observeQuota(slot.Org, client)
	if err != nil && !forge.IsNotFound(err) {
		// The delete may have succeeded server-side with the response
		// lost; re-check by identity instead of blindly re-sending it.
		if gone, checkErr := a.repoGoneOrForeign(ctx, client, slot, l); checkErr == nil && gone {
			a.logf("[slots] released %s (delete reported %v but the repo is gone)", slot, err)
			return nil
		}
		return fmt.Errorf("releasing pool slot %s: deleting repo: %w", slot, err)
	}
	a.logf("[slots] released %s (%s)", slot, a.quotaSummary(slot.Org))
	return nil
}

// repoGoneOrForeign reports whether no repository under slot's name
// belongs to lease l: it is absent, or it is a different repository.
// A lookup failure other than not-found is returned.
func (a *slotAllocator) repoGoneOrForeign(ctx context.Context, client forge.Client, slot Slot, l lease) (bool, error) {
	repo, err := client.GetRepo(ctx, slot.Org, slot.Repo)
	a.observeQuota(slot.Org, client)
	if err != nil {
		if forge.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return !l.owns(repo), nil
}

// Held returns the slots currently leased by this allocator, sorted.
func (a *slotAllocator) Held() []Slot {
	a.mu.Lock()
	defer a.mu.Unlock()
	slots := make([]Slot, 0, len(a.held))
	for s := range a.held {
		slots = append(slots, s)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].String() < slots[j].String() })
	return slots
}

// QuotaDiagnostics renders the last observed rate-limit state of every
// org this allocator has talked to, one "org: state" entry per org.
func (a *slotAllocator) QuotaDiagnostics() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	orgs := sortedKeys(a.quota)
	lines := make([]string, 0, len(orgs))
	for _, org := range orgs {
		lines = append(lines, fmt.Sprintf("%s: %s", org, a.quota[org]))
	}
	return strings.Join(lines, "\n")
}

func (a *slotAllocator) isHeld(slot Slot) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.held[slot]
	return ok
}

// orgBlockedUntil reports whether org is still inside a rate-limit
// window, and until when.
func (a *slotAllocator) orgBlockedUntil(org string) (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	until, ok := a.blockedUntil[org]
	if !ok {
		return time.Time{}, false
	}
	if !a.now().Before(until) {
		delete(a.blockedUntil, org)
		return time.Time{}, false
	}
	return until, true
}

// observeQuota records the client's latest rate-limit headers for org
// and closes the org to claims until reset when the remaining budget is
// at or below rateLimitReserve.
func (a *slotAllocator) observeQuota(org string, client forge.Client) {
	r, ok := client.(forge.RateLimitReporter)
	if !ok {
		return
	}
	rl, seen := r.RateLimit()
	if !seen {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.quota[org] = rl
	if rl.Remaining <= rateLimitReserve && rl.Reset.After(a.now()) {
		a.extendBlockLocked(org, rl.Reset)
	}
}

// blockOrg closes org to claims after a rate-limit error. The window
// ends at the error's Retry-After deadline when it carries one, else
// after defaultRateLimitWindow; an exhausted primary quota with a later
// reset extends it to that reset. Returns the end of the window.
func (a *slotAllocator) blockOrg(org string, client forge.Client, cause error) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	until := a.now().Add(defaultRateLimitWindow)
	if ra, ok := gh.RetryAfterFromError(cause); ok {
		until = a.now().Add(ra)
	}
	if r, ok := client.(forge.RateLimitReporter); ok {
		if rl, seen := r.RateLimit(); seen && rl.Remaining <= rateLimitReserve && rl.Reset.After(until) {
			until = rl.Reset
		}
	}
	a.extendBlockLocked(org, until)
	return a.blockedUntil[org]
}

func (a *slotAllocator) extendBlockLocked(org string, until time.Time) {
	if cur, ok := a.blockedUntil[org]; !ok || until.After(cur) {
		a.blockedUntil[org] = until
	}
}

func (a *slotAllocator) quotaSummary(org string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	rl, ok := a.quota[org]
	if !ok {
		return "quota unknown"
	}
	return "quota " + rl.String()
}

// isBackoffError reports whether err means the org is throttling or too
// slow to answer, so claims should move to another org for a while.
func isBackoffError(err error) bool {
	return gh.IsRateLimitError(err) || errors.Is(err, context.DeadlineExceeded)
}

// isDeniedError reports whether err means the credentials or the call
// were refused.
func isDeniedError(err error) bool {
	return forge.IsForbidden(err) || isHTTPStatus(err, http.StatusUnauthorized, http.StatusForbidden)
}

// isHTTPStatus reports whether err wraps a GitHub API error with one of
// the given status codes.
func isHTTPStatus(err error, codes ...int) bool {
	var apiErr *gh.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return slices.Contains(codes, apiErr.StatusCode)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func joinOrgErrors(m map[string]error) string {
	orgs := sortedKeys(m)
	items := make([]string, 0, len(orgs))
	for _, org := range orgs {
		items = append(items, fmt.Sprintf("%s (%v)", org, m[org]))
	}
	return strings.Join(items, ", ")
}
