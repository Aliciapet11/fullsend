package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// The gate Run uses to decide that a delegating agent's breakdown must carry
// the parent's entry even when it spawned nothing. It reads the runner-held
// record only: the manifest is agent-writable and never authority.
func TestCodexRunnerHeldDigestSet_Delegating(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		roles map[string]string
		want  bool
	}{
		{name: "nil map: no role registered", roles: nil, want: false},
		{name: "empty map: no role registered", roles: map[string]string{}, want: false},
		{name: "one role registered", roles: map[string]string{"default": "d0d0"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := codexRunnerHeldDigestSet{ConfigTOML: "cfg", RoleFiles: tt.roles}
			assert.Equal(t, tt.want, d.delegating(), "delegating follows the runner-held role set only")
		})
	}
}

// Thread ids of the fixtures (testdata/codex/README.md, "Child rollout
// fixtures") and of the spawn items in collab_spawn.ndjson.
const (
	codexChildSameModelID  = "01a06300-0000-7000-8000-0000000000a1"
	codexChildOtherModelID = "01a06300-0000-7000-8000-0000000000a2"
	codexChildFailedID     = "01a06300-0000-7000-8000-0000000000a3"
	codexChildNoModelID    = "01a06300-0000-7000-8000-0000000000a4"
	codexChildDeniedID     = "01a06300-0000-7000-8000-0000000000a9"
)

// codexChildFixtures maps each child id to its rollout fixture.
var codexChildFixtures = map[string]string{
	codexChildSameModelID:  "child_usage_same_model.jsonl",
	codexChildOtherModelID: "child_usage_other_model.jsonl",
	codexChildFailedID:     "child_usage_failed.jsonl",
	codexChildNoModelID:    "child_usage_no_model.jsonl",
}

// writeCodexChildRollouts lays the child fixtures out as codex would under a
// sessions directory (YYYY/MM/DD/rollout-<start>-<thread id>.jsonl) and
// returns that directory.
func writeCodexChildRollouts(t *testing.T, ids ...string) string {
	t.Helper()
	sessions := filepath.Join(t.TempDir(), "sessions")
	day := filepath.Join(sessions, "2026", "09", "29")
	require.NoError(t, os.MkdirAll(day, 0o755))
	for _, id := range ids {
		name := "rollout-2026-09-29T15-46-38-" + id + ".jsonl"
		require.NoError(t, os.WriteFile(filepath.Join(day, name), readCodexFixture(t, codexChildFixtures[id]), 0o644))
	}
	return sessions
}

// runCodexChildUsageRead executes the read command under /bin/sh against a
// local sessions directory and parses what it printed. The command is run,
// not inspected: a Go-level assertion on its text would not catch a quoting
// slip (the same reason codex_run_test.go executes the guards).
func runCodexChildUsageRead(t *testing.T, sessions string, ids []string) []codexChildUsage {
	t.Helper()
	cmd, err := codexChildUsageReadCommand(sessions, ids)
	require.NoError(t, err)
	out, err := exec.Command("/bin/sh", "-c", cmd).Output()
	require.NoError(t, err)
	return parseCodexChildUsage(out)
}

func TestCodexChildUsageReadCommand_RejectsBadIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   string
	}{
		{name: "path segment", id: "../etc/passwd"},
		{name: "upper case", id: "01A06300-0000-7000-8000-0000000000A1"},
		{name: "too short", id: "01a06300-0000-7000-8000-00000000a1"},
		{name: "shell text", id: "'; rm -rf /; '"},
		{name: "empty", id: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := codexChildUsageReadCommand("/sandbox/codex-config/sessions", []string{codexChildSameModelID, tt.id})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "is not a codex thread id")
		})
	}
}

func TestCodexChildUsageReadCommand_Executes(t *testing.T) {
	t.Parallel()

	sessions := writeCodexChildRollouts(t, codexChildSameModelID, codexChildOtherModelID, codexChildFailedID)
	// Unsorted, one id twice, one id with no rollout.
	ids := []string{codexChildFailedID, codexChildSameModelID, codexChildSameModelID, codexChildOtherModelID, "01a06300-0000-7000-8000-0000000000ff"}

	cmd, err := codexChildUsageReadCommand(sessions, ids)
	require.NoError(t, err)
	assert.Contains(t, cmd, fmt.Sprintf("command -p head -c %d", codexChildUsageMaxBytes),
		"the read is bounded inside the sandbox, not after the bytes arrived")
	assert.Contains(t, cmd, `-type f -name "rollout-*-$id.jsonl"`,
		"only regular rollout files, found by thread id; never a cat of the directory")
	assert.Equal(t, 1, strings.Count(cmd, shellQuote(codexChildSameModelID)), "a repeated id is read once")

	got := runCodexChildUsageRead(t, sessions, ids)
	require.Len(t, got, 3, "an id with no rollout yields no record; a repeated id one; sorted by id")
	assert.Equal(t, codexChildUsage{
		ID: codexChildSameModelID, Model: "gpt-5.6-luna", HasUsage: true,
		Usage: codexUsage{InputTokens: 7381, CacheWriteInputTokens: 7378, OutputTokens: 8},
	}, got[0], "the thread total of the record, not the sum of the two token_counts")
	assert.Equal(t, codexChildUsage{
		ID: codexChildOtherModelID, Model: "gpt-5.6-terra", HasUsage: true,
		Usage: codexUsage{InputTokens: 4000, CachedInputTokens: 1000, CacheWriteInputTokens: 2000, OutputTokens: 300, ReasoningOutputTokens: 100},
	}, got[1])
	assert.Equal(t, codexChildUsage{ID: codexChildFailedID, Model: "gpt-5.6-nonexistent"}, got[2],
		"a child that errored before its first response has a model and no usage")
}

// The read takes the thread total of the last token_usage_record. The
// token_count codex writes after a context-window overflow has its usage
// counters at zero, and a rate-limit token_count before the first response
// has no usage; neither is read.
func TestCodexChildUsageReadCommand_ReadsTheThreadTotal(t *testing.T) {
	t.Parallel()

	usage := func(in, out, total int) string {
		return fmt.Sprintf(`{"input_tokens":%d,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d}`, in, out, total)
	}
	count := func(info string) string {
		return `{"type":"event_msg","payload":{"type":"token_count","info":` + info + `}}`
	}
	record := func(in, out int) string {
		return `{"type":"token_usage_record","payload":{"thread_token_usage":` + usage(in, out, in+out) + `}}`
	}
	rollout := strings.Join([]string{
		`{"type":"turn_context","payload":{"model":"gpt-5.6-luna"}}`,
		count(`null`),
		record(3000, 4),
		count(`{"total_token_usage":` + usage(3000, 4, 3004) + `}`),
		record(90000, 20),
		count(`{"total_token_usage":` + usage(0, 0, 258400) + `,"last_token_usage":` + usage(0, 0, 168380) + `,"model_context_window":258400}`),
	}, "\n") + "\n"
	sessions := writeCodexChildRollouts(t)
	require.NoError(t, os.WriteFile(filepath.Join(sessions, "2026", "09", "29", "rollout-2026-09-29T15-46-38-"+codexChildSameModelID+".jsonl"), []byte(rollout), 0o644))

	got := runCodexChildUsageRead(t, sessions, []string{codexChildSameModelID})
	require.Len(t, got, 1)
	assert.Equal(t, codexChildUsage{
		ID: codexChildSameModelID, Model: "gpt-5.6-luna", HasUsage: true,
		Usage: codexUsage{InputTokens: 90000, OutputTokens: 20},
	}, got[0], "the last record's thread total, not the zeroed token_count after it")
}

func TestParseCodexChildUsage(t *testing.T) {
	t.Parallel()

	turn := func(model string) string {
		return `{"type":"turn_context","payload":{"model":"` + model + `"}}`
	}
	record := func(in, out int) string {
		return fmt.Sprintf(`{"type":"token_usage_record","payload":{"thread_token_usage":{"input_tokens":%d,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d}}}`, in, out, in+out)
	}
	data := strings.Join([]string{
		record(999, 999), // before any marker: nobody's line
		codexChildUsageMarker + " " + codexChildSameModelID,
		turn("gpt-5.6-terra"),
		turn("gpt-5.6-sol"),
		record(10, 1),
		"not json",
		record(20, 2),
		"",
		codexChildUsageMarker + " " + codexChildOtherModelID,
		record(5, 5),
		codexChildUsageMarker + " " + codexChildFailedID,
		turn("gpt-5.6-terra"),
		`{"type":"token_usage_record","payload":{"thread_token_usage":{"input_tokens":7,"cach`,
	}, "\n")

	got := parseCodexChildUsage([]byte(data))
	require.Len(t, got, 3)
	assert.Equal(t, codexChildUsage{ID: codexChildSameModelID, Model: "gpt-5.6-terra", HasUsage: true, Usage: codexUsage{InputTokens: 20, OutputTokens: 2}},
		got[0], "first turn_context names the model, the last record wins, a malformed line is skipped")
	assert.Equal(t, codexChildUsage{ID: codexChildOtherModelID, HasUsage: true, Usage: codexUsage{InputTokens: 5, OutputTokens: 5}},
		got[1], "no turn_context: the model stays empty and the fold buckets it")
	assert.Equal(t, codexChildUsage{ID: codexChildFailedID, Model: "gpt-5.6-terra"},
		got[2], "a truncated last line (the head -c cut) is not usage")
	assert.Empty(t, parseCodexChildUsage(nil))
}

func TestCodexChildIDs_CollectsCompletedSpawns(t *testing.T) {
	t.Parallel()

	stream := readCodexFixture(t, "collab_spawn.ndjson")
	want := []string{codexChildSameModelID, codexChildOtherModelID, codexChildFailedID}

	whole := &codexChildIDs{}
	n, err := whole.Write(stream)
	require.NoError(t, err)
	assert.Equal(t, len(stream), n, "an io.Writer reports every byte consumed or the TeeReader fails")
	assert.Equal(t, want, whole.IDs,
		"the three completed spawns in stream order; the failed spawn's receiver is not a child; wait and close items name no new thread")
	assert.NotContains(t, whole.IDs, codexChildDeniedID)

	chunked := &codexChildIDs{}
	for i := 0; i < len(stream); i += 7 {
		_, err := chunked.Write(stream[i:min(i+7, len(stream))])
		require.NoError(t, err)
	}
	assert.Equal(t, want, chunked.IDs, "lines are reassembled across Write calls, as a TeeReader delivers them")
}

func TestCodexChildIDs_SkipsOverlongLine(t *testing.T) {
	t.Parallel()

	spawn := `{"type":"item.completed","item":{"id":"item_1","type":"collab_tool_call","tool":"spawn_agent","sender_thread_id":"01a06300-0000-7000-8000-000000000050","receiver_thread_ids":["` + codexChildSameModelID + `"],"prompt":null,"agents_states":{},"status":"completed"}}`
	// A completed spawn naming another child, padded past the bound: if the
	// line were scanned, its receiver would be collected.
	overlong := `{"type":"item.completed","item":{"id":"item_0","type":"collab_tool_call","tool":"spawn_agent","sender_thread_id":"01a06300-0000-7000-8000-000000000050","receiver_thread_ids":["` + codexChildOtherModelID + `"],"prompt":"` + strings.Repeat("a", streamBufSize) + `","agents_states":{},"status":"completed"}}`

	c := &codexChildIDs{}
	_, err := c.Write([]byte(overlong + "\n" + spawn + "\n"))
	require.NoError(t, err)
	assert.Equal(t, []string{codexChildSameModelID}, c.IDs,
		"a line past the parser's own bound is dropped whole, as parseCodexStream drops it, even when it names a child; the next line is still scanned")
	assert.Empty(t, c.line, "nothing pending after a newline")
}

// A failed child is visible in the stream and never fails the run: codex
// exits 0 and completes the turn (paid probe, 2026-09-29), so the wait and
// close items are the only trace. Pinned here because the delegation
// instructions given to the parent depend on it.
func TestParseCodexStream_FailedCollabItem(t *testing.T) {
	t.Parallel()

	events, threadID := collectCodexEvents(t, "collab_spawn.ndjson")
	assert.Equal(t, "01a06300-0000-7000-8000-000000000050", threadID)

	tools := codexEventsOfType[ToolUseEvent](events)
	require.Len(t, tools, 9, "four spawns, two waits, three closes")
	for _, tool := range tools {
		assert.Equal(t, "Agent", tool.Name)
	}
	assert.Equal(t, "spawn_agent (1 agent(s))", tools[0].Summary)
	assert.Equal(t, "spawn_agent (1 agent(s)) (failed)", tools[3].Summary, "a failed spawn is reported, not hidden")
	assert.Equal(t, "wait (2 agent(s))", tools[4].Summary)
	assert.Equal(t, "wait (1 agent(s)) (failed)", tools[5].Summary, "the errored child's wait carries status failed")
	assert.Equal(t, "close_agent (1 agent(s)) (failed)", tools[8].Summary)

	result := codexOnlyResult(t, events)
	assert.False(t, result.IsError, "a failed child never sets the run verdict")
	assert.Equal(t, 1, result.NumTurns)
	assert.Equal(t, 15, result.InputTokens)
	assert.Equal(t, 1140, result.OutputTokens)
	assert.Equal(t, 291, result.ReasoningTokens)
	assert.Equal(t, 13618, result.CacheCreationInputTokens)
	assert.Equal(t, 40590, result.CacheReadInputTokens)
}

// fakeOpenshellCodexChildren is the fake openshell for the fold and run-level
// tests. It answers the manifest `cat` from storeDir (as fakeOpenshellCodex
// does), streams streamFixture for the codex launch, and executes the
// child-usage read command for real under sh with the sandbox sessions path
// rewritten to sessionsDir, so the command text is exercised, not stubbed.
// FULLSEND_TEST_CHILD_READ_FAIL makes that command exit 1;
// FULLSEND_TEST_CODEX_EXIT sets the launch's exit status.
func fakeOpenshellCodexChildren(t *testing.T, logPath, storeDir, streamFixture, sessionsDir string) {
	t.Helper()
	binDir := t.TempDir()
	script := `#!/bin/sh
echo "$@" >> '` + logPath + `'
if [ "$2" = "exec" ]; then
  for last; do :; done
  case "$last" in
    cat\ *) f=$(printf '%s' "${last#cat }" | tr -d "'" | tr '/' '_'); cat '` + storeDir + `'/"$f"; exit $? ;;
    *"exec --json"*) cat '` + streamFixture + `'; exit "${FULLSEND_TEST_CODEX_EXIT:-0}" ;;
    *` + codexChildUsageMarker + `*)
      if [ -n "${FULLSEND_TEST_CHILD_READ_FAIL:-}" ]; then echo "fake openshell: refusing the rollout read" >&2; exit 1; fi
      sh -c "$(printf '%s' "$last" | sed 's#` + CodexRuntime{}.codexSessionsDir() + `#` + sessionsDir + `#g')"; exit $? ;;
  esac
  exit 0
fi
exit 0
`
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "openshell"), []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// codexParentMetrics is the parent's own iteration as the stream fixture
// reports it (collab_spawn.ndjson, normalized).
func codexParentMetrics(model string) *RunMetrics {
	return &RunMetrics{Model: model, NumTurns: 1, InputTokens: 15, OutputTokens: 1140, ReasoningTokens: 291, CacheCreationInputTokens: 13618, CacheReadInputTokens: 40590}
}

// codexTokenTotals is the run-level view the tests compare; RunMetrics
// itself carries an atomic and must not be copied.
type codexTokenTotals struct{ input, output, reasoning, cacheWrite, cacheRead int }

func codexTotalsOf(m *RunMetrics) codexTokenTotals {
	return codexTokenTotals{m.InputTokens, m.OutputTokens, m.ReasoningTokens, m.CacheCreationInputTokens, m.CacheReadInputTokens}
}

// assertCodexUsageSumsToTotals is the invariant docs/cli/run.md states for
// per_model_usage: the entries sum to the run totals for the five fields an
// entry has. Reasoning is run-level only and outside it.
func assertCodexUsageSumsToTotals(t *testing.T, m *RunMetrics) {
	t.Helper()
	var sum ModelUsage
	for _, u := range m.PerModelUsage {
		sum.Add(u)
	}
	assert.Equal(t, m.InputTokens, sum.InputTokens, "input")
	assert.Equal(t, m.OutputTokens, sum.OutputTokens, "output")
	assert.Equal(t, m.CacheCreationInputTokens, sum.CacheCreationInputTokens, "cache_creation")
	assert.Equal(t, m.CacheReadInputTokens, sum.CacheReadInputTokens, "cache_read")
	assert.Equal(t, m.TotalCostUSD, sum.CostUSD, "cost (0 on codex)")
}

// TestFoldCodexChildUsage is the arithmetic per fixture: the parent's entry,
// then each child's last cumulative snapshot through codexUsage.counters(),
// added exactly once. Numbers: parent 15/1140/291/13618/40590 (real, paid
// probe); luna child 3/8/0/7378/0 (real); terra child 1000/200/100/2000/1000
// (synthetic); the no-model child 50/5/0/0/0 (synthetic) under unknown; the
// failed child nothing.
func TestFoldCodexChildUsage(t *testing.T) {
	r := CodexRuntime{}
	parentOnly := ModelUsage{Requests: 1, InputTokens: 15, OutputTokens: 1140, CacheCreationInputTokens: 13618, CacheReadInputTokens: 40590}
	lunaBoth := ModelUsage{Requests: 2, InputTokens: 18, OutputTokens: 1148, CacheCreationInputTokens: 20996, CacheReadInputTokens: 40590}
	terra := ModelUsage{Requests: 1, InputTokens: 1000, OutputTokens: 200, CacheCreationInputTokens: 2000, CacheReadInputTokens: 1000}

	for name, tc := range map[string]struct {
		ids    []string
		folded int
		want   map[string]ModelUsage
		totals codexTokenTotals
	}{
		"same model aggregates into one entry": {
			ids: []string{codexChildSameModelID}, folded: 1,
			want:   map[string]ModelUsage{"gpt-5.6-luna": lunaBoth},
			totals: codexTokenTotals{18, 1148, 291, 20996, 40590},
		},
		"different model gets its own entry": {
			ids: []string{codexChildOtherModelID}, folded: 1,
			want:   map[string]ModelUsage{"gpt-5.6-luna": parentOnly, "gpt-5.6-terra": terra},
			totals: codexTokenTotals{1015, 1340, 391, 15618, 41590},
		},
		"failed child contributes nothing, not even a request": {
			ids: []string{codexChildFailedID}, folded: 0,
			want:   map[string]ModelUsage{"gpt-5.6-luna": parentOnly},
			totals: codexTokenTotals{15, 1140, 291, 13618, 40590},
		},
		"a child with usage but no model buckets under unknown": {
			ids: []string{codexChildNoModelID}, folded: 1,
			want:   map[string]ModelUsage{"gpt-5.6-luna": parentOnly, piSubagentUnknownModel: {Requests: 1, InputTokens: 50, OutputTokens: 5}},
			totals: codexTokenTotals{65, 1145, 291, 13618, 40590},
		},
		"all three": {
			ids: []string{codexChildSameModelID, codexChildOtherModelID, codexChildFailedID}, folded: 2,
			want:   map[string]ModelUsage{"gpt-5.6-luna": lunaBoth, "gpt-5.6-terra": terra},
			totals: codexTokenTotals{1018, 1348, 391, 22996, 41590},
		},
	} {
		t.Run(name, func(t *testing.T) {
			sessions := writeCodexChildRollouts(t, codexChildSameModelID, codexChildOtherModelID, codexChildFailedID, codexChildNoModelID)
			fakeOpenshellCodexChildren(t, filepath.Join(t.TempDir(), "openshell.log"), t.TempDir(), "", sessions)

			m := codexParentMetrics("gpt-5.6-luna")
			n, err := r.foldCodexChildUsage("sb", tc.ids, m)
			require.NoError(t, err)
			assert.Equal(t, tc.folded, n, "one per child thread with usage")
			assert.Equal(t, tc.want, m.PerModelUsage)
			assert.Equal(t, tc.totals, codexTotalsOf(m), "child reasoning is in the run total only")
			assert.Zero(t, m.TotalCostUSD, "codex reports no cost; nothing is invented")
			assertCodexUsageSumsToTotals(t, m)
		})
	}
}

// A planted negative counter cannot lower the run's figures: counters()
// floors only its two subtractions, so the fold floors the three raw fields
// (a wrap is TestFoldCodexChildUsage_SkipsImplausibleCounters).
func TestFoldCodexChildUsage_ClampsNegativeCounters(t *testing.T) {
	sessions := writeCodexChildRollouts(t)
	rollout := `{"type":"turn_context","payload":{"model":"gpt-5.6-luna"}}` + "\n" +
		`{"type":"token_usage_record","payload":{"thread_token_usage":{"input_tokens":10,"cached_input_tokens":-5000,"cache_write_input_tokens":-7000,"output_tokens":20,"reasoning_output_tokens":-900,"total_tokens":30}}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(sessions, "2026", "09", "29", "rollout-2026-09-29T15-46-38-"+codexChildSameModelID+".jsonl"), []byte(rollout), 0o644))
	fakeOpenshellCodexChildren(t, filepath.Join(t.TempDir(), "openshell.log"), t.TempDir(), "", sessions)

	m := codexParentMetrics("gpt-5.6-luna")
	n, err := CodexRuntime{}.foldCodexChildUsage("sb", []string{codexChildSameModelID}, m)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	// Input is 10 - (-5000) - (-7000) = 12010 and output 20 - (-900) = 920:
	// inflation is accepted, as on pi; nothing drops below the parent's figures.
	assert.Equal(t, codexTokenTotals{15 + 12010, 1140 + 920, 291, 13618, 40590}, codexTotalsOf(m))
	assert.Equal(t, ModelUsage{Requests: 2, InputTokens: 15 + 12010, OutputTokens: 1140 + 920, CacheCreationInputTokens: 13618, CacheReadInputTokens: 40590},
		m.PerModelUsage["gpt-5.6-luna"])
	assertCodexUsageSumsToTotals(t, m)
}

// A read cut at the bound is a warning; the children before the cut stay
// folded and the ones past it are not counted.
func TestFoldCodexChildUsage_CutReadIsAWarning(t *testing.T) {
	sessions := writeCodexChildRollouts(t, codexChildSameModelID)
	day := filepath.Join(sessions, "2026", "09", "29")
	planted := `{"type":"turn_context","payload":{"model":"` + strings.Repeat("x", codexChildUsageMaxBytes) + `"}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(day, "rollout-2026-09-29T15-46-38-"+codexChildOtherModelID+".jsonl"), []byte(planted), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(day, "rollout-2026-09-29T15-46-38-"+codexChildFailedID+".jsonl"), readCodexFixture(t, "child_usage_other_model.jsonl"), 0o644))
	fakeOpenshellCodexChildren(t, filepath.Join(t.TempDir(), "openshell.log"), t.TempDir(), "", sessions)

	m := codexParentMetrics("gpt-5.6-luna")
	n, err := CodexRuntime{}.foldCodexChildUsage("sb", []string{codexChildSameModelID, codexChildOtherModelID, codexChildFailedID}, m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cut at")
	assert.Equal(t, 1, n, "the child before the cut")
	assert.NotContains(t, m.PerModelUsage, "gpt-5.6-terra", "the child past the cut")
	assert.Equal(t, 2, m.PerModelUsage["gpt-5.6-luna"].Requests)
	assertCodexUsageSumsToTotals(t, m)
}

// A counter no real thread reaches is skipped: two planted rollouts whose
// input sums past MaxInt would otherwise wrap the run's 15 down to 5.
func TestFoldCodexChildUsage_SkipsImplausibleCounters(t *testing.T) {
	sessions := writeCodexChildRollouts(t)
	day := filepath.Join(sessions, "2026", "09", "29")
	for name, input := range map[string]string{"a": "9223372036854775807", "b": "9223372036854775799"} {
		rollout := `{"type":"token_usage_record","payload":{"thread_token_usage":{"input_tokens":` + input + `}}}` + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(day, "rollout-"+name+"-"+codexChildSameModelID+".jsonl"), []byte(rollout), 0o644))
	}
	fakeOpenshellCodexChildren(t, filepath.Join(t.TempDir(), "openshell.log"), t.TempDir(), "", sessions)

	m := codexParentMetrics("gpt-5.6-luna")
	n, err := CodexRuntime{}.foldCodexChildUsage("sb", []string{codexChildSameModelID}, m)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, codexTokenTotals{15, 1140, 291, 13618, 40590}, codexTotalsOf(m))
	assertCodexUsageSumsToTotals(t, m)
}

// No child id, no sandbox command: PATH holds no openshell, so any exec
// would fail to start. The parent's entry still lands in the breakdown
// (a delegating agent that dispatched nothing), and a parent without a
// model is bucketed as pi buckets a record without one.
func TestFoldCodexChildUsage_NoChildrenNoSandbox(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	for _, model := range []string{"gpt-5.6-luna", ""} {
		m := codexParentMetrics(model)
		n, err := CodexRuntime{}.foldCodexChildUsage("sb", nil, m)
		require.NoError(t, err, "no id, no sandbox command")
		assert.Zero(t, n)
		key := model
		if key == "" {
			key = piSubagentUnknownModel
		}
		assert.Equal(t, map[string]ModelUsage{key: {Requests: 1, InputTokens: 15, OutputTokens: 1140, CacheCreationInputTokens: 13618, CacheReadInputTokens: 40590}}, m.PerModelUsage)
		assert.Equal(t, codexTokenTotals{15, 1140, 291, 13618, 40590}, codexTotalsOf(m), "the parent's numbers are not double-counted into the totals")
		assertCodexUsageSumsToTotals(t, m)
	}
}

func TestFoldCodexChildUsage_ReadFailuresKeepTheParentEntry(t *testing.T) {
	r := CodexRuntime{}
	parentOnly := map[string]ModelUsage{"gpt-5.6-luna": {Requests: 1, InputTokens: 15, OutputTokens: 1140, CacheCreationInputTokens: 13618, CacheReadInputTokens: 40590}}

	t.Run("a bad id never reaches the shell", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		m := codexParentMetrics("gpt-5.6-luna")
		n, err := r.foldCodexChildUsage("sb", []string{codexChildSameModelID, "'; rm -rf /; '"}, m)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is not a codex thread id")
		assert.Zero(t, n)
		assert.Equal(t, parentOnly, m.PerModelUsage)
	})

	t.Run("openshell cannot start", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		m := codexParentMetrics("gpt-5.6-luna")
		_, err := r.foldCodexChildUsage("sb", []string{codexChildSameModelID}, m)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reading the child rollouts")
		assert.Equal(t, parentOnly, m.PerModelUsage)
		assert.Equal(t, codexTokenTotals{15, 1140, 291, 13618, 40590}, codexTotalsOf(m), "totals untouched")
	})

	t.Run("the read command exits non-zero", func(t *testing.T) {
		fakeOpenshellCodexChildren(t, filepath.Join(t.TempDir(), "openshell.log"), t.TempDir(), "", t.TempDir())
		t.Setenv("FULLSEND_TEST_CHILD_READ_FAIL", "1")
		m := codexParentMetrics("gpt-5.6-luna")
		_, err := r.foldCodexChildUsage("sb", []string{codexChildSameModelID}, m)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exited 1")
		assert.Contains(t, err.Error(), "refusing the rollout read", "stderr is in the message")
		assert.Equal(t, parentOnly, m.PerModelUsage)
		assertCodexUsageSumsToTotals(t, m)
	})
}

func TestFoldCodexChildUsage_TwoIterationsSumToTotals(t *testing.T) {
	sessions := writeCodexChildRollouts(t, codexChildSameModelID, codexChildOtherModelID)
	fakeOpenshellCodexChildren(t, filepath.Join(t.TempDir(), "openshell.log"), t.TempDir(), "", sessions)
	r := CodexRuntime{}

	first := codexParentMetrics("gpt-5.6-luna")
	n, err := r.foldCodexChildUsage("sb", []string{codexChildSameModelID, codexChildOtherModelID}, first)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	// The retry dispatched nothing; a delegating agent still folds, so its
	// parent entry is in the breakdown.
	second := &RunMetrics{Model: "gpt-5.6-luna", NumTurns: 1, InputTokens: 100, OutputTokens: 50, ReasoningTokens: 7, CacheCreationInputTokens: 20, CacheReadInputTokens: 30}
	n, err = r.foldCodexChildUsage("sb", nil, second)
	require.NoError(t, err)
	assert.Zero(t, n)

	// Summed as internal/cli aggregateRunMetrics sums iterations.
	sum := &RunMetrics{PerModelUsage: map[string]ModelUsage{}}
	for _, m := range []*RunMetrics{first, second} {
		sum.InputTokens += m.InputTokens
		sum.OutputTokens += m.OutputTokens
		sum.ReasoningTokens += m.ReasoningTokens
		sum.CacheCreationInputTokens += m.CacheCreationInputTokens
		sum.CacheReadInputTokens += m.CacheReadInputTokens
		sum.TotalCostUSD += m.TotalCostUSD
		for spec, u := range m.PerModelUsage {
			entry := sum.PerModelUsage[spec]
			entry.Add(u)
			sum.PerModelUsage[spec] = entry
		}
	}
	assert.Equal(t, map[string]ModelUsage{
		"gpt-5.6-luna":  {Requests: 3, InputTokens: 118, OutputTokens: 1198, CacheCreationInputTokens: 21016, CacheReadInputTokens: 40620},
		"gpt-5.6-terra": {Requests: 1, InputTokens: 1000, OutputTokens: 200, CacheCreationInputTokens: 2000, CacheReadInputTokens: 1000},
	}, sum.PerModelUsage, "two parent iterations and one luna child under luna; the terra child alone under terra")
	assert.Equal(t, codexTokenTotals{1118, 1398, 398, 23016, 41620}, codexTotalsOf(sum))
	assertCodexUsageSumsToTotals(t, sum)
}

// runCodexWithChildren runs CodexRuntime.Run against the fake with the given
// stream fixture and a sessions tree holding the rollouts of the three
// children the stream spawns, and returns the metrics, the exit code, the
// printer output and the fake's argv log. delegating sets RoleFiles on the
// runner-held digests (what Bootstrap records for an agent with the Agent
// tool).
func runCodexWithChildren(t *testing.T, streamFixture string, delegating bool) (*RunMetrics, int, string, string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "openshell.log")
	storeDir := t.TempDir()
	r := CodexRuntime{}
	seedCodexManifest(t, storeDir, r, nil)
	if delegating {
		d, ok := lookupRunnerHeldDigests("sb")
		require.True(t, ok)
		d.RoleFiles = map[string]string{"default": "d0d0", "explore": "e0e0"}
		recordRunnerHeldDigests("sb", d)
	}
	sessions := writeCodexChildRollouts(t, codexChildSameModelID, codexChildOtherModelID, codexChildFailedID)
	fakeOpenshellCodexChildren(t, logPath, storeDir, filepath.Join("testdata", "codex", streamFixture), sessions)

	var out bytes.Buffer
	metrics := &RunMetrics{}
	exit, err := r.Run(context.Background(), RunParams{
		SandboxName: "sb",
		RepoDir:     "/sandbox/workspace/repo",
		Model:       "openai/gpt-5.6-luna",
		Timeout:     time.Minute,
	}, ui.New(&out), time.Now(), metrics)
	require.NoError(t, err)
	return metrics, exit, out.String(), readFileString(t, logPath)
}

// A run that spawned: metrics.json carries the children. Digests without
// RoleFiles here, so the spawn in the stream alone is what triggers the read
// (the second half of the gate).
func TestCodexRun_FoldsChildUsage(t *testing.T) {
	metrics, exit, out, log := runCodexWithChildren(t, "collab_spawn.ndjson", false)

	assert.Equal(t, 0, exit)
	assert.Equal(t, "gpt-5.6-luna", metrics.Model)
	assert.Equal(t, codexTokenTotals{1018, 1348, 391, 22996, 41590}, codexTotalsOf(metrics),
		"parent (15/1140/291/13618/40590) plus the luna child (3/8/0/7378/0) plus the terra child (1000/200/100/2000/1000); the failed child nothing")
	assert.Equal(t, map[string]ModelUsage{
		"gpt-5.6-luna":  {Requests: 2, InputTokens: 18, OutputTokens: 1148, CacheCreationInputTokens: 20996, CacheReadInputTokens: 40590},
		"gpt-5.6-terra": {Requests: 1, InputTokens: 1000, OutputTokens: 200, CacheCreationInputTokens: 2000, CacheReadInputTokens: 1000},
	}, metrics.PerModelUsage)
	assertCodexUsageSumsToTotals(t, metrics)
	assert.Zero(t, metrics.TotalCostUSD)
	assert.Equal(t, 1, metrics.NumTurns, "turns stay parent-only")
	assert.Contains(t, out, "2 sub-agent thread(s) folded into the run metrics")
	assert.Equal(t, 1, strings.Count(log, codexChildUsageMarker), "one read command, after the run")
	assert.Contains(t, log, shellQuote(codexChildFailedID), "the failed child's rollout is read too; it is its content that contributes nothing")
	assert.NotContains(t, log, codexChildDeniedID, "the failed spawn's receiver is never looked up")
}

// A delegating agent that dispatched nothing: the breakdown carries the
// parent's entry and no sandbox command runs (the first half of the gate).
func TestCodexRun_DelegatingWithoutSpawnHasParentEntry(t *testing.T) {
	metrics, exit, _, log := runCodexWithChildren(t, "basic_run.ndjson", true)

	assert.Equal(t, 0, exit)
	assert.Equal(t, map[string]ModelUsage{
		"gpt-5.6-luna": {Requests: 1, InputTokens: 12, OutputTokens: 284, CacheCreationInputTokens: 11087, CacheReadInputTokens: 32508},
	}, metrics.PerModelUsage, "the numbers TestApplyCodexMetrics pins for basic_run.ndjson")
	assertCodexUsageSumsToTotals(t, metrics)
	assert.NotContains(t, log, codexChildUsageMarker, "no id, no sandbox command")
}

// The byte-equal invariant: a non-delegating agent whose stream reports no
// spawn writes the same metrics.json as before.
func TestCodexRun_NoSpawnNoBreakdown(t *testing.T) {
	metrics, exit, _, log := runCodexWithChildren(t, "basic_run.ndjson", false)

	assert.Equal(t, 0, exit)
	assert.Nil(t, metrics.PerModelUsage)
	data, err := json.Marshal(metrics)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "per_model_usage", "omitempty keeps the key out")
	assert.NotContains(t, log, codexChildUsageMarker)
}

// A failed read is a warning; the iteration keeps its exit code and the
// parent's numbers, and the breakdown keeps the parent's entry.
func TestCodexRun_ChildReadFailureIsAWarning(t *testing.T) {
	t.Setenv("FULLSEND_TEST_CHILD_READ_FAIL", "1")
	metrics, exit, out, _ := runCodexWithChildren(t, "collab_spawn.ndjson", false)

	assert.Equal(t, 0, exit, "losing the breakdown must not fail an iteration that succeeded")
	assert.Contains(t, out, "! Could not read the sub-agent usage: reading the child rollouts: exited 1: fake openshell: refusing the rollout read")
	assert.NotContains(t, out, "folded into the run metrics")
	assert.Equal(t, codexTokenTotals{15, 1140, 291, 13618, 40590}, codexTotalsOf(metrics), "the parent's tokens only")
	assert.Equal(t, map[string]ModelUsage{
		"gpt-5.6-luna": {Requests: 1, InputTokens: 15, OutputTokens: 1140, CacheCreationInputTokens: 13618, CacheReadInputTokens: 40590},
	}, metrics.PerModelUsage)
	assertCodexUsageSumsToTotals(t, metrics)
}

// A codex process that did not exit 0 (killed at the timeout, cancelled, or
// crashed) reports the parent's tokens only and runs no sandbox command:
// the rollouts of a killed run are not read (docs/cli/run.md).
func TestCodexRun_NonZeroExitSkipsTheChildRead(t *testing.T) {
	t.Setenv("FULLSEND_TEST_CODEX_EXIT", "137")
	metrics, exit, out, log := runCodexWithChildren(t, "collab_spawn.ndjson", true)

	assert.Equal(t, 137, exit)
	assert.Nil(t, metrics.PerModelUsage)
	assert.Equal(t, codexTokenTotals{15, 1140, 291, 13618, 40590}, codexTotalsOf(metrics))
	assert.NotContains(t, log, codexChildUsageMarker)
	assert.NotContains(t, out, "sub-agent")
}
