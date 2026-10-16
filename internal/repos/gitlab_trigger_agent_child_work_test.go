package repos

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/scaffold"
)

// commitShippedScaffold commits the shipped typed wrapper, agent, poll and
// dispatcher templates and a root that includes the wrapper, the committed
// state a typed installation reaches before dispatch can be activated.
func commitShippedScaffold(t *testing.T, e *trigEnv) {
	t.Helper()
	prefix := trigOwner + "/" + trigRepo + "/"
	for _, path := range []string{fullsendPipelineInclude, fullsendAgentTemplatePath, fullsendPollTemplatePath, fullsendDispatcherTemplatePath} {
		content, err := scaffold.GitLabPerRepoFile(path)
		require.NoError(t, err)
		e.state.FileContents[prefix+path] = content
	}
	base, err := newGitLabCI()
	require.NoError(t, err)
	root, err := mergeGitLabCIWithWrapper(base, e.state.FileContents[prefix+fullsendPipelineInclude])
	require.NoError(t, err)
	e.state.FileContents[prefix+".gitlab-ci.yml"] = root
}

func (e *trigEnv) appendRootCI(t *testing.T, extra string) {
	t.Helper()
	key := trigOwner + "/" + trigRepo + "/.gitlab-ci.yml"
	e.state.FileContents[key] = append(e.state.FileContents[key], []byte(extra)...)
}

// appendAgentTemplate appends to the committed agent template's config
// document (the last document, after spec:inputs).
func (e *trigEnv) appendAgentTemplate(t *testing.T, extra string) {
	t.Helper()
	key := trigOwner + "/" + trigRepo + "/" + fullsendAgentTemplatePath
	e.state.FileContents[key] = append(e.state.FileContents[key], []byte(extra)...)
}

// The shipped scaffold alone is never reported: the agent include that the
// wrapper admits for exactly api and pipeline sources is the one allowed work
// in an agent child pipeline, and the poll and dispatcher includes reject
// source=pipeline.
func TestDispatchExtraWork_ShippedScaffoldIsClean(t *testing.T) {
	e := readyTriggerDispatchEnv(t)
	commitShippedScaffold(t, e)

	problem, err := gitlabDispatchExtraWorkProblem(context.Background(), e.state, trigOwner, trigRepo, trigBranch)
	require.NoError(t, err)
	assert.Empty(t, problem)
	gate := e.dispatchGate()
	assert.True(t, gate.Ready, gate.Deferred)
}

// An unrelated user job that a source=pipeline pipeline would run receives
// the protected credentials of the agent child pipeline, so the activation
// gate defers (#8438). A job excluded from every source is fine.
func TestDispatchExtraWork_UnrelatedJobInAgentChildPipelineDefersActivation(t *testing.T) {
	cases := map[string]struct {
		extra string
		want  string // "" means no problem
	}{
		"unguarded job": {
			extra: "\ndeploy:\n  script: [./deploy.sh]\n",
			want:  "trigger pipeline would run",
		},
		"job excluded only from trigger pipelines": {
			extra: "\ndeploy:\n  script: [./deploy.sh]\n  rules:\n    - if: '$CI_PIPELINE_SOURCE == \"trigger\"'\n      when: never\n    - when: on_success\n",
			want:  "agent child pipeline (source=pipeline)",
		},
		"job with rules that cannot be evaluated for source=pipeline": {
			extra: "\ndeploy:\n  script: [./deploy.sh]\n  rules:\n    - if: '$CI_PIPELINE_SOURCE == \"trigger\"'\n      when: never\n    - changes: [src/**]\n",
			want:  "source=pipeline",
		},
		"job excluded from trigger and pipeline sources": {
			extra: "\ndeploy:\n  script: [./deploy.sh]\n  rules:\n    - if: '$CI_PIPELINE_SOURCE == \"trigger\" || $CI_PIPELINE_SOURCE == \"pipeline\"'\n      when: never\n    - when: on_success\n",
		},
		"job that only runs for merge requests": {
			extra: "\nlint:\n  script: [./lint.sh]\n  rules:\n    - if: '$CI_PIPELINE_SOURCE == \"merge_request_event\"'\n",
		},
		"predefined variable override": {
			extra: "\n.tpl:\n  variables:\n    CI_PIPELINE_SOURCE: schedule\n",
			want:  "predefined-variable override",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := readyTriggerDispatchEnv(t)
			commitShippedScaffold(t, e)
			e.appendRootCI(t, tc.extra)

			problem, err := gitlabDispatchExtraWorkProblem(context.Background(), e.state, trigOwner, trigRepo, trigBranch)
			require.NoError(t, err)
			gate := e.dispatchGate()
			if tc.want == "" {
				assert.Empty(t, problem)
				assert.True(t, gate.Ready, gate.Deferred)
				return
			}
			assert.Contains(t, problem, tc.want)
			assert.False(t, gate.Ready)
			joined := strings.Join(gate.Deferred, "\n")
			assert.Contains(t, joined, tc.want)
			assert.NotContains(t, joined, "webhook fast-path deferred", "the gate reports the reason without the fast-path prefix")
		})
	}
}

// The webhook fast path on its own never starts an agent child pipeline, so a
// job that only a source=pipeline pipeline would run does not defer it; the
// source=pipeline check belongs to the activation gate.
func TestDispatchExtraWork_FastPathCheckIsTriggerOnly(t *testing.T) {
	e := readyTriggerDispatchEnv(t)
	commitShippedScaffold(t, e)
	e.appendRootCI(t, "\ndeploy:\n  script: [./deploy.sh]\n  rules:\n    - if: '$CI_PIPELINE_SOURCE == \"trigger\"'\n      when: never\n    - when: on_success\n")

	problem, err := gitlabTriggerExtraWorkProblem(context.Background(), e.state, trigOwner, trigRepo, trigBranch)
	require.NoError(t, err)
	assert.Empty(t, problem)
}

// An agent include in a user file is allowed for source=pipeline only because
// it is the scaffold agent template; any other include is still reported.
func TestDispatchExtraWork_OtherIncludeIsReported(t *testing.T) {
	e := readyTriggerDispatchEnv(t)
	commitShippedScaffold(t, e)
	key := trigOwner + "/" + trigRepo + "/.gitlab-ci.yml"
	root := strings.Replace(string(e.state.FileContents[key]), "include:\n", "include:\n  - local: other.yml\n    rules:\n      - if: '$CI_PIPELINE_SOURCE == \"trigger\"'\n        when: never\n      - when: always\n", 1)
	e.state.FileContents[key] = []byte(root)

	problem, err := gitlabDispatchExtraWorkProblem(context.Background(), e.state, trigOwner, trigRepo, trigBranch)
	require.NoError(t, err)
	assert.Contains(t, problem, "source=pipeline")
	assert.Contains(t, problem, "include")
}

// An unreadable committed file cannot establish exclusion, so the gate defers
// instead of activating.
func TestDispatchExtraWork_ReadFailureDefersActivation(t *testing.T) {
	e := readyTriggerDispatchEnv(t)
	commitShippedScaffold(t, e)
	e.state.Errors["GetFileContent"] = errTrigInjected

	gate := e.dispatchGate()
	assert.False(t, gate.Ready)
	assert.Contains(t, strings.Join(gate.Deferred, "\n"), "could not verify that unrelated jobs are excluded")
}

// The committed agent template is included by name for source=pipeline, so its
// own contents must be scanned too: an extra job, include, predefined-variable
// override or job-level execution surface committed there would run in an agent
// child pipeline with the protected agent credentials outside run-agent-job.sh's
// creator and HMAC checks (#8438).
func TestDispatchExtraWork_UnrelatedWorkInAgentTemplateDefersActivation(t *testing.T) {
	const agentJobPrefix = "\"fullsend $[[ inputs.stage | expand_vars ]] agent\":\n  stage: agent\n"
	cases := map[string]struct {
		mutate func(t *testing.T, e *trigEnv)
		want   string
	}{
		"extra job in the agent template": {
			mutate: func(t *testing.T, e *trigEnv) { e.appendAgentTemplate(t, "\ndeploy:\n  script: [./deploy.sh]\n") },
			want:   "agent child pipeline (source=pipeline)",
		},
		"extra include in the agent template": {
			mutate: func(t *testing.T, e *trigEnv) { e.appendAgentTemplate(t, "\ninclude:\n  - local: evil.yml\n") },
			want:   "source=pipeline",
		},
		"job-level after_script on the agent job": {
			mutate: func(t *testing.T, e *trigEnv) {
				key := trigOwner + "/" + trigRepo + "/" + fullsendAgentTemplatePath
				e.state.FileContents[key] = []byte(strings.Replace(string(e.state.FileContents[key]),
					agentJobPrefix, agentJobPrefix+"  after_script: [./leak.sh]\n", 1))
			},
			want: "job-level after_script on the agent job",
		},
		"predefined-variable override in the agent template": {
			mutate: func(t *testing.T, e *trigEnv) {
				e.appendAgentTemplate(t, "\n.tpl:\n  variables:\n    CI_PIPELINE_SOURCE: schedule\n")
			},
			want: "predefined-variable override",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := readyTriggerDispatchEnv(t)
			commitShippedScaffold(t, e)
			tc.mutate(t, e)

			problem, err := gitlabDispatchExtraWorkProblem(context.Background(), e.state, trigOwner, trigRepo, trigBranch)
			require.NoError(t, err)
			assert.Contains(t, problem, tc.want)
			assert.Contains(t, problem, fullsendAgentTemplatePath)
			gate := e.dispatchGate()
			assert.False(t, gate.Ready)

			// The webhook fast path never starts an agent child pipeline, so
			// agent-template-only work does not defer it.
			fastPath, err := gitlabTriggerExtraWorkProblem(context.Background(), e.state, trigOwner, trigRepo, trigBranch)
			require.NoError(t, err)
			assert.Empty(t, fastPath)
		})
	}
}

// Inherited global execution defaults must be evaluated for the agent job
// separately from the dispatcher: a dispatcher that sets inherit: default:
// false does not stop the agent job from inheriting a global after_script in
// the agent child pipeline (#8438).
func TestDispatchExtraWork_AgentInheritsGlobalEvenWhenDispatcherDisablesInheritance(t *testing.T) {
	e := readyTriggerDispatchEnv(t)
	commitShippedScaffold(t, e)

	// The dispatcher disables default inheritance (so the dispatcher-derived
	// check alone would skip the global), but the agent job does not.
	dispKey := trigOwner + "/" + trigRepo + "/" + fullsendDispatcherTemplatePath
	e.state.FileContents[dispKey] = []byte(strings.Replace(string(e.state.FileContents[dispKey]),
		"\"fullsend webhook dispatcher\":\n  stage: dispatch\n",
		"\"fullsend webhook dispatcher\":\n  stage: dispatch\n  inherit:\n    default: false\n", 1))
	// A global after_script in the agent template is inherited by the agent job.
	e.appendAgentTemplate(t, "\ndefault:\n  after_script: [./leak.sh]\n")

	problem, err := gitlabDispatchExtraWorkProblem(context.Background(), e.state, trigOwner, trigRepo, trigBranch)
	require.NoError(t, err)
	assert.Contains(t, problem, "after_script")
	assert.Contains(t, problem, "agent job")
	gate := e.dispatchGate()
	assert.False(t, gate.Ready)
}
