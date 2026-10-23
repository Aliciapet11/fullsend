@skip:gitlab
Feature: Fix dispatch honours actor roles and the review bot identity

  The bash route in reusable-dispatch.yml decides whether the built-in
  fix stage starts, from who acted. A /fs-fix comment needs a write
  actor: the triage actor (TEST_ACTOR_TRIAGE_PAT) may run /fs-triage and
  /fs-review but not /fs-fix. A changes-requested review starts fix only
  when the reviewer is the registered review bot.

  The first scenario puts the triage actor's refusal and the write
  actor's grant on one pull request. The grant is the positive control:
  it shows a /fs-fix comment on this pull request reaches the fix stage,
  so the refusal cannot pass because fix never loaded. The pull request
  is opened by the write actor, so bash routing reviews it on open; that
  run starts before the comments and the assertions only count runs
  created after each comment.

  The second scenario stands in for a registered review bot requesting
  changes. The suite cannot act as that bot: it holds only the e2e App
  and the human-like actor PATs. The dummy runtime cannot make the real
  review bot request changes either, because the review result needs
  the pull request's head SHA, which a static fixture cannot carry. The
  positive path (review bot requests changes, fix starts) is covered on
  every pull request by the @playback pipeline. Here the suite's e2e
  App, a bot that is not the registered review bot, requests changes on
  a pull request that a registered review bot would send to fix: same
  repository, opened by a write actor, labelled fullsend-fix. GitHub
  refuses REQUEST_CHANGES on a reviewer's own pull request, so the write
  actor opens it rather than the suite's bot. Only the reviewer identity
  then keeps fix from starting. A custom harness triggered by the same
  changes-requested review is the positive control: it shows the review
  event reached dispatch, which still refused the fix stage. It relies
  on bot review events being dispatched without a bot-role lookup
  result (ADR 0107), as GitHub dispatch does today.

  Fix is asserted by its dispatch count, not by the run's result: the
  dummy agent writes a review result, which the fix stage does not
  accept. The count only covers runs that already exist, so after the
  write actor's /fs-fix the "review did not run" assertion runs first:
  its settle window gives that comment's run time to be created.

  Background:
    Given the enrolled test repository

  Scenario: /fs-fix from the triage actor is refused and from the write actor starts fix
    Given a dummy agent that would:
      | description      | op            | args                                                   |
      | Emit review JSON | write_fixture | output/agent-result.json, fixtures/review/comment.json |
    When the write actor opens a pull request
    And the triage actor posts "/fs-fix" on the pull request
    Then the harness "fix" agent did not run
    And the harness "fix" was dispatched exactly 0 times
    When the write actor posts "/fs-fix" on the pull request
    Then the harness "review" agent did not run
    And the harness "fix" was dispatched exactly 1 time

  Scenario: a changes-requested review from a bot other than the review bot does not start fix
    Given a custom harness "review-state-probe" with:
      """
      agent: agents/triage.md
      role: triage
      slug: fullsend-ai-review-state-probe
      model: opus
      image: ghcr.io/fullsend-ai/fullsend-sandbox:latest
      policy: policies/base.yaml
      trigger: |
        event.entity.kind == "change_proposal"
        && event.transition.kind == "review_submitted"
        && event.transition.review.state == "changes_requested"
      """
    And a dummy agent that would:
      | description      | op            | args                                                   |
      | Emit review JSON | write_fixture | output/agent-result.json, fixtures/review/comment.json |
    When the write actor opens a pull request
    And the pull request is labeled "fullsend-fix"
    And a changes-requested review is submitted on the pull request
    Then the harness "review-state-probe" workflow completes successfully
    And the harness "fix" agent did not run
    And the harness "fix" was dispatched exactly 0 times
