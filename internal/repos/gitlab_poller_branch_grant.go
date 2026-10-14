package repos

import (
	"errors"
	"fmt"
	"sort"

	"github.com/fullsend-ai/fullsend/internal/forge"
)

// Replacement Poller protected-branch grants (#8409).
//
// A fresh Poller generation is a new user ID, so a protected default branch
// that admits the current Poller by user does not admit its replacement. When
// the existing policy does not let the replacement start pipelines there, the
// handoff adds exactly one per-user merge-access entry for that account on the
// branch's exact-name rule. It never widens access to a role, never edits a
// wildcard rule (which also covers other branches), and never touches another
// entry. The entry ID is recorded in the generation document as installer-
// owned (PollerBranchGrant), so only that entry is ever revoked: when the
// generation is quarantined or abandoned, and by retirement once a later
// generation supersedes it. A user entry that already admitted the account,
// for example one an administrator added, is never recorded and never
// removed.

// PollerBranchGrant identifies the single protected-branch merge-access entry
// the installer added for a Poller generation account. It is recorded only for
// an entry fullsend added itself, so revoking it by entry ID never removes an
// administrator's grant.
//
// The attempt is recorded before the rule is changed, with the entry IDs that
// existed beforehand and no entry ID yet (an unresolved attempt). If the run is
// interrupted, or the entry cannot be identified afterwards, the next run knows
// an entry may exist. An unresolved attempt is never cleared by inference: not
// by an entry-ID difference (an administrator may have added that entry), and
// not by elapsed time (a request that was sent may still commit, and no
// server-side bound or fencing exists). It stays recorded, blocks the handoff
// and cutover, and is reconciled by the operator. Entries that appeared after
// the snapshot are never claimed or removed; they are left in place and named
// in the report.
type PollerBranchGrant struct {
	Branch  string `json:"branch"`
	EntryID int64  `json:"entry_id,omitempty"`
	// PriorEntryIDs are the merge-access entry IDs that existed before an
	// unresolved attempt (EntryID zero). They only serve to name the entries
	// that appeared afterwards, and are cleared once the entry is identified.
	PriorEntryIDs []int `json:"prior_entry_ids,omitempty"`
}

// unresolved reports whether the grant is an attempt whose entry was not yet
// identified.
func (g *PollerBranchGrant) unresolved() bool { return g != nil && g.EntryID == 0 }

// validatePollerBranchGrant checks a recorded grant: it needs a positive
// owning account and a branch name, and a positive entry ID unless it is an
// unresolved attempt on a pending generation (allowUnresolved). A nil grant is
// valid.
func validatePollerBranchGrant(field string, g *PollerBranchGrant, ownerID int64, allowUnresolved bool) error {
	if g == nil {
		return nil
	}
	if ownerID <= 0 {
		return fmt.Errorf("%s requires its recorded Poller account ID", field)
	}
	if g.Branch == "" || g.EntryID < 0 || (g.EntryID == 0 && !allowUnresolved) {
		return fmt.Errorf("%s requires a branch and a positive entry ID", field)
	}
	if g.EntryID > 0 && len(g.PriorEntryIDs) > 0 {
		return fmt.Errorf("%s records attempt details for an identified entry", field)
	}
	return nil
}

// revokeBranchGrant removes exactly the recorded installer-owned merge-access
// entry of account uid, on a bounded context detached from cancellation. An
// entry or rule that is already gone succeeds. The failure keeps only its
// class.
//
// An unresolved attempt never removes or clears anything, and always returns an
// error so the obligation stays recorded: a request fullsend sent may still
// commit, and an entry that appeared for the account after the recorded
// snapshot may be an administrator's. Such entries are left in place and named
// in the error for the operator to reconcile.
func (g *pollerGenerationRun) revokeBranchGrant(uid int64, grant *PollerBranchGrant) error {
	if grant == nil {
		return nil
	}
	ctx, cancel := gitlabCleanupContext(g.ctx)
	defer cancel()
	if grant.unresolved() {
		observed := "an entry may still appear"
		rule, err := g.client.GetProtectedBranch(ctx, g.owner, g.repo, grant.Branch)
		if err != nil {
			observed += fmt.Sprintf(" (rereading the branch failed: %v)", safeAPIError("rereading the protected branch", err))
		} else if rule != nil {
			prior := map[int]struct{}{}
			for _, id := range grant.PriorEntryIDs {
				prior[id] = struct{}{}
			}
			var unattributed []int
			for _, l := range rule.MergeAccessLevels {
				if _, existed := prior[l.ID]; int64(l.UserID) == uid && l.ID > 0 && !existed {
					unattributed = append(unattributed, l.ID)
				}
			}
			if len(unattributed) > 0 {
				observed = fmt.Sprintf("merge-access entry ID(s) %v of that user appeared after the attempt was recorded and are left in place (fullsend cannot tell whether it or an administrator added them), and an entry may still appear", unattributed)
			}
		}
		return fmt.Errorf("the protected-branch merge grant attempt for GitLab Poller user ID %d on %q is unresolved: fullsend cannot prove that a request it sent will not commit, so it never clears or revokes the attempt on its own; %s", uid, grant.Branch, observed)
	}
	if err := g.client.RevokeProtectedBranchMergeUserEntry(ctx, g.owner, g.repo, grant.Branch, int(grant.EntryID), int(uid)); err != nil {
		return safeAPIError(fmt.Sprintf("removing protected-branch merge-access entry ID %d of GitLab Poller user ID %d on %q", grant.EntryID, uid, grant.Branch), err)
	}
	return nil
}

// releasePendingBranchGrant revokes the pending generation's recorded grant
// and returns the generation without it. On failure the grant stays recorded
// and an operator-facing clause naming the exact entry is returned.
func (g *pollerGenerationRun) releasePendingBranchGrant(p PendingPollerGeneration) (PendingPollerGeneration, string) {
	if p.BranchGrant == nil {
		return p, ""
	}
	if err := g.revokeBranchGrant(p.UserID, p.BranchGrant); err != nil {
		if p.BranchGrant.unresolved() {
			return p, fmt.Sprintf("; its protected-branch merge grant attempt for user ID %d on %q is still unresolved (%v)", p.UserID, p.BranchGrant.Branch, err)
		}
		return p, fmt.Sprintf("; also remove its protected-branch merge-access entry ID %d on %q (%v)", p.BranchGrant.EntryID, p.BranchGrant.Branch, err)
	}
	if !p.BranchGrant.unresolved() {
		g.details = append(g.details, fmt.Sprintf("Removed the protected-branch merge access fullsend granted the replacement GitLab Poller (user ID %d) on %q (entry ID %d)", p.UserID, p.BranchGrant.Branch, p.BranchGrant.EntryID))
	}
	p.BranchGrant = nil
	return p, ""
}

// mergeEntryIDs returns the IDs of the rule's merge-access entries.
func mergeEntryIDs(rule *forge.ProtectedBranchRule) map[int]struct{} {
	ids := map[int]struct{}{}
	if rule == nil {
		return ids
	}
	for _, l := range rule.MergeAccessLevels {
		if l.ID > 0 {
			ids[l.ID] = struct{}{}
		}
	}
	return ids
}

// grantReplacementBranchAccess adds a per-user merge-access entry for the
// pending replacement account uid on the protected default branch when the
// existing policy does not let it start pipelines there, and records the entry
// as installer-owned before the account is used. It returns granted when an
// entry was added, and a deferReason when the grant cannot be made narrowly
// (only a wildcard rule protects the branch, or the installer may not edit the
// rule). A branch that is unprotected, or already admits the account through a
// role or an existing user entry, is left unchanged and nothing is recorded.
func (g *pollerGenerationRun) grantReplacementBranchAccess(uid int64) (granted bool, deferReason string, err error) {
	branch, rule, err := loadDefaultBranchRule(g.ctx, g.client, g.owner, g.repo)
	if err != nil {
		if forge.IsNotSupported(err) {
			return false, "", nil
		}
		return false, "", safeAPIError("reading the protected default branch for the replacement GitLab Poller", err)
	}
	if PollerCanCreatePipeline(rule, []int{int(uid)}) {
		return false, "", nil
	}
	if rule.Name != branch {
		return false, fmt.Sprintf("default branch %q is protected only by wildcard rule %q, and fullsend never edits a wildcard rule because it also covers other branches; add an exact-name protection rule for %q, or allow user ID %d to merge on it", branch, rule.Name, branch, uid), nil
	}

	p := *g.st.Pending
	// A grant recorded earlier no longer admits the account (it was removed,
	// or the default branch changed): remove what remains of it before adding
	// its replacement, so at most one installer-owned entry exists.
	if p.BranchGrant != nil {
		if revErr := g.revokeBranchGrant(uid, p.BranchGrant); revErr != nil {
			return false, "", revErr
		}
		p.BranchGrant = nil
		if writeErr := g.setPending(p); writeErr != nil {
			return false, "", writeErr
		}
	}

	// Record the attempt, with the entry IDs that exist beforehand, before the
	// rule is changed: an interrupted request, a failed reread, or a failed
	// rollback then still leaves a cleanup obligation instead of an applied
	// grant that a later run would mistake for a pre-existing one.
	prior := mergeEntryIDs(rule)
	priorIDs := make([]int, 0, len(prior))
	for id := range prior {
		priorIDs = append(priorIDs, id)
	}
	sort.Ints(priorIDs)
	p.BranchGrant = &PollerBranchGrant{Branch: branch, PriorEntryIDs: priorIDs}
	if writeErr := g.setPending(p); writeErr != nil {
		return false, "", writeErr
	}

	createdID, mutated, grantErr := g.client.GrantProtectedBranchMergeUserEntry(g.ctx, g.owner, g.repo, branch, int(uid))
	// Reread whatever the request returned: a failed request may still have
	// been applied, and the rule shows whether the account is now admitted.
	readCtx, cancel := gitlabCleanupContext(g.ctx)
	after, readErr := g.client.GetProtectedBranch(readCtx, g.owner, g.repo, branch)
	cancel()
	if readErr != nil {
		readErr = safeAPIError(fmt.Sprintf("rereading protected branch %q after granting the replacement GitLab Poller (user ID %d) merge access; a merge-access entry for that user may remain", branch, uid), readErr)
	}
	// The entry is identified only when the forge reports the entry its own
	// request created. It is never inferred from an entry-ID difference: an
	// administrator may have added an entry for the account in the meantime, and
	// a failed or ambiguous request may have created nothing.
	entryID := createdID
	added := &PollerBranchGrant{Branch: branch, EntryID: int64(entryID)}
	// Whether the request changed the rule is known unless it may have been
	// applied and the reread failed.
	outcomeKnown := !mutated || readErr == nil

	if grantErr != nil {
		// A request that was sent and failed without an answer from the forge
		// (a timeout or dropped connection) may still be committed after the
		// reread, so a reread that finds no entry does not resolve it. Only an
		// explicit refusal proves the rule was not changed.
		refused := forge.IsForbidden(grantErr) || forge.IsNotFound(grantErr)
		ambiguous := mutated && entryID == 0 && !refused
		var cleanupErr, recordErr error
		if entryID > 0 {
			cleanupErr = g.revokeBranchGrant(uid, added)
		}
		switch {
		case entryID == 0 && (ambiguous || (!outcomeKnown && !refused)):
			// The unresolved attempt stays recorded: it is never cleared on
			// inference, and the operator reconciles it. An explicit refusal is
			// not in this case even when the reread failed: the forge answered
			// that it did not change the rule, so no entry can appear later.
		case entryID > 0 && cleanupErr != nil:
			// Keep the known entry as a cleanup obligation.
			p.BranchGrant = added
			recordErr = g.setPending(p)
		default:
			p.BranchGrant = nil
			recordErr = g.setPending(p)
		}
		if readErr == nil && cleanupErr == nil && recordErr == nil && (forge.IsForbidden(grantErr) || forge.IsNotFound(grantErr)) {
			return false, fmt.Sprintf("the installer could not add user ID %d to the merge access of protected branch %q (%v); fullsend grants only that user on the exact-name rule and never widens access to Developers, so allow the installer token to edit that rule or allow user ID %d to merge on %q", uid, branch, safeAPIError("granting protected-branch merge access", grantErr), uid, branch), nil
		}
		return false, "", errors.Join(safeAPIError(fmt.Sprintf("granting the replacement GitLab Poller (user ID %d) merge access on protected branch %q", uid, branch), grantErr), readErr, cleanupErr, recordErr)
	}
	if !mutated {
		// The forge found the account already admitted and changed nothing, so
		// there is no entry of fullsend's to record: drop the attempt.
		p.BranchGrant = nil
		recordErr := g.setPending(p)
		if readErr != nil || recordErr != nil {
			return false, "", errors.Join(readErr, recordErr)
		}
		if !PollerCanCreatePipeline(after, []int{int(uid)}) {
			return false, "", fmt.Errorf("granting the replacement GitLab Poller (user ID %d) merge access on protected branch %q did not take effect", uid, branch)
		}
		g.details = append(g.details, fmt.Sprintf("The replacement GitLab Poller (user ID %d) was already admitted on protected branch %q when the grant ran, so fullsend added no entry and will not remove any", uid, branch))
		return true, "", nil
	}
	// The unresolved attempt stays recorded through every failure below until
	// the entry is identified, so the obligation is never lost.
	if readErr != nil {
		if entryID == 0 {
			return false, "", readErr
		}
		// The forge reported the entry its own request created, so the reread
		// failure does not hide it: record it as the cleanup obligation, or
		// revoke that exact entry when it cannot be recorded. Elevation stays
		// blocked for this run either way.
		p.BranchGrant = added
		if writeErr := g.setPending(p); writeErr != nil {
			return false, "", errors.Join(readErr, writeErr, g.revokeBranchGrant(uid, added))
		}
		return false, "", readErr
	}
	if !PollerCanCreatePipeline(after, []int{int(uid)}) {
		failure := fmt.Errorf("granting the replacement GitLab Poller (user ID %d) merge access on protected branch %q did not take effect", uid, branch)
		if entryID == 0 {
			return false, "", failure
		}
		// The forge reported the entry its own request created, so a reread
		// that no longer shows it does not erase the obligation: record the
		// identified entry (exact-ID revocation is a no-op if it is gone), or
		// revoke it when it cannot be recorded.
		p.BranchGrant = added
		if writeErr := g.setPending(p); writeErr != nil {
			return false, "", errors.Join(failure, writeErr, g.revokeBranchGrant(uid, added))
		}
		return false, "", failure
	}
	if entryID == 0 {
		return false, "", fmt.Errorf("granting the replacement GitLab Poller (user ID %d) merge access on protected branch %q changed the rule, but the new merge-access entry could not be identified; the attempt stays recorded and is never cleared on inference; the operator reconciles it", uid, branch)
	}

	p.BranchGrant = added
	if writeErr := g.setPending(p); writeErr != nil {
		// The unresolved attempt is still recorded; also undo the grant now.
		return false, "", errors.Join(writeErr, g.revokeBranchGrant(uid, added))
	}
	g.details = append(g.details, fmt.Sprintf("Granted the replacement GitLab Poller (user ID %d) merge access on protected branch %q (entry ID %d) so it can start pipelines; no other access was changed", uid, branch, entryID))
	return true, "", nil
}
