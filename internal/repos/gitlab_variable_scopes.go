package repos

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/fullsend-ai/fullsend/internal/forge"
)

// GitLabAcceptUnverifiedVariableScopesVar names the project CI/CD variable
// with which a project Maintainer accepts inherited CI/CD variable scopes
// that the installing credential cannot inspect. Its value is a comma- or
// whitespace-separated list of scopes, each either "instance" (instance-level
// variables on self-managed GitLab) or "group:<full/group/path>" (the
// variables of one ancestor group). Fullsend reads it but never writes it.
const GitLabAcceptUnverifiedVariableScopesVar = "FULLSEND_GITLAB_ACCEPT_UNVERIFIED_VARIABLE_SCOPES"

// unverifiedVariableScope is an inherited CI/CD variable scope whose
// variables could not be listed with the installing credential.
type unverifiedVariableScope struct {
	// key is the scope's name in GitLabAcceptUnverifiedVariableScopesVar.
	key string
	// reason explains why the scope could not be inspected.
	reason string
}

// gitlabInheritedVariableOverrideProblem checks the instance-level and
// ancestor-group CI/CD variables a project inherits for a definition of one
// of the predefined variables the trigger-pipeline evaluator treats as fixed
// (the keys of env). Such a variable takes precedence over the predefined
// value, so the evaluator could conclude that a job is excluded from trigger
// pipelines when GitLab would in fact run it.
//
// Every scope the credential can read is checked, and a definition found in
// any of them is a problem regardless of acknowledgement. Listing
// instance-level variables needs administrator access and listing group
// variables needs group Owner access, which a project Maintainer usually
// lacks. A scope that answers forbidden is unverified: inability to inspect
// it does not show that an override exists, but nor does it rule one out.
// The fast path therefore proceeds only when every unverified scope is named
// in GitLabAcceptUnverifiedVariableScopesVar, a project variable only a
// project Maintainer can set; otherwise the returned problem names each
// unaccepted scope and the value that would accept them. Errors other than
// forbidden mean the overrides could not be checked at all.
func gitlabInheritedVariableOverrideProblem(ctx context.Context, client forge.Client, owner, repo string, env map[string]*string) (string, error) {
	var unverified []unverifiedVariableScope
	// Instance-level variables (self-managed GitLab) are inherited by every
	// project.
	instanceVars, instanceErr := client.ListInstanceVariables(ctx)
	switch {
	case forge.IsForbidden(instanceErr):
		unverified = append(unverified, unverifiedVariableScope{
			key:    "instance",
			reason: "instance-level CI/CD variables cannot be inspected without administrator access",
		})
	case instanceErr != nil:
		return "", safeAPIError("checking instance variable overrides", instanceErr)
	}
	for _, variable := range instanceVars {
		if _, reserved := env[variable.Name]; reserved {
			return "webhook fast-path deferred: predefined instance variable override: " + variable.Name, nil
		}
	}
	// owner may contain only the first path component for nested projects.
	namespace := owner
	if slash := strings.LastIndex(repo, "/"); slash >= 0 {
		namespace += "/" + repo[:slash]
	}
	parts := strings.Split(namespace, "/")
	for i := range parts {
		group := strings.Join(parts[:i+1], "/")
		inherited, groupErr := client.ListOrgVariables(ctx, group)
		if forge.IsNotFound(groupErr) && i == 0 && len(parts) == 1 {
			continue
		} // personal namespace
		if forge.IsForbidden(groupErr) {
			// The remaining groups are still checked: a readable scope's
			// override must be reported even when another scope is unreadable.
			unverified = append(unverified, unverifiedVariableScope{
				key:    "group:" + group,
				reason: fmt.Sprintf("group variables of %q cannot be inspected without group Owner access", group),
			})
			continue
		}
		if groupErr != nil {
			return "", safeAPIError("checking inherited group variable overrides", groupErr)
		}
		for _, variable := range inherited {
			if _, reserved := env[variable.Name]; reserved {
				return "webhook fast-path deferred: predefined group variable override: " + variable.Name, nil
			}
		}
	}
	if len(unverified) == 0 {
		return "", nil
	}
	accepted, err := acceptedUnverifiedVariableScopes(ctx, client, owner, repo)
	if err != nil {
		return "", err
	}
	var reasons, keys []string
	for _, scope := range unverified {
		keys = append(keys, scope.key)
		if !accepted[scope.key] {
			reasons = append(reasons, scope.reason)
		}
	}
	if len(reasons) == 0 {
		return "", nil
	}
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("webhook fast-path deferred: %s, so predefined variable overrides cannot be ruled out and the managed credentials are revoked; after confirming that none of %s is defined there, a project Maintainer can accept the unverified scopes by setting the project CI/CD variable %s to %q",
		strings.Join(reasons, "; "), strings.Join(names, ", "), GitLabAcceptUnverifiedVariableScopesVar, strings.Join(keys, ",")), nil
}

// acceptedUnverifiedVariableScopes reads the scopes listed in
// GitLabAcceptUnverifiedVariableScopesVar. An absent variable accepts none.
// Entries must match a scope key exactly; anything else is ignored.
func acceptedUnverifiedVariableScopes(ctx context.Context, client forge.Client, owner, repo string) (map[string]bool, error) {
	raw, exists, err := client.GetRepoVariable(ctx, owner, repo, GitLabAcceptUnverifiedVariableScopesVar)
	if err != nil {
		return nil, safeAPIError("reading "+GitLabAcceptUnverifiedVariableScopesVar, err)
	}
	accepted := map[string]bool{}
	if !exists {
		return accepted, nil
	}
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		accepted[entry] = true
	}
	return accepted, nil
}
