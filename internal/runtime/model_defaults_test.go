package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/ui"
)

// Both Claude-family runtimes default the three aliases to the same ids
// (#8426), and every alias with a default has a chain back to the previous
// one.
func TestClaudeAliasDefaults_MatchPiTable(t *testing.T) {
	for alias, id := range claudeAliasDefaults {
		assert.Equal(t, id, piModelAliases[alias], "pi and Claude Code default %s alike", alias)
		assert.NotEmpty(t, claudeAliasFallbacks[alias], "%s has a built-in fallback chain", alias)
	}
	assert.Equal(t, "claude-opus-5-5", claudeAliasDefaults["opus"])
	assert.Equal(t, "claude-sonnet-5-5", claudeAliasDefaults["sonnet"])
	assert.Equal(t, "claude-haiku-5-5", claudeAliasDefaults["haiku"])
	assert.Equal(t, "claude-fable-5-1", piModelAliases["fable"], "fable is unchanged")
}

func TestImplicitFallbackModels(t *testing.T) {
	assert.Equal(t, []string{"claude-opus-4-6"}, ImplicitFallbackModels("opus", nil))
	assert.Equal(t, []string{"claude-sonnet-4-6"}, ImplicitFallbackModels(" sonnet ", nil))
	assert.Equal(t, []string{"claude-haiku-4-5"}, ImplicitFallbackModels("haiku", map[string]string{"sonnet": "claude-sonnet-5"}),
		"a retarget of another alias leaves this one's chain alone")

	assert.Nil(t, ImplicitFallbackModels("sonnet", map[string]string{"sonnet": "claude-sonnet-5"}),
		"a models.aliases retarget has no implicit chain")
	assert.Nil(t, ImplicitFallbackModels("fable", nil), "fable has no chain")
	assert.Nil(t, ImplicitFallbackModels("claude-opus-5-5", nil), "a pinned id has no chain")
	assert.Nil(t, ImplicitFallbackModels("anthropic-vertex/claude-opus-5-5", nil), "a provider/id spec has no chain")
	assert.Nil(t, ImplicitFallbackModels("", nil))

	got := ImplicitFallbackModels("opus", nil)
	got[0] = "mutated"
	assert.Equal(t, []string{"claude-opus-4-6"}, ImplicitFallbackModels("opus", nil), "the table is not shared")
}

func TestEffectiveFallbackModels(t *testing.T) {
	assert.Equal(t, []string{"claude-opus-4-8"}, effectiveFallbackModels("opus", []string{"claude-opus-4-8"}, nil),
		"an explicit FULLSEND_FALLBACK_MODELS wins over the built-in chain")
	assert.Equal(t, []string{"claude-opus-4-6"}, effectiveFallbackModels("opus", nil, nil))
	assert.Nil(t, effectiveFallbackModels("claude-opus-5-5", nil, nil))
}

func TestModelFallbackLoop(t *testing.T) {
	now := func() time.Time { return time.Unix(1000, 0) }
	var tried []string
	attempt := func(model string, _ time.Duration, mayFallBack bool) string {
		tried = append(tried, model)
		if model == "c" {
			assert.False(t, mayFallBack, "the last model cannot fall back")
		}
		return model
	}
	notServed := func(s string) bool { return s != "c" }

	res, served := modelFallbackLoop([]string{"a", "b", "c"}, time.Minute, time.Second, now, attempt, notServed,
		func(string, string, time.Duration) bool { return true })
	assert.Equal(t, "c", res)
	assert.Equal(t, 2, served)
	assert.Equal(t, []string{"a", "b", "c"}, tried)

	tried = nil
	res, served = modelFallbackLoop([]string{"a", "b"}, time.Minute, time.Second, now, attempt, notServed,
		func(string, string, time.Duration) bool { return false })
	assert.Equal(t, "a", res, "a refused fallback makes the abandoned attempt final")
	assert.Equal(t, 0, served)
	assert.Equal(t, []string{"a"}, tried)
}

func TestClaudeModelID(t *testing.T) {
	assert.Equal(t, "claude-opus-5-5", claudeModelID("opus", nil))
	assert.Equal(t, "claude-sonnet-5", claudeModelID("sonnet", map[string]string{"sonnet": "claude-sonnet-5"}))
	assert.Equal(t, "fable", claudeModelID("fable", nil), "fable stays on Claude Code's own mapping")
	assert.Equal(t, "claude-opus-4-6", claudeModelID("claude-opus-4-6", nil))
}

func TestClaudeModelChain(t *testing.T) {
	cases := []struct {
		name   string
		params RunParams
		gw     bool
		want   []string
	}{
		{"alias gets its built-in chain", RunParams{Model: "opus"}, false, []string{"claude-opus-5-5", "claude-opus-4-6"}},
		{"explicit list wins", RunParams{Model: "sonnet", FallbackModels: []string{"haiku", "claude-sonnet-4-6"}}, false,
			[]string{"claude-sonnet-5-5", "claude-haiku-5-5", "claude-sonnet-4-6"}},
		{"duplicates and provider/id entries are dropped", RunParams{Model: "opus", FallbackModels: []string{"opus", "anthropic-vertex/x", "claude-opus-4-6"}}, false,
			[]string{"claude-opus-5-5", "claude-opus-4-6"}},
		{"retarget has no implicit chain", RunParams{Model: "sonnet", ModelAliases: map[string]string{"sonnet": "claude-sonnet-5"}}, false, []string{"sonnet"}},
		{"retarget with an explicit list", RunParams{Model: "sonnet", ModelAliases: map[string]string{"sonnet": "claude-sonnet-5"}, FallbackModels: []string{"claude-sonnet-4-6"}}, false,
			[]string{"claude-sonnet-5", "claude-sonnet-4-6"}},
		{"pinned id", RunParams{Model: "claude-opus-5-5", FallbackModels: []string{"claude-opus-4-6"}}, false, []string{"claude-opus-5-5"}},
		{"provider/id", RunParams{Model: "anthropic-vertex/claude-opus-5-5"}, false, []string{"anthropic-vertex/claude-opus-5-5"}},
		{"fable has no chain", RunParams{Model: "fable"}, false, []string{"fable"}},
		{"gateway runs never fall back", RunParams{Model: "opus"}, true, []string{"opus"}},
		{"no model", RunParams{}, false, []string{""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.params.SandboxName = "fs-model-chain-none"
			assert.Equal(t, tc.want, claudeModelChain(tc.params, tc.gw))
		})
	}

	t.Run("agent frontmatter alias when the run names none", func(t *testing.T) {
		const sb = "fs-model-chain-frontmatter"
		claudeAgentModels.Store(sb, "haiku")
		t.Cleanup(func() { claudeAgentModels.Delete(sb) })
		assert.Equal(t, []string{"claude-haiku-5-5", "claude-haiku-4-5"}, claudeModelChain(RunParams{SandboxName: sb}, false))
		assert.Equal(t, []string{"claude-opus-4-6"}, claudeModelChain(RunParams{SandboxName: sb, Model: "claude-opus-4-6"}, false),
			"the run's model wins over the frontmatter")
	})
}

func TestClaudeShouldFallBack(t *testing.T) {
	notFound := "API Error: 404 {\"error\":{\"code\":404,\"message\":\"Publisher model projects/p/locations/us-east5/publishers/anthropic/models/claude-opus-5-5 not found.\",\"status\":\"NOT_FOUND\"}}"
	dataSharing := "API Error: 403 Access to this model requires data sharing to be enabled for publisher 'anthropic'"
	errResult := func(msg string) *ResultEvent { return &ResultEvent{IsError: true, ErrorMessage: msg} }

	assert.True(t, claudeShouldFallBack(claudeAttemptResult{exitCode: 1, lastResult: errResult(notFound)}))
	assert.True(t, claudeShouldFallBack(claudeAttemptResult{exitCode: 1, lastResult: errResult(dataSharing)}))

	assert.False(t, claudeShouldFallBack(claudeAttemptResult{lastResult: errResult("API Error: 403 PERMISSION_DENIED: caller lacks aiplatform.endpoints.predict")}),
		"another 403 is final")
	assert.False(t, claudeShouldFallBack(claudeAttemptResult{lastResult: errResult("API Error: 429 quota exceeded")}), "quota is final")
	assert.False(t, claudeShouldFallBack(claudeAttemptResult{lastResult: &ResultEvent{ErrorMessage: notFound}}), "not an error result")
	assert.False(t, claudeShouldFallBack(claudeAttemptResult{lastResult: errResult(notFound), answered: true}), "the model already answered")
	assert.False(t, claudeShouldFallBack(claudeAttemptResult{lastResult: &ResultEvent{IsError: true, ErrorMessage: notFound, OutputTokens: 3}}), "output tokens were spent")
	assert.False(t, claudeShouldFallBack(claudeAttemptResult{execErr: io.EOF, lastResult: errResult(notFound)}), "infrastructure failure")
	assert.False(t, claudeShouldFallBack(claudeAttemptResult{}), "no result")
}

func TestClaudeAttemptOutput(t *testing.T) {
	assert.False(t, claudeAttemptOutput(TextEvent{Text: "API Error: 404 not found"}), "the synthetic API error message is not output")
	assert.True(t, claudeAttemptOutput(TextEvent{Text: "Looking at the issue"}))
	assert.True(t, claudeAttemptOutput(ToolUseEvent{Name: "Bash"}))
	assert.True(t, claudeAttemptOutput(ThinkingEvent{Text: "hmm"}))
	assert.False(t, claudeAttemptOutput(InitEvent{Model: "m"}))
	assert.False(t, claudeAttemptOutput(ResultEvent{}))
}

func TestBuildRunCommand_FallbackModelsUseBuiltinDefault(t *testing.T) {
	cmd := buildRunCommand(RunParams{
		AgentBaseName:  "agent",
		Model:          "claude-opus-5-5",
		FallbackModels: []string{"sonnet", "claude-haiku-4-5"},
		RepoDir:        "/sandbox/workspace/repo",
	})
	assert.Contains(t, cmd, "--fallback-model 'claude-sonnet-5-5,claude-haiku-4-5'",
		"an aliased fallback entry gets its built-in default id")
	assert.Contains(t, cmd, "--model 'claude-opus-5-5'")
}

// fakeOpenshellClaude installs an openshell stub whose Claude launch answers
// with the stream in streams for the first model it finds in the command,
// and records every invocation to logPath.
func fakeOpenshellClaude(t *testing.T, logPath string, streams map[string]string) {
	t.Helper()
	dir := t.TempDir()
	var cases strings.Builder
	for model, stream := range streams {
		f := filepath.Join(dir, model+".ndjson")
		require.NoError(t, os.WriteFile(f, []byte(stream), 0o644))
		// Claude Code exits 1 when its result is an error.
		code := "0"
		if strings.Contains(stream, `"is_error":true`) {
			code = "1"
		}
		fmt.Fprintf(&cases, "  *\"--model '%s'\"*) cat '%s'; exit %s ;;\n", model, f, code)
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + logPath + "'\ncase \"$*\" in\n" + cases.String() + "esac\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "openshell"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

const claudeNotServedStream = `{"type":"system","subtype":"init","model":"claude-opus-5-5","claude_code_version":"2.1.296","session_id":"0b7b6c8a-930e-4750-89b9-3129edaa5ccf"}
{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: 404 Publisher model projects/p/locations/us-east5/publishers/anthropic/models/claude-opus-5-5 not found."}]}}
{"type":"result","subtype":"success","is_error":true,"result":"API Error: 404 Publisher model projects/p/locations/us-east5/publishers/anthropic/models/claude-opus-5-5 not found.","num_turns":1,"total_cost_usd":0}
`

const claudeServedStream = `{"type":"system","subtype":"init","model":"claude-opus-4-6","claude_code_version":"2.1.296","session_id":"1c8c7d9b-0000-4750-89b9-3129edaa5ccf"}
{"type":"assistant","message":{"model":"claude-opus-4-6","content":[{"type":"text","text":"done"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"done","num_turns":1,"total_cost_usd":0.01,"usage":{"input_tokens":10,"output_tokens":2}}
`

// A project that does not serve the alias's default runs on the fallback:
// the abandoned attempt leaves no events and no transcript behind, and the
// fallback is recorded in the metrics.
func TestClaudeRuntime_Run_FallsBackWhenNotServed(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "openshell.log")
	fakeOpenshellClaude(t, logPath, map[string]string{
		"claude-opus-5-5": claudeNotServedStream,
		"claude-opus-4-6": claudeServedStream,
	})

	var events []AgentEvent
	var metrics RunMetrics
	exit, err := ClaudeRuntime{}.Run(context.Background(), RunParams{
		SandboxName: "fs-claude-fallback", AgentBaseName: "code", RepoDir: "/r", Model: "opus",
		Timeout: 30 * time.Second, OnEvent: func(e AgentEvent) { events = append(events, e) },
	}, ui.New(io.Discard), time.Now(), &metrics)
	require.NoError(t, err)
	assert.Equal(t, 0, exit)

	assert.Equal(t, "claude-opus-4-6", metrics.Model)
	assert.Equal(t, []string{"claude-opus-5-5"}, metrics.FallbackFrom)
	for _, e := range events {
		if txt, ok := e.(TextEvent); ok {
			assert.NotContains(t, txt.Text, "API Error", "the abandoned attempt's events are discarded")
		}
		if res, ok := e.(ResultEvent); ok {
			assert.False(t, res.IsError, "only the served attempt's result is rendered")
		}
	}

	log, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(log), "--model 'claude-opus-5-5'")
	assert.Contains(t, string(log), "--model 'claude-opus-4-6'")
	assert.NotContains(t, string(log), "--fallback-model", "the built-in chain is not passed to Claude Code")
	assert.Contains(t, string(log), "0b7b6c8a-930e-4750-89b9-3129edaa5ccf.jsonl", "the abandoned transcript is removed")
}

// Any other error is final: no second attempt.
func TestClaudeRuntime_Run_OtherErrorsDoNotFallBack(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "openshell.log")
	denied := strings.ReplaceAll(claudeNotServedStream,
		"404 Publisher model projects/p/locations/us-east5/publishers/anthropic/models/claude-opus-5-5 not found.",
		"403 PERMISSION_DENIED: Permission denied on resource project p.")
	fakeOpenshellClaude(t, logPath, map[string]string{"claude-opus-5-5": denied, "claude-opus-4-6": claudeServedStream})

	var metrics RunMetrics
	exit, err := ClaudeRuntime{}.Run(context.Background(), RunParams{
		SandboxName: "fs-claude-nofallback", AgentBaseName: "code", RepoDir: "/r", Model: "opus",
		Timeout: 30 * time.Second, OnEvent: func(AgentEvent) {},
	}, ui.New(io.Discard), time.Now(), &metrics)
	require.NoError(t, err)
	assert.Equal(t, 1, exit)
	assert.Empty(t, metrics.FallbackFrom)

	log, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.NotContains(t, string(log), "--model 'claude-opus-4-6'")
}

// pi Run applies the alias's built-in chain with no FULLSEND_FALLBACK_MODELS
// set, and records the fallback in the metrics.
func TestPiRuntimeRun_ImplicitFallbackWhenNotServed(t *testing.T) {
	t.Setenv("FULLSEND_PI_MODEL", "")
	t.Setenv(piProviderEnv, "")
	work := t.TempDir()
	store := filepath.Join(work, "store")
	logPath := filepath.Join(work, "openshell.log")
	fixture, err := filepath.Abs(filepath.Join("testdata", "pi", "basic_run.ndjson"))
	require.NoError(t, err)
	fakeOpenshellPi(t, logPath, store, fixture)
	inner, err := exec.LookPath("openshell")
	require.NoError(t, err)

	notServed := filepath.Join(work, "not-served.ndjson")
	msg := `{"role":"assistant","content":[],"model":"claude-opus-5-5","provider":"anthropic-vertex","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"total":0}},"stopReason":"error","errorMessage":"404 Publisher model projects/p/locations/us-east5/publishers/anthropic/models/claude-opus-5-5 not found."}`
	require.NoError(t, os.WriteFile(notServed, []byte(`{"type":"agent_start"}`+"\n"+
		`{"type":"message_end","message":`+msg+"}\n"+
		`{"type":"agent_end","messages":[`+msg+"]}\n"), 0o644))
	wrapDir := t.TempDir()
	wrapper := "#!/bin/sh\ncase \"$*\" in\n  *\"--model 'anthropic-vertex/claude-opus-5-5'\"*) echo \"$@\" >> '" + logPath + "'; cat '" + notServed + "'; exit 0 ;;\nesac\nexec '" + inner + "' \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(wrapDir, "openshell"), []byte(wrapper), 0o755))
	t.Setenv("PATH", wrapDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	require.NoError(t, PiRuntime{}.Bootstrap(bootstrapInput{
		sandboxName: "sb", agentPath: writeAgentFile(t, testAgentDef), agentName: "triage",
	}))
	var events []AgentEvent
	var metrics RunMetrics
	exit, err := PiRuntime{}.Run(context.Background(), RunParams{
		SandboxName: "sb", AgentBaseName: "triage", RepoDir: "/sandbox/workspace/repo", Model: "opus",
		Timeout: 30 * time.Second, OnEvent: func(e AgentEvent) { events = append(events, e) },
	}, ui.New(io.Discard), time.Now(), &metrics)
	require.NoError(t, err)
	assert.Equal(t, 0, exit)
	assert.Equal(t, "claude-opus-4-6", metrics.Model)
	assert.Equal(t, []string{"claude-opus-5-5"}, metrics.FallbackFrom)
	for _, e := range events {
		if res, ok := e.(ResultEvent); ok {
			assert.False(t, res.IsError, "the abandoned attempt's error is not rendered")
		}
	}
	log, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(log), "--model 'anthropic-vertex/claude-opus-5-5'")
	assert.Contains(t, string(log), "--model 'anthropic-vertex/claude-opus-4-6'")
}

func TestPiAgentModelFallbacks(t *testing.T) {
	assert.Equal(t, map[string][]string{
		"opus":   {"anthropic-vertex/claude-opus-4-6"},
		"sonnet": {"anthropic-vertex/claude-sonnet-4-6"},
		"haiku":  {"anthropic-vertex/claude-haiku-4-5"},
	}, piAgentModelFallbacks(nil))
	got := piAgentModelFallbacks(map[string]string{"sonnet": "claude-sonnet-5", "opus": "x", "haiku": "y"})
	assert.Nil(t, got, "every alias retargeted leaves no table")
}

func TestPiApplyChildFallbacks(t *testing.T) {
	strp := func(s string) *string { return &s }
	m := &piAgentManifest{
		ModelFallbacks:  piAgentModelFallbacks(nil),
		SubagentDefault: "anthropic-vertex/claude-sonnet-5-5",
		Personas: map[string]piPersonaManifestEntry{
			"challenger":  {Model: "anthropic-vertex/claude-opus-5-5"},
			"correctness": {Model: "anthropic-vertex/claude-haiku-5-5"},
			"pinned":      {Model: "anthropic-vertex/claude-opus-5-5"},
			"inherits":    {},
			"defaulted":   {Model: "anthropic-vertex/claude-sonnet-5-5"},
		},
	}
	personas := []piPersona{
		{Name: "challenger", Model: "sonnet"},
		{Name: "correctness", Model: "haiku@default"},
		{Name: "pinned", Model: "claude-opus-5-5"},
		{Name: "inherits"},
		{Name: "defaulted"},
	}
	cfg := map[string]*string{"default": strp("sonnet"), "challenger": strp("opus")}
	piApplyChildFallbacks(m, personas, cfg)

	assert.Equal(t, []string{"anthropic-vertex/claude-sonnet-4-6"}, m.SubagentDefaultFallbacks)
	assert.Equal(t, []string{"anthropic-vertex/claude-opus-4-6"}, m.Personas["challenger"].Fallbacks, "subagents.<persona> outranks the frontmatter")
	assert.Equal(t, []string{"anthropic-vertex/claude-haiku-4-5"}, m.Personas["correctness"].Fallbacks, "an @suffix is dropped")
	assert.Nil(t, m.Personas["pinned"].Fallbacks, "a pinned id never falls back")
	assert.Nil(t, m.Personas["inherits"].Fallbacks, "an inheriting persona takes the parent's live model")
	assert.Equal(t, []string{"anthropic-vertex/claude-sonnet-4-6"}, m.Personas["defaulted"].Fallbacks, "subagents.default applies to a persona that says nothing")

	pinnedDefault := &piAgentManifest{ModelFallbacks: piAgentModelFallbacks(nil), SubagentDefault: "anthropic-vertex/claude-sonnet-5-5"}
	piApplyChildFallbacks(pinnedDefault, nil, map[string]*string{"default": strp("anthropic-vertex/claude-sonnet-5-5")})
	assert.Nil(t, pinnedDefault.SubagentDefaultFallbacks)
}
