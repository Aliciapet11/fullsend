---
title: "144. Reuse poll and entity-bound run processes between work cycles"
status: Accepted
relates_to:
  - agent-architecture
  - agent-infrastructure
topics:
  - agents
  - dispatch
  - entities
  - polling
  - concurrency
---

# 144. Reuse poll and entity-bound run processes between work cycles

Date: 2026-10-20

## Status

Accepted

Partially supersedes [ADR 0106](0106-serialize-agent-runs-and-coalesce-subsequent-events.md):
a follow-up for the same entity may run in the active `fullsend run` process
instead of requiring a separate platform execution. ADR 0106's serialization
key, active-run preservation, and pending-work invariant remain current.

Partially supersedes [ADR 0063](0063-polling-based-work-discovery.md):
`fullsend poll` may remain resident instead of always exiting after one cycle.
Its per-repository scope and driver architecture remain current.

## Context

[ADR 0098](0098-entity-first-harness-evaluation.md) makes the current entity,
with an optional event, the input to harness routing. [ADR 0106](0106-serialize-agent-runs-and-coalesce-subsequent-events.md)
serializes runs for the same harness and entity, but requires a new platform
execution for follow-up work. Repeating dispatch and runner allocation when
another change arrives during or soon after a run delays the visible response.
Cluster platforms can instead deliver a wakeup to an existing process.

Keeping every process alive indefinitely would waste CI time and capacity.
The [infrastructure problem](../problems/agent-infrastructure.md#challenges-in-headless-and-cluster-hosted-runtimes)
also calls out the cost of idle compute. A reusable host process should not
imply a persistent agent sandbox or model session.

## Options

### Always exit after one cycle

This works on every platform, but each follow-up pays dispatch and runner
startup latency.

### Always remain resident

This reduces wakeup latency, but consumes CI job budgets and idle capacity even
when no more work arrives.

### Select a lifecycle from the invocation context

One-shot execution remains available; resident execution reuses the host while
there is a useful opportunity for another work cycle.

## Decision

Adopt three process lifecycles for `fullsend poll` and entity-bound
`fullsend run`: **once** performs one discovery cycle in `poll`, or one entity
eligibility check and at most one sandbox run in `run`, then exits; **poll**
repeats checks and waits for work; **timed** does the same within a hard
deadline and also exits after a period without actionable work. Scheduled
checks and no-op wakeups do not extend the timed idle period. A timed run
starts no sandbox unless its remaining budget can accommodate expected
execution and teardown; otherwise it returns actionable work to the
invocation layer for a later execution. Resident checks use rate-limit-aware
intervals that may favor entities which change more often, while retaining
periodic reconciliation after missed notifications.

A bound `run` accepts an event, entity, or entity ID and resolves it, using
source, kind, and repository context, to one canonical entity. Ambiguous or
unresolvable bindings fail rather than falling back to unbound execution. A
bound run re-reads current entity state; the supplied input only establishes
the binding. With no binding input, `run` is unbound and once, primarily for
local use, and cannot select a resident lifecycle. An event-only harness can
bind to the entity resolved from its event, but cannot become actionable from
scheduled checks without another verified event.

An explicit lifecycle choice takes precedence for `poll` and bound `run`.
Otherwise, they default to timed when running in CI with a usable job
deadline, once in CI without one, and poll outside CI. The job deadline is
distinct from an individual sandbox timeout. ADR 0063's reserved `fullsend
poll --watch` denotes the explicit poll lifecycle; the lifecycle selection
must remain distinct from the existing `fullsend poll --mode` input-discovery
setting. Each bound `run` cycle rechecks current state, applicable
authorization, and harness eligibility before creating a fresh sandbox, even
when `poll` supplied a matching candidate. An earlier decision or listener
authentication never carries authority into a later cycle. The resident
process never runs two sandboxes concurrently. Harnesses without entity
sources remain event-triggered and wait for verified event-bearing
notifications rather than becoming actionable from eventless checks.

In poll and timed lifecycles, both commands expose a private, authenticated
host-side HTTP listener that accepts an optional event or an eventless wakeup.
A bound `run` rejects notifications outside its harness, repository, and
canonical entity. A repository-scoped `poll` rejects other repositories and
routes accepted events to matching harnesses. For event-bearing delivery to
`run`, the invocation path verifies provenance, normalizes and authorizes the
event, and applies harness routing; `poll` does the same before forwarding
an incoming event. The newest accepted event is retained per serialization
key until the next check. `poll` must not coalesce events for different keys.
Eventless wakeups may coalesce into one prompt check but cannot replace a
retained event. A notification during a poll cycle or sandbox schedules an
immediate follow-up for its key. Neither listener authentication nor an
eventless wakeup grants authority to run a sandbox. Where networking allows,
`poll` may forward the event to an active bound `run` through the invocation
layer. The sandbox and agent runtime cannot control these listeners or the
host loop.

The invocation layer owns the [ADR 0106](0106-serialize-agent-runs-and-coalesce-subsequent-events.md)
scope of harness, target repository, and canonical entity. It atomically wakes
the one active bound process or starts a new one, with a durable pending path
that preserves work across endpoint lookup, delivery, and process-exit races.
HTTP is a low-latency delivery path, not the ownership record. A resident
`run` reports each completed batch when it becomes idle; a timed CI job may
present several sandbox invocations as one job-level result while retaining
per-invocation attribution.

## Consequences

- A warm bound process can respond to later work without reallocating a runner,
  while each actionable attempt still receives a fresh sandbox.
- Timed processes can reuse a CI job briefly and release it on idle or before
  its deadline instead of occupying the full allotted time.
- Invocation backends need a durable wake-or-start path in addition to optional
  direct HTTP delivery.
- The current one-shot CLI and platform workflows need lifecycle, event
  delivery, and result-reporting changes; entity-query and CEL data acquisition
  strategies remain separate decisions.
