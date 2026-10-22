package repos

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issue8691CI is the shape reported in #8691: a job inherits rules that
// reject trigger (and schedule) pipelines through two levels of extends, and
// another job reuses the same rules through !reference.
const issue8691CI = `
.build_time_only_rules:
  rules:
    - if: '$CI_PIPELINE_SOURCE == "trigger"'
      when: never
    - if: '$CI_PIPELINE_SOURCE == "schedule"'
      when: never

.base:
  image:
    name: registry.fedoraproject.org/fedora:latest
  extends:
    - .build_time_only_rules

test-tox:
  variables:
    KUBERNETES_CPU_REQUEST: "2"
    PIP_CACHE_DIR: "$CI_PROJECT_DIR/.cache/pip"
  stage: test
  extends:
    - .base
  script: [tox]
`

const issue8691ReferenceJob = `
lint:
  stage: test
  script: [./lint.sh]
  rules:
    - !reference [.build_time_only_rules, rules]
    - when: on_success
`

const (
	rejectTriggerTemplate = ".reject:\n  rules:\n    - if: '$CI_PIPELINE_SOURCE == \"trigger\"'\n      when: never\n    - when: on_success\n"
	admitTemplate         = ".admit:\n  rules:\n    - when: always\n"
)

// A job's extends: and the !reference tags in its rules are resolved against
// templates in the same file under GitLab's merge semantics; anything that
// cannot be resolved still fails closed (#8691).
func TestTriggerExtraWork_ExtendsAndReferenceResolution(t *testing.T) {
	cases := map[string]struct {
		ci     string
		shared func(string) bool
		want   string // "" means no problem
	}{
		"nested extends list rejecting trigger": {ci: issue8691CI},
		"rules reused through !reference":       {ci: issue8691CI + issue8691ReferenceJob},
		"extended rules admit trigger": {
			ci:   admitTemplate + "job:\n  script: [x]\n  extends: .admit\n",
			want: `job "job" that a trigger pipeline would run alongside the dispatcher`,
		},
		"extended template without rules": {
			ci:   ".img:\n  image: alpine\njob:\n  script: [x]\n  extends: .img\n",
			want: `job "job" that a trigger pipeline would run`,
		},
		"job rules override rejecting template rules": {
			ci:   rejectTriggerTemplate + "job:\n  script: [x]\n  extends: .reject\n  rules:\n    - when: always\n",
			want: `job "job" that a trigger pipeline would run`,
		},
		"job rules override admitting template rules": {
			ci: admitTemplate + "job:\n  script: [x]\n  extends: .admit\n  rules:\n    - if: '$CI_PIPELINE_SOURCE == \"trigger\"'\n      when: never\n",
		},
		"later extends entry wins (admitting)": {
			ci:   rejectTriggerTemplate + admitTemplate + "job:\n  script: [x]\n  extends: [.reject, .admit]\n",
			want: `job "job" that a trigger pipeline would run`,
		},
		"later extends entry wins (rejecting)": {
			ci: rejectTriggerTemplate + admitTemplate + "job:\n  script: [x]\n  extends: [.admit, .reject]\n",
		},
		"later extends entry without rules keeps earlier rules": {
			ci: rejectTriggerTemplate + ".img:\n  image: alpine\njob:\n  script: [x]\n  extends: [.reject, .img]\n",
		},
		"inherited when never without rules": {
			ci: ".off:\n  when: never\njob:\n  script: [x]\n  extends: .off\n",
		},
		"extends inherited through a YAML merge key": {
			ci: rejectTriggerTemplate + ".m: &m\n  extends: .reject\njob:\n  <<: *m\n  script: [x]\n",
		},
		"extending a template whose rules use !reference": {
			ci: rejectTriggerTemplate + ".t:\n  rules: !reference [.reject, rules]\njob:\n  script: [x]\n  extends: .t\n",
		},
		"cyclic extends": {
			ci:   ".a:\n  extends: .b\n.b:\n  extends: .a\njob:\n  script: [x]\n  extends: .a\n",
			want: `job "job" whose extends chain is cyclic`,
		},
		"over-deep extends chain": {
			ci:   deepExtendsChain(gitlabMaxExtendsDepth + 2),
			want: "nested more than 11 levels",
		},
		"missing extends target": {
			ci:   "job:\n  script: [x]\n  extends: .missing\n",
			want: `job "job" whose extends target ".missing" is not defined in the same file`,
		},
		"extends target defined in another file": {
			ci:     rejectTriggerTemplate + "job:\n  script: [x]\n  extends: .reject\n",
			shared: func(n string) bool { return n == ".reject" },
			want:   `extends target ".reject" is also defined in another file the pipeline loads`,
		},
		"extends target that is not a mapping": {
			ci:   ".s: text\njob:\n  script: [x]\n  extends: .s\n",
			want: `extends target ".s" is not a mapping`,
		},
		"only on an extended template": {
			ci:   rejectTriggerTemplate + ".o:\n  only: [main]\njob:\n  script: [x]\n  extends: [.reject, .o]\n",
			want: "uses only or except",
		},
		"except on a job that extends": {
			ci:   rejectTriggerTemplate + "job:\n  script: [x]\n  extends: .reject\n  except: [tags]\n",
			want: "uses only or except",
		},
		"interpolated extends value": {
			ci:   rejectTriggerTemplate + "job:\n  script: [x]\n  extends: $[[ inputs.template ]]\n",
			want: "extends value is not a literal template name",
		},
		"!reference to admitting rules": {
			ci:   admitTemplate + "job:\n  script: [x]\n  rules:\n    - !reference [.admit, rules]\n",
			want: `job "job" that a trigger pipeline would run`,
		},
		"nested !reference": {
			ci: rejectTriggerTemplate + ".a:\n  rules:\n    - !reference [.reject, rules]\njob:\n  script: [x]\n  rules: !reference [.a, rules]\n",
		},
		"!reference to a single rule": {
			ci: ".never_trigger:\n  if: '$CI_PIPELINE_SOURCE == \"trigger\"'\n  when: never\njob:\n  script: [x]\n  rules:\n    - !reference [.never_trigger]\n    - when: on_success\n",
		},
		"self-referential !reference": {
			ci:   ".a:\n  rules: !reference [.a, rules]\njob:\n  script: [x]\n  rules: !reference [.a, rules]\n",
			want: "more than 10 levels deep",
		},
		"!reference to a missing key": {
			ci:   rejectTriggerTemplate + "job:\n  script: [x]\n  rules: !reference [.reject, nope]\n",
			want: "!reference [.reject, nope], which does not resolve in the same file",
		},
		"!reference to a missing template": {
			ci:   "job:\n  script: [x]\n  rules:\n    - !reference [.gone, rules]\n",
			want: `!reference target ".gone" is not defined in the same file`,
		},
		"!reference with a non-literal path": {
			ci:   rejectTriggerTemplate + "job:\n  script: [x]\n  rules:\n    - !reference ['$[[ inputs.t ]]', rules]\n",
			want: "not a list of literal keys",
		},
		"!reference target defined in another file": {
			ci:     rejectTriggerTemplate + "job:\n  script: [x]\n  rules: !reference [.reject, rules]\n",
			shared: func(n string) bool { return n == ".reject" },
			want:   `!reference target ".reject" is also defined in another file`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := triggerExtraWorkProblemForSource([]byte(tc.ci), "the committed .gitlab-ci.yml", "main", "trigger", nil, nil, nil, tc.shared)
			if tc.want == "" {
				assert.Empty(t, got)
				return
			}
			assert.Contains(t, got, tc.want)
			assert.Contains(t, got, "Fullsend does not edit user-owned jobs")
		})
	}
}

// The unevaluable diagnostic names the extends construct that blocked the
// scan, instead of the generic "cannot be evaluated" text.
func TestTriggerExtraWork_UnresolvedExtendsNamesTheConstruct(t *testing.T) {
	got := triggerExtraWorkProblemForSource([]byte("test-tox:\n  script: [x]\n  extends: [.base]\n"), "the committed .gitlab-ci.yml", "main", "trigger", nil, nil, nil, nil)

	assert.Equal(t, `the committed .gitlab-ci.yml has job "test-tox" whose extends target ".base" is not defined in the same file (templates from includes are not resolved), so its exclusion from trigger pipelines cannot be established; add rules to it that reject $CI_PIPELINE_SOURCE == "trigger" pipelines (Fullsend does not edit user-owned jobs)`, got)
}

// The issue's configuration no longer defers the webhook fast path, and the
// activation gate's source=pipeline and source=schedule checks accept it too.
func TestGitLabExtraWork_IssueConfigWithExtendsIsAccepted(t *testing.T) {
	e := readyTriggerDispatchEnv(t)
	commitShippedScaffold(t, e)
	e.appendRootCI(t, issue8691CI)

	fastPath, err := gitlabTriggerExtraWorkProblem(context.Background(), e.state, trigOwner, trigRepo, trigBranch)
	require.NoError(t, err)
	assert.Empty(t, fastPath)

	gate, err := gitlabDispatchExtraWorkProblem(context.Background(), e.state, trigOwner, trigRepo, trigBranch)
	require.NoError(t, err)
	assert.Empty(t, gate)
}

// A template that another file of the pipeline also defines is deep-merged by
// GitLab, so the other file could add rules; extends does not resolve
// through it.
func TestGitLabExtraWork_ExtendsTargetSharedWithWrapperFailsClosed(t *testing.T) {
	e := readyTriggerDispatchEnv(t)
	commitShippedScaffold(t, e)
	e.appendRootCI(t, issue8691CI)
	key := trigOwner + "/" + trigRepo + "/" + fullsendPipelineInclude
	e.state.FileContents[key] = append(e.state.FileContents[key], []byte("\n.base:\n  rules:\n    - when: always\n")...)

	problem, err := gitlabTriggerExtraWorkProblem(context.Background(), e.state, trigOwner, trigRepo, trigBranch)
	require.NoError(t, err)
	assert.Contains(t, problem, `job "test-tox" whose extends target ".base" is also defined in another file the pipeline loads`)
}

// End to end: the webhook fast path is provisioned for the issue's
// configuration.
func TestEnsureGitLabWebhookFastPath_ExtendsExcludingTriggerIsProvisioned(t *testing.T) {
	c := newWebhookFake()
	c.FileContents[triggerJobsPrefix+".gitlab-ci.yml"] = append(c.FileContents[triggerJobsPrefix+".gitlab-ci.yml"], []byte(issue8691CI+issue8691ReferenceJob)...)

	res := ensureWebhook(t, c, false, false)

	assert.Equal(t, "update", res.Action, strings.Join(res.Details, "\n"))
	assert.Len(t, c.triggers(), 1)
	assert.Len(t, c.hooks(), 1)
}

func TestSharedTopLevelKeys(t *testing.T) {
	shared := sharedTopLevelKeys(
		[]byte(".a:\n  x: 1\n.b:\n  x: 1\n"),
		nil,
		[]byte("---\nspec:\n  inputs: {}\n---\n.b:\n  y: 2\n.c: {}\n"),
		[]byte("not: [valid\n"),
	)

	assert.False(t, shared(".a"))
	assert.True(t, shared(".b"))
	assert.False(t, shared(".c"))
	assert.False(t, shared("spec"), "only the configuration document counts")
}

func deepExtendsChain(n int) string {
	var b strings.Builder
	b.WriteString(rejectTriggerTemplate)
	prev := ".reject"
	for i := range n {
		name := ".t" + strings.Repeat("x", i)
		fmt.Fprintf(&b, "%s:\n  extends: %s\n", name, prev)
		prev = name
	}
	fmt.Fprintf(&b, "job:\n  script: [x]\n  extends: %s\n", prev)
	return b.String()
}
