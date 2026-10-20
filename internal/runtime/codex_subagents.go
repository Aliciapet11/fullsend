package runtime

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/fullsend-ai/fullsend/internal/sandbox"
)

// Native child accounting for the Codex runtime.
//
// A Codex child is a separate thread. Its tokens never reach the parent's
// `exec --json` stream (turn.completed.usage excludes children — #6970, probe
// of 2026-09-03), and codex records them only in that thread's rollout under
// $CODEX_HOME/sessions/: the running total in `token_usage_record` lines
// (`payload.thread_token_usage`, the stream's five usage fields plus
// total_tokens), the model in the `turn_context` line, and the thread id in
// the file name (rollout-<start time>-<thread id>.jsonl). Run reads those
// lines back with one bounded command after codex exits and folds them into
// RunMetrics, so metrics.json carries the children (#6970, transcripts and
// usage).
//
// The fold covers the fields a per-model entry has: input, output,
// cache-creation and cache-read tokens, and cost, which is 0 on codex. Child
// reasoning tokens go to the run-level ReasoningTokens only, as on pi:
// RunMetrics has no per-model reasoning field.

// codexChildUsageMarker starts the line the read command prints before each
// child's rollout lines, so the parser knows where one child's lines end and
// the next child's begin. A rollout line that imitates it can only
// misattribute that child's own lines.
const codexChildUsageMarker = "fullsend-child"

// codexChildUnreadableMarker starts the line the read command prints for a
// child whose rollout it could not read, or that a failed search of the
// sessions directory did not find. A rollout line that imitates it can only
// add a warning and misattribute that child's own lines.
const codexChildUnreadableMarker = "fullsend-unreadable"

// codexChildUsageMaxBytes bounds the read. The sessions directory is
// agent-writable, so an unbounded read is an unbounded allocation in the
// runner (the reason piSubagentUsageMaxBytes exists). The command selects at
// most two lines per child, so a real read is a few kilobytes.
const codexChildUsageMaxBytes = 1 << 20

// codexChildUsageMaxTokens is far above any counter a real thread reports. A
// child whose record carries a counter past it is skipped, with a warning,
// so under the bounded read the sums of the counters left cannot wrap the
// run's figures. A counter past the int range does not decode, so its record
// counts nothing, as a malformed line does.
const codexChildUsageMaxTokens = 1 << 40

// codexChildUsageThreadID is the shape of a codex thread id (UUID text). A
// child id comes off the stream and goes into a shell command, so nothing
// else is accepted; shellQuote wraps it as well.
var codexChildUsageThreadID = regexp.MustCompile(`^[0-9a-f-]{36}$`)

// codexChildUsage is what one child's rollout reported.
type codexChildUsage struct {
	ID string
	// Model is turn_context.payload.model of the child's last turn, which is
	// its own: a fork copies the parent's turn_context lines ahead of the
	// child's. "" when the rollout has no turn_context.
	Model string
	// Usage is payload.thread_token_usage of the child's last
	// token_usage_record: codex adds every completed response to it, and a
	// context-window overflow does not reset it, so one record is the whole
	// child.
	Usage codexUsage
	// HasUsage is false for a child with no token_usage_record, such as one
	// that errored before its first response (the provider rejected its
	// model in the 2026-09-29 paid probe); it contributes nothing, not even a
	// request.
	HasUsage bool
	// Unreadable is set when the child's rollout failed `test -r`, or a
	// failed search of the sessions directory did not find it; the fold
	// reports it.
	Unreadable bool
}

// codexChildUsageReadCommand renders the one in-sandbox command that reads
// the children's rollouts. For each id, sorted and once, it finds every
// rollout named for the thread (codex writes one), prints a marker line,
// then the rollout's last turn_context line and its last token_usage_record
// line. The record and not the last token_count: codex writes a token_count
// whose usage counters are zero when a request overflows the context window,
// and writes it only after the response's tools finish, so a child cancelled
// in between can lack one for its last response. A rollout that fails
// `test -r`, or one a failed search of the sessions directory did not find
// (a missing directory included), prints codexChildUnreadableMarker in its
// place; a rollout missing from a directory searched without error prints
// nothing, and a path outside the sessions directory (a planted file name
// with a newline in it) is skipped. Two greps rather than a cat: a review
// child's rollout is megabytes and the two lines are all the fold needs, so
// the head -c bound is a safety net, not the usual limit. `command -p`
// bypasses shell functions and uses the system PATH, as the launch guards
// do.
func codexChildUsageReadCommand(sessionsDir string, ids []string) (string, error) {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		if !codexChildUsageThreadID.MatchString(id) {
			return "", fmt.Errorf("child thread id %q is not a codex thread id", id)
		}
		quoted = append(quoted, shellQuote(id))
	}
	return fmt.Sprintf(
		`for id in %s; do files=$(command -p find %s -type f -name "rollout-*-$id.jsonl" 2>/dev/null) || [ -n "$files" ] || printf '%%s %%s\n' %s "$id"; printf '%%s\n' "$files" | while IFS= read -r f; do case $f in %s/*) ;; *) continue ;; esac; if [ ! -r "$f" ]; then printf '%%s %%s\n' %s "$id"; continue; fi; printf '%%s %%s\n' %s "$id"; command -p grep -a -e '"type":"turn_context"' -- "$f" 2>/dev/null | command -p tail -n 1; command -p grep -a -e '"type":"token_usage_record"' -- "$f" 2>/dev/null | command -p tail -n 1; done; done | command -p head -c %d`,
		strings.Join(quoted, " "), shellQuote(sessionsDir), shellQuote(codexChildUnreadableMarker), shellQuote(sessionsDir), shellQuote(codexChildUnreadableMarker), shellQuote(codexChildUsageMarker), codexChildUsageMaxBytes), nil
}

// parseCodexChildUsage decodes the read command's output: a marker line per
// child, then that child's turn_context and token_usage_record lines, or an
// unreadable marker line for a child it could not read. A line before the
// first marker belongs to nobody; a line that is not JSON (the head -c cut,
// or something planted in the agent-writable directory) is skipped.
func parseCodexChildUsage(data []byte) []codexChildUsage {
	var out []codexChildUsage
	marker := []byte(codexChildUsageMarker + " ")
	unreadable := []byte(codexChildUnreadableMarker + " ")
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), maxTranscriptLineSize)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if id, ok := bytes.CutPrefix(line, marker); ok {
			out = append(out, codexChildUsage{ID: string(id)})
			continue
		}
		if id, ok := bytes.CutPrefix(line, unreadable); ok {
			out = append(out, codexChildUsage{ID: string(id), Unreadable: true})
			continue
		}
		if len(out) == 0 {
			continue
		}
		var env struct {
			Type    string `json:"type"`
			Payload struct {
				Model  string     `json:"model"`
				Thread codexUsage `json:"thread_token_usage"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		cur := &out[len(out)-1]
		switch {
		case env.Type == "turn_context":
			cur.Model = strings.TrimSpace(env.Payload.Model)
		case env.Type == "token_usage_record":
			cur.Usage = env.Payload.Thread
			cur.HasUsage = true
		}
	}
	return out
}

// codexChildIDs collects the thread ids of the children the parent spawned.
// It is an io.Writer that Run feeds through a tee of the `exec --json`
// stream: the parser's events carry no thread id (ToolUseEvent is a name and
// a summary) and parseCodexStream's signature is shared with pi's, so the
// ids are read here, one line at a time, from the completed spawn_agent
// items (codexCollabToolCallItem). A spawn that failed made no thread.
//
// A final line with no newline is not scanned; in a codex stream the last
// line is the terminal event, never an item.
type codexChildIDs struct {
	IDs      []string
	line     []byte
	overlong bool
}

func (c *codexChildIDs) Write(p []byte) (int, error) {
	rest := p
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			c.take(rest)
			return len(p), nil
		}
		c.take(rest[:i])
		if !c.overlong {
			c.scan(c.line)
		}
		c.line, c.overlong = c.line[:0], false
		rest = rest[i+1:]
	}
}

// take appends to the pending line, dropping it once it reaches the
// parser's own line bound (streamBufSize): parseCodexStream skips such a
// line too.
func (c *codexChildIDs) take(b []byte) {
	if c.overlong || len(c.line)+len(b) >= streamBufSize {
		c.overlong = true
		c.line = c.line[:0]
		return
	}
	c.line = append(c.line, b...)
}

func (c *codexChildIDs) scan(line []byte) {
	// Cheap pre-filter: almost no line of a stream names the tool.
	if !bytes.Contains(line, []byte("spawn_agent")) {
		return
	}
	var evt struct {
		Type string `json:"type"`
		Item struct {
			codexCollabToolCallItem
			Type string `json:"type"`
		} `json:"item"`
	}
	if json.Unmarshal(line, &evt) != nil || evt.Type != "item.completed" || evt.Item.Type != "collab_tool_call" ||
		evt.Item.Tool != "spawn_agent" || evt.Item.Status != "completed" {
		return
	}
	c.IDs = append(c.IDs, evt.Item.ReceiverThreadIDs...)
}

// foldCodexChildUsage adds the parent's own entry to m.PerModelUsage and,
// when the parent spawned children, reads their rollouts with one bounded
// sandbox command and folds each child's cumulative usage into the totals
// and the breakdown. It returns how many child rollouts it folded (codex
// writes one per thread). An error names what was not counted: a read that
// did not happen or did not finish, a rollout that could not be read, or a
// child skipped for a counter past codexChildUsageMaxTokens; the children it
// did read stay folded. The caller reports it and keeps the run, because
// losing the children's tokens must not fail an iteration that succeeded. No
// sandbox command runs when childIDs is empty.
//
// The parent's entry is added first and always. Iterations are summed
// (internal/cli aggregateRunMetrics), so an iteration that dispatched
// nothing, or whose read failed, still has to be in the breakdown or the
// run's per_model_usage stops summing to its totals across a retry.
//
// Requests: one for the parent iteration, one per child rollout folded (the
// invocation-count semantics of the pi fold). A child with no
// token_usage_record, or whose rollout is missing, recorded nothing and
// contributes nothing. A child whose rollout names no model is bucketed
// under piSubagentUnknownModel, as pi does for a record without one. Child
// reasoning tokens go to the run-level total only. The figures come from an
// agent-writable directory, so every counter is floored at zero, as pi's
// fold clamps its records, and a child with a counter past
// codexChildUsageMaxTokens is skipped and reported; both happen before
// anything is added.
func (r CodexRuntime) foldCodexChildUsage(sandboxName string, childIDs []string, m *RunMetrics) (folded int, err error) {
	per := m.PerModelUsage
	if per == nil {
		per = make(map[string]ModelUsage, len(childIDs)+1)
	}
	add := func(key string, u ModelUsage) {
		entry := per[key]
		entry.Add(u)
		per[key] = entry
	}
	parent := m.Model
	if parent == "" {
		parent = piSubagentUnknownModel
	}
	add(parent, ModelUsage{
		Requests:                 1,
		InputTokens:              m.InputTokens,
		OutputTokens:             m.OutputTokens,
		CacheCreationInputTokens: m.CacheCreationInputTokens,
		CacheReadInputTokens:     m.CacheReadInputTokens,
		CostUSD:                  m.TotalCostUSD,
	})
	m.PerModelUsage = per
	if len(childIDs) == 0 {
		return 0, nil
	}

	cmd, err := codexChildUsageReadCommand(r.codexSessionsDir(), childIDs)
	if err != nil {
		return 0, err
	}
	stdout, stderr, exitCode, err := sandbox.Exec(sandboxName, cmd, 10*time.Second)
	if err != nil {
		return 0, fmt.Errorf("reading the child rollouts: %w", err)
	}
	if exitCode != 0 {
		return 0, fmt.Errorf("reading the child rollouts: exited %d: %s", exitCode, strings.TrimSpace(sanitizeOutput(stderr)))
	}
	unreadable, implausible := 0, 0
	for _, child := range parseCodexChildUsage([]byte(stdout)) {
		if child.Unreadable {
			unreadable++
		}
		if !child.HasUsage {
			continue
		}
		// counters() floors only its two subtractions; the three raw fields
		// are floored here.
		c := child.Usage.counters()
		reasoning, cacheWrite, cacheRead := nonNegative(c.Reasoning), nonNegative(c.CacheWrite), nonNegative(c.CacheRead)
		if max(c.Input, c.Output, reasoning, cacheWrite, cacheRead) > codexChildUsageMaxTokens {
			implausible++
			continue
		}
		// The model comes from an agent-writable rollout and becomes a
		// metrics.json key, so it goes through the shared secret redactor.
		key := redactSummary(child.Model)
		if key == "" {
			key = piSubagentUnknownModel
		}
		add(key, ModelUsage{
			Requests:                 1,
			InputTokens:              c.Input,
			OutputTokens:             c.Output,
			CacheCreationInputTokens: cacheWrite,
			CacheReadInputTokens:     cacheRead,
		})
		m.InputTokens += c.Input
		m.OutputTokens += c.Output
		m.ReasoningTokens += reasoning
		m.CacheCreationInputTokens += cacheWrite
		m.CacheReadInputTokens += cacheRead
		folded++
	}
	var lost []string
	if unreadable > 0 {
		lost = append(lost, fmt.Sprintf("%d child rollout(s) could not be read", unreadable))
	}
	if implausible > 0 {
		lost = append(lost, fmt.Sprintf("%d child(ren) with a counter past %d not counted", implausible, codexChildUsageMaxTokens))
	}
	if len(stdout) >= codexChildUsageMaxBytes {
		lost = append(lost, fmt.Sprintf("cut at %d bytes; only the children before the cut are counted", codexChildUsageMaxBytes))
	}
	if len(lost) > 0 {
		return folded, fmt.Errorf("reading the child rollouts: %s", strings.Join(lost, "; "))
	}
	return folded, nil
}
