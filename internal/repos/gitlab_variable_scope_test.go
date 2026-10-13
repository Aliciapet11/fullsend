package repos

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/forge"
)

// An instance-level CI/CD variable can redefine a predefined variable the
// evaluator treats as fixed, so provisioning defers and an existing fast
// path is revoked.
func TestEnsureGitLabWebhookFastPath_InstanceVariableOverride(t *testing.T) {
	t.Run("fresh install defers", func(t *testing.T) {
		c := newWebhookFake()
		c.InstanceVariables = []string{"UNRELATED", "CI_PIPELINE_SOURCE"}

		res := ensureWebhook(t, c, false, false)

		assert.Equal(t, "deferred", res.Action)
		assert.Empty(t, c.CreatedTriggerTokens)
		assert.Empty(t, c.CreatedProjectHooks)
		assert.Contains(t, strings.Join(res.Details, "\n"), "predefined instance variable override: CI_PIPELINE_SOURCE")
	})

	t.Run("existing fast path is revoked", func(t *testing.T) {
		c := newWebhookFake()
		ensureWebhook(t, c, false, false)
		require.Len(t, c.triggers(), 1)
		c.InstanceVariables = []string{"CI_COMMIT_REF_PROTECTED"}

		res, err := ensureWebhookRaw(c)

		require.NoError(t, err)
		assert.Equal(t, "update", res.Action)
		assert.Empty(t, c.triggers())
		assert.Empty(t, c.hooks())
	})

	t.Run("unrelated instance variables do not defer", func(t *testing.T) {
		c := newWebhookFake()
		c.InstanceVariables = []string{"UNRELATED"}

		ensureWebhook(t, c, false, false)

		assert.Len(t, c.triggers(), 1)
	})

	t.Run("uninspectable instance variables defer and revoke", func(t *testing.T) {
		c := newWebhookFake()
		ensureWebhook(t, c, false, false)
		require.Len(t, c.triggers(), 1)
		c.Errors["ListInstanceVariables"] = fmt.Errorf("list instance variables: %w", forge.ErrForbidden)

		res, err := ensureWebhookRaw(c)

		require.NoError(t, err)
		assert.Equal(t, "update", res.Action)
		assert.Contains(t, strings.Join(res.Details, "\n"), "instance-level CI/CD variables cannot be inspected")
		assert.Empty(t, c.triggers())
		assert.Empty(t, c.hooks())
	})
}

// A project Maintainer cannot list group variables (group Owner access is
// required). That defers the fast path rather than failing the install,
// and still revokes an existing managed credential.
func TestEnsureGitLabWebhookFastPath_GroupVariablesForbiddenDefers(t *testing.T) {
	t.Run("fresh install defers without an error", func(t *testing.T) {
		c := newWebhookFake()
		c.Errors["ListOrgVariables"] = fmt.Errorf("listing group variables: %w", forge.ErrForbidden)

		res, err := ensureWebhookRaw(c)

		require.NoError(t, err)
		assert.Equal(t, "deferred", res.Action)
		assert.Empty(t, c.CreatedTriggerTokens)
		assert.Contains(t, strings.Join(res.Details, "\n"), "group Owner access")
	})

	t.Run("existing fast path is revoked", func(t *testing.T) {
		c := newWebhookFake()
		ensureWebhook(t, c, false, false)
		require.Len(t, c.triggers(), 1)
		c.Errors["ListOrgVariables"] = fmt.Errorf("listing group variables: %w", forge.ErrForbidden)

		res, err := ensureWebhookRaw(c)

		require.NoError(t, err)
		assert.Equal(t, "update", res.Action)
		assert.Empty(t, c.triggers())
	})

	t.Run("other group errors still fail", func(t *testing.T) {
		c := newWebhookFake()
		c.Errors["ListOrgVariables"] = errors.New("boom")

		_, err := ensureWebhookRaw(c)

		require.Error(t, err)
	})
}

// A same-named dispatcher job in the root or wrapper is merged over the
// included template's job, so the template-only readiness check cannot
// speak for the job that runs.
func TestGitLabWebhookReadiness_DispatcherJobOverride(t *testing.T) {
	override := "\n" + dispatcherJobHeader + "  rules:\n    - when: never\n"
	for name, path := range map[string]string{
		"root":    ".gitlab-ci.yml",
		"wrapper": fullsendPipelineInclude,
	} {
		t.Run(name, func(t *testing.T) {
			c := newWebhookFake()
			key := triggerJobsPrefix + path
			c.FileContents[key] = append(c.FileContents[key], []byte(override)...)

			res := ensureWebhook(t, c, false, false)

			assert.Equal(t, "deferred", res.Action)
			assert.Empty(t, c.CreatedTriggerTokens)
			assert.Contains(t, strings.Join(res.Details, "\n"), "override the dispatcher template")
		})
	}

	t.Run("convergence reports work after the override appears", func(t *testing.T) {
		c := newWebhookFake()
		ensureWebhook(t, c, false, false)
		key := triggerJobsPrefix + ".gitlab-ci.yml"
		c.FileContents[key] = append(c.FileContents[key], []byte(override)...)

		needs, err := GitLabWebhookNeedsProvisioning(context.Background(), c, webhookTestBase, webhookTestOwner, webhookTestRepo)

		require.NoError(t, err)
		assert.True(t, needs)
	})
}

// Early safety and project-lookup errors never echo server error text,
// which could contain a stored credential.
func TestGitLabTriggerSafety_EarlyErrorsWithholdServerText(t *testing.T) {
	const echoed = "echoed-credential-value"
	cases := map[string]struct {
		method string
		run    func(c webhookFake) error
	}{
		"override role on install": {"GetPipelineVariablesMinimumOverrideRole", func(c webhookFake) error {
			_, err := ensureWebhookRaw(c)
			return err
		}},
		"override role on reconcile": {"GetPipelineVariablesMinimumOverrideRole", func(c webhookFake) error {
			_, err := ReconcileGitLabWebhookSafety(context.Background(), c, webhookTestOwner, webhookTestRepo, false)
			return err
		}},
		"project lookup on install": {"GetRepo", func(c webhookFake) error {
			_, err := ensureWebhookRaw(c)
			return err
		}},
		"project lookup on reconcile": {"GetRepo", func(c webhookFake) error {
			_, err := ReconcileGitLabWebhookSafety(context.Background(), c, webhookTestOwner, webhookTestRepo, false)
			return err
		}},
		"trigger owner lookup on reconcile": {"GetProjectMemberAccessLevel", func(c webhookFake) error {
			_, err := ReconcileGitLabWebhookSafety(context.Background(), c, webhookTestOwner, webhookTestRepo, false)
			return err
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newWebhookFake()
			ensureWebhook(t, c, false, false)
			injected := errors.New("server said " + echoed)
			c.Errors[tc.method] = injected

			err := tc.run(c)

			require.Error(t, err)
			assert.NotContains(t, err.Error(), echoed)
			assert.ErrorIs(t, err, injected, "the original error stays reachable for errors.Is")
		})
	}
}

// acceptScopes records a project Maintainer's acceptance of unverified
// inherited variable scopes.
func acceptScopes(c webhookFake, value string) {
	c.VariableValues[webhookTestOwner+"/"+webhookTestRepo+"/"+GitLabAcceptUnverifiedVariableScopesVar] = value
}

// groupScopedFake answers ListOrgVariables per group: forbidden for the
// groups in forbidden, and the listed variable names for the groups in vars.
type groupScopedFake struct {
	webhookFake
	forbidden map[string]bool
	vars      map[string][]string
}

func (c groupScopedFake) ListOrgVariables(_ context.Context, group string) ([]forge.OrgVariable, error) {
	if c.forbidden[group] {
		return nil, fmt.Errorf("listing group variables: %w", forge.ErrForbidden)
	}
	var out []forge.OrgVariable
	for _, name := range c.vars[group] {
		out = append(out, forge.OrgVariable{Name: name})
	}
	return out, nil
}

// A project Maintainer without administrator or group Owner access can
// activate the fast path by explicitly accepting the scopes it cannot
// inspect. Readable scopes keep their collision detection, and scopes that
// were not accepted still defer and revoke.
func TestEnsureGitLabWebhookFastPath_AcceptedUnverifiedVariableScopes(t *testing.T) {
	forbidInstance := func(c webhookFake) {
		c.Errors["ListInstanceVariables"] = fmt.Errorf("list instance variables: %w", forge.ErrForbidden)
	}
	// nestedKey stores the acceptance of the nested project group/sub/app.
	nestedKey := webhookTestOwner + "/sub/" + webhookTestRepo + "/" + GitLabAcceptUnverifiedVariableScopesVar
	forbidGroups := func(c webhookFake) {
		c.Errors["ListOrgVariables"] = fmt.Errorf("listing group variables: %w", forge.ErrForbidden)
	}

	t.Run("accepted instance scope activates the fast path", func(t *testing.T) {
		c := newWebhookFake()
		forbidInstance(c)
		acceptScopes(c, "instance")

		res := ensureWebhook(t, c, false, false)

		assert.Equal(t, "update", res.Action)
		assert.Len(t, c.triggers(), 1)
		assert.Len(t, c.hooks(), 1)
		needs, err := GitLabWebhookNeedsProvisioning(context.Background(), c, webhookTestBase, webhookTestOwner, webhookTestRepo)
		require.NoError(t, err)
		assert.False(t, needs, "an accepted scope is not drift")
	})

	t.Run("accepted instance and group scopes activate the fast path", func(t *testing.T) {
		c := newWebhookFake()
		forbidInstance(c)
		forbidGroups(c)
		acceptScopes(c, " group:"+webhookTestOwner+",\ninstance ")

		ensureWebhook(t, c, false, false)

		assert.Len(t, c.triggers(), 1)
		assert.Len(t, c.hooks(), 1)
	})

	t.Run("unaccepted scope defers, names it, and revokes", func(t *testing.T) {
		c := newWebhookFake()
		ensureWebhook(t, c, false, false)
		require.Len(t, c.triggers(), 1)
		forbidInstance(c)
		forbidGroups(c)
		acceptScopes(c, "instance")

		res, err := ensureWebhookRaw(c)

		require.NoError(t, err)
		assert.Equal(t, "update", res.Action)
		joined := strings.Join(res.Details, "\n")
		assert.Contains(t, joined, `group variables of "group" cannot be inspected without group Owner access`)
		assert.NotContains(t, joined, "instance-level CI/CD variables cannot be inspected", "the accepted scope is not reported as unaccepted")
		assert.Contains(t, joined, GitLabAcceptUnverifiedVariableScopesVar+` to "instance,group:group"`)
		assert.Contains(t, joined, "CI_COMMIT_REF_PROTECTED")
		assert.Contains(t, joined, "CI_PIPELINE_SOURCE")
		assert.Empty(t, c.triggers())
		assert.Empty(t, c.hooks())
	})

	t.Run("acceptance must name the exact scope", func(t *testing.T) {
		c := newWebhookFake()
		forbidGroups(c)
		acceptScopes(c, "group:other,group,*")

		res := ensureWebhook(t, c, false, false)

		assert.Equal(t, "deferred", res.Action)
		assert.Empty(t, c.CreatedTriggerTokens)
	})

	t.Run("readable group override still defers when instance is accepted", func(t *testing.T) {
		c := newWebhookFake()
		forbidInstance(c)
		acceptScopes(c, "instance")
		c.OrgVariables = map[string]bool{webhookTestOwner + "/CI_PIPELINE_SOURCE": true}

		res := ensureWebhook(t, c, false, false)

		assert.Equal(t, "deferred", res.Action)
		assert.Empty(t, c.CreatedTriggerTokens)
		assert.Contains(t, strings.Join(res.Details, "\n"), "predefined group variable override: CI_PIPELINE_SOURCE")
	})

	t.Run("readable override in a later group is found past an unreadable one", func(t *testing.T) {
		base := newWebhookFake()
		c := groupScopedFake{
			webhookFake: base,
			forbidden:   map[string]bool{webhookTestOwner: true},
			vars:        map[string][]string{webhookTestOwner + "/sub": {"CI_COMMIT_REF_PROTECTED"}},
		}
		base.VariableValues[nestedKey] = "group:" + webhookTestOwner

		problem, err := gitlabInheritedVariableOverrideProblem(context.Background(), c, webhookTestOwner, "sub/"+webhookTestRepo, triggerPipelineEnv("main"))

		require.NoError(t, err)
		assert.Equal(t, "webhook fast-path deferred: predefined group variable override: CI_COMMIT_REF_PROTECTED", problem)
	})

	t.Run("nested unreadable group must be accepted by its full path", func(t *testing.T) {
		base := newWebhookFake()
		c := groupScopedFake{
			webhookFake: base,
			forbidden:   map[string]bool{webhookTestOwner + "/sub": true},
		}
		base.VariableValues[nestedKey] = "group:" + webhookTestOwner

		problem, err := gitlabInheritedVariableOverrideProblem(context.Background(), c, webhookTestOwner, "sub/"+webhookTestRepo, triggerPipelineEnv("main"))
		require.NoError(t, err)
		assert.Contains(t, problem, `group variables of "group/sub" cannot be inspected`)

		base.VariableValues[nestedKey] = "group:" + webhookTestOwner + "/sub"
		problem, err = gitlabInheritedVariableOverrideProblem(context.Background(), c, webhookTestOwner, "sub/"+webhookTestRepo, triggerPipelineEnv("main"))
		require.NoError(t, err)
		assert.Empty(t, problem)
	})

	t.Run("acceptance read failure fails closed", func(t *testing.T) {
		c := newWebhookFake()
		forbidInstance(c)
		c.Errors["GetRepoVariable"] = errors.New("server said secret")

		problem, err := gitlabInheritedVariableOverrideProblem(context.Background(), c, webhookTestOwner, webhookTestRepo, triggerPipelineEnv("main"))

		require.Error(t, err)
		assert.Empty(t, problem)
		assert.NotContains(t, err.Error(), "server said secret")
		assert.Contains(t, err.Error(), GitLabAcceptUnverifiedVariableScopesVar)
	})

	t.Run("acceptance is not read when every scope is readable", func(t *testing.T) {
		c := newWebhookFake()
		c.Errors["GetRepoVariable"] = errors.New("must not be called")

		problem, err := gitlabInheritedVariableOverrideProblem(context.Background(), c, webhookTestOwner, webhookTestRepo, triggerPipelineEnv("main"))

		require.NoError(t, err)
		assert.Empty(t, problem)
	})

	t.Run("non-forbidden instance errors still fail", func(t *testing.T) {
		c := newWebhookFake()
		c.Errors["ListInstanceVariables"] = errors.New("boom")
		acceptScopes(c, "instance")

		_, err := ensureWebhookRaw(c)

		require.Error(t, err)
	})
}
