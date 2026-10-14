#!/usr/bin/env bash
# setup-new-e2e-org_test.sh — tests for the mint-status team step of
# setup-new-e2e-org.sh (ensure_status_team) against a stubbed gh.
#
# Covers: a missing team is created, its creator removed and the write actor
# added; a second run on a provisioned org makes no changes; a non-404 error
# reading the team, a membership or the member list aborts; a membership that
# stays pending after the add fails; a creator left on the team by a failed
# removal is removed on the retry; a write actor who is a maintainer is set
# back to member; any other member or pending invitee (including an email
# invitation with a null login) on the team fails; no
# PAT value is printed. Also checks that the script no longer references
# the legacy hosted-GCP-mint settings and keeps the
# FULLSEND_FOREIGN_E2E_REPOS step.
#
# Run from the repo root:
#   bash hack/setup-new-e2e-org_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SETUP="${SCRIPT_DIR}/setup-new-e2e-org.sh"
FAILURES=0
ORG="halfsend-01"
TEAM="mint-status"
WRITE="fstest-write"
TRIAGE="fstest-triage"
OUTSIDER="fstest-outsider"
STRANGER="someone-else"
OPERATOR="org-operator"

pass() { echo "ok       $*"; }
fail() {
  echo "FAIL     $*" >&2
  FAILURES=$((FAILURES + 1))
}

SHIM_DIR=$(mktemp -d)
STATE=""
trap 'rm -rf "${SHIM_DIR}" "${STATE}"' EXIT

# --- gh: teams and team memberships kept as files under STUB_STATE --------
# ${S}/team                 team exists
# ${S}/members/<login>      membership exists; content is "STATE ROLE"
# ${S}/team_get_error       GET of the team fails with HTTP 500
# ${S}/member_get_error     GET of any membership fails with HTTP 500
# ${S}/list_error           listing team members fails with HTTP 500
# ${S}/delete_error         DELETE of a membership fails with HTTP 500
# ${S}/email_invitation     the invitations list includes an email invitation
#                           (null login)
# ${S}/put_state            "STATE ROLE" a PUT produces (default: active, role
#                           from the request)
# Pending memberships show up in the invitations list, not the members list.
cat > "${SHIM_DIR}/gh" <<'EOF'
#!/usr/bin/env bash
S="${STUB_STATE}"
echo "gh $*" >> "${S}/gh.log"
[[ "${1:-}" == "api" ]] || { echo "unexpected gh $*" >&2; exit 2; }
shift
method=GET path="" jq_filter="" put_role="member"
while [[ $# -gt 0 ]]; do
  case "$1" in
    -X) method="$2"; shift 2 ;;
    --jq) jq_filter="$2"; shift 2 ;;
    -f|-F)
      case "$2" in role=*) put_role="${2#role=}" ;; esac
      shift 2 ;;
    --silent|--paginate) shift ;;
    /*) path="${1%%\?*}"; shift ;;
    *) echo "unexpected arg $1" >&2; exit 2 ;;
  esac
done
notfound() { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
servererr() { echo "gh: Server Error (HTTP 500)" >&2; exit 1; }
# gh --jq prints a scalar null as an empty line, unlike jq -r ("null").
out() { if [[ -n "${jq_filter}" ]]; then jq -r "${jq_filter} | if . == null then \"\" else . end" <<< "$1"; else echo "$1"; fi; }
# list_logins prints a JSON array of {login} for members in the given state.
list_logins() {
  local want="$1" f st ro
  for f in "${S}"/members/*; do
    [[ -f "${f}" ]] || continue
    read -r st ro < "${f}"
    [[ "${st}" == "${want}" ]] && basename "${f}"
  done | jq -R -s 'split("\n") | map(select(length > 0)) | map({login: .})'
}
team_path="/orgs/${STUB_ORG}/teams/${STUB_TEAM}"
case "${method} ${path}" in
  "GET /user")
    out "{\"login\":\"${STUB_OPERATOR}\"}" ;;
  "GET ${team_path}")
    [[ -f "${S}/team_get_error" ]] && servererr
    [[ -f "${S}/team" ]] || notfound
    out "{\"slug\":\"${STUB_TEAM}\"}" ;;
  "POST /orgs/${STUB_ORG}/teams")
    touch "${S}/team"
    echo "active maintainer" > "${S}/members/${STUB_OPERATOR}"
    out "{\"slug\":\"${STUB_TEAM}\"}" ;;
  "GET ${team_path}/members")
    [[ -f "${S}/list_error" ]] && servererr
    out "$(list_logins active)" ;;
  "GET ${team_path}/invitations")
    inv="$(list_logins pending)"
    # An email invitation has no login: GitHub returns it as null.
    [[ -f "${S}/email_invitation" ]] && inv=$(jq '. + [{login: null}]' <<< "${inv}")
    out "${inv}" ;;
  "GET ${team_path}/memberships/"*)
    [[ -f "${S}/member_get_error" ]] && servererr
    login="${path##*/}"
    [[ -f "${S}/members/${login}" ]] || notfound
    read -r st ro < "${S}/members/${login}"
    out "{\"state\":\"${st}\",\"role\":\"${ro}\"}" ;;
  "PUT ${team_path}/memberships/"*)
    login="${path##*/}"
    if [[ -s "${S}/put_state" ]]; then
      cp "${S}/put_state" "${S}/members/${login}"
    else
      echo "active ${put_role}" > "${S}/members/${login}"
    fi
    read -r st ro < "${S}/members/${login}"
    out "{\"state\":\"${st}\",\"role\":\"${ro}\"}" ;;
  "DELETE ${team_path}/memberships/"*)
    [[ -f "${S}/delete_error" ]] && servererr
    login="${path##*/}"
    [[ -f "${S}/members/${login}" ]] || notfound
    rm -f "${S}/members/${login}" ;;
  *)
    echo "unexpected gh api ${method} ${path}" >&2; exit 2 ;;
esac
EOF
chmod +x "${SHIM_DIR}/gh"

# new_state resets the stub state to an org with no team.
new_state() {
  [[ -n "${STATE}" ]] && rm -rf "${STATE}"
  STATE=$(mktemp -d)
  mkdir -p "${STATE}/members"
  : > "${STATE}/gh.log"
}

# provision marks the team as existing with the write actor active.
provision() {
  touch "${STATE}/team"
  echo "active member" > "${STATE}/members/${WRITE}"
}

# run_step runs ensure_status_team with the stub; sets OUT and RC.
run_step() {
  RC=0
  OUT=$(PATH="${SHIM_DIR}:${PATH}" STUB_STATE="${STATE}" STUB_ORG="${ORG}" \
    STUB_TEAM="${TEAM}" STUB_OPERATOR="${OPERATOR}" \
    TEST_ACTOR_WRITE_PAT="pat-value-must-not-leak" \
    bash -c 'source "$1"; ensure_status_team "$2" "$3"' _ \
    "${SETUP}" "${ORG}" "${WRITE}" 2>&1) || RC=$?
}

# mutations lists the non-GET gh calls made.
mutations() { grep -E -- '-X (POST|PUT|DELETE)' "${STATE}/gh.log" || true; }

# --- 1. missing team: create, drop creator, add write actor ---------------
new_state
run_step
if [[ "${RC}" -ne 0 ]]; then
  fail "fresh org: expected success, got rc=${RC}: ${OUT}"
else
  pass "fresh org: step succeeds"
fi
if grep -q -- "-X POST /orgs/${ORG}/teams" "${STATE}/gh.log"; then
  pass "fresh org: team created"
else
  fail "fresh org: team not created"
fi
if [[ "$(cat "${STATE}/members/${WRITE}" 2>/dev/null)" == "active member" ]]; then
  pass "fresh org: ${WRITE} is an active member"
else
  fail "fresh org: ${WRITE} is not an active member"
fi
if [[ ! -e "${STATE}/members/${OPERATOR}" ]]; then
  pass "fresh org: team creator removed"
else
  fail "fresh org: team creator still on the team"
fi
if grep -q "pat-value-must-not-leak" <<< "${OUT}" \
    || grep -q "pat-value-must-not-leak" "${STATE}/gh.log"; then
  fail "fresh org: PAT value appeared in output"
else
  pass "fresh org: no PAT value in output"
fi

# --- 2. re-run on a provisioned org is a no-op ----------------------------
: > "${STATE}/gh.log"
run_step
if [[ "${RC}" -eq 0 && -z "$(mutations)" ]]; then
  pass "re-run: succeeds with no changes"
else
  fail "re-run: rc=${RC}, changes: $(mutations)"
fi

# --- 3. existing team, pending write actor: membership re-issued ----------
new_state
provision
echo "pending member" > "${STATE}/members/${WRITE}"
run_step
if [[ "${RC}" -eq 0 ]] \
    && grep -q -- "-X PUT /orgs/${ORG}/teams/${TEAM}/memberships/${WRITE}" "${STATE}/gh.log" \
    && ! grep -q -- "-X POST" "${STATE}/gh.log"; then
  pass "pending member: added without recreating the team"
else
  fail "pending member: rc=${RC}: ${OUT}"
fi

# --- 4. team read fails with a non-404 error: abort -----------------------
new_state
touch "${STATE}/team_get_error"
run_step
if [[ "${RC}" -ne 0 && -z "$(mutations)" ]] && grep -q "could not read team" <<< "${OUT}"; then
  pass "team read error: aborts without changes"
else
  fail "team read error: rc=${RC}, changes: $(mutations): ${OUT}"
fi

# --- 5. membership read fails with a non-404 error: abort -----------------
new_state
provision
touch "${STATE}/member_get_error"
run_step
if [[ "${RC}" -ne 0 && -z "$(mutations)" ]]; then
  pass "membership read error: aborts without changes"
else
  fail "membership read error: rc=${RC}, changes: $(mutations): ${OUT}"
fi

# --- 6. membership still pending after the add: fail ----------------------
new_state
touch "${STATE}/team"
echo "pending member" > "${STATE}/put_state"
run_step
if [[ "${RC}" -ne 0 ]] && grep -q "not 'active member'" <<< "${OUT}"; then
  pass "pending after add: fails"
else
  fail "pending after add: rc=${RC}: ${OUT}"
fi

# --- 7. other people on the team: fail ------------------------------------
for actor in "${TRIAGE}" "${OUTSIDER}" "${STRANGER}"; do
  new_state
  provision
  echo "active member" > "${STATE}/members/${actor}"
  run_step
  if [[ "${RC}" -ne 0 ]] && grep -q "${actor} is on ${ORG}/${TEAM}" <<< "${OUT}"; then
    pass "unexpected member ${actor} on team: fails"
  else
    fail "unexpected member ${actor} on team: rc=${RC}: ${OUT}"
  fi
done

# --- 8. pending invitee on the team: fail ---------------------------------
new_state
provision
echo "pending member" > "${STATE}/members/${STRANGER}"
run_step
if [[ "${RC}" -ne 0 ]] && grep -q "${STRANGER} is on ${ORG}/${TEAM}" <<< "${OUT}"; then
  pass "pending invitee on team: fails"
else
  fail "pending invitee on team: rc=${RC}: ${OUT}"
fi

# --- 8b. email invitation (null login) on the team: fail ------------------
new_state
provision
touch "${STATE}/email_invitation"
run_step
if [[ "${RC}" -ne 0 ]] && grep -q "<email-invitation> is on ${ORG}/${TEAM}" <<< "${OUT}"; then
  pass "email invitation on team: fails"
else
  fail "email invitation on team: rc=${RC}: ${OUT}"
fi

# --- 9. creator removal fails after create, then the retry cleans up ------
new_state
touch "${STATE}/delete_error"
run_step
if [[ "${RC}" -ne 0 && -e "${STATE}/members/${OPERATOR}" ]]; then
  pass "creator removal error: step fails, creator still on team"
else
  fail "creator removal error: rc=${RC}: ${OUT}"
fi
rm -f "${STATE}/delete_error"
: > "${STATE}/gh.log"
run_step
if [[ "${RC}" -eq 0 && ! -e "${STATE}/members/${OPERATOR}" ]] \
    && ! grep -q -- "-X POST" "${STATE}/gh.log"; then
  pass "retry: leftover creator removed, step succeeds"
else
  fail "retry: rc=${RC}: ${OUT}"
fi

# --- 10. write actor is a maintainer: set back to member ------------------
new_state
provision
echo "active maintainer" > "${STATE}/members/${WRITE}"
run_step
if [[ "${RC}" -eq 0 \
    && "$(cat "${STATE}/members/${WRITE}")" == "active member" ]] \
    && grep -q -- "-X PUT /orgs/${ORG}/teams/${TEAM}/memberships/${WRITE}" "${STATE}/gh.log"; then
  pass "maintainer write actor: downgraded to member"
else
  fail "maintainer write actor: rc=${RC}: ${OUT}"
fi

# --- 11. maintainer role cannot be downgraded: fail -----------------------
new_state
provision
echo "active maintainer" > "${STATE}/members/${WRITE}"
echo "active maintainer" > "${STATE}/put_state"
run_step
if [[ "${RC}" -ne 0 ]] && grep -q "not 'active member'" <<< "${OUT}"; then
  pass "maintainer after downgrade attempt: fails"
else
  fail "maintainer after downgrade attempt: rc=${RC}: ${OUT}"
fi

# --- 12. member list read fails: abort ------------------------------------
new_state
provision
touch "${STATE}/list_error"
run_step
if [[ "${RC}" -ne 0 ]] && grep -q "could not list members" <<< "${OUT}"; then
  pass "member list error: fails"
else
  fail "member list error: rc=${RC}: ${OUT}"
fi

# --- 13. no legacy hosted-GCP-mint check (#8416) ---------------------------
if grep -nE 'MINT_PROJECT|MINT_FUNCTION|MINT_REGION|gcloud|ROLE_APP_IDS' "${SETUP}"; then
  fail "legacy mint check: script still references hosted-GCP-mint settings"
else
  pass "legacy mint check: no hosted-GCP-mint settings referenced"
fi
if grep -q 'FULLSEND_FOREIGN_E2E_REPOS' "${SETUP}"; then
  pass "legacy mint check: FULLSEND_FOREIGN_E2E_REPOS step kept"
else
  fail "legacy mint check: FULLSEND_FOREIGN_E2E_REPOS step missing"
fi

if [[ "${FAILURES}" -gt 0 ]]; then
  echo "${FAILURES} test(s) failed" >&2
  exit 1
fi
echo "all tests passed"
