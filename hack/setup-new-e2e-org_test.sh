#!/usr/bin/env bash
# setup-new-e2e-org_test.sh — tests for the mint-status team step of
# setup-new-e2e-org.sh (ensure_status_team) against a stubbed gh.
#
# Covers: a missing team is created, its creator removed and the write actor
# added; a second run on a provisioned org makes no changes; a non-404 error
# reading the team or a membership aborts; a membership that stays pending
# after the add fails; an excluded actor on the team fails; no PAT value is
# printed.
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
# ${S}/members/<login>      membership exists; content is its state
# ${S}/team_get_error       GET of the team fails with HTTP 500
# ${S}/member_get_error     GET of any membership fails with HTTP 500
# ${S}/put_state            state a PUT membership produces (default active)
cat > "${SHIM_DIR}/gh" <<'EOF'
#!/usr/bin/env bash
S="${STUB_STATE}"
echo "gh $*" >> "${S}/gh.log"
[[ "${1:-}" == "api" ]] || { echo "unexpected gh $*" >&2; exit 2; }
shift
method=GET path="" jq_filter=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -X) method="$2"; shift 2 ;;
    --jq) jq_filter="$2"; shift 2 ;;
    -f|-F) shift 2 ;;
    --silent) shift ;;
    /*) path="$1"; shift ;;
    *) echo "unexpected arg $1" >&2; exit 2 ;;
  esac
done
notfound() { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
servererr() { echo "gh: Server Error (HTTP 500)" >&2; exit 1; }
out() { if [[ -n "${jq_filter}" ]]; then jq -r "${jq_filter}" <<< "$1"; else echo "$1"; fi; }
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
    echo active > "${S}/members/${STUB_OPERATOR}"
    out "{\"slug\":\"${STUB_TEAM}\"}" ;;
  "GET ${team_path}/memberships/"*)
    [[ -f "${S}/member_get_error" ]] && servererr
    login="${path##*/}"
    [[ -f "${S}/members/${login}" ]] || notfound
    out "{\"state\":\"$(cat "${S}/members/${login}")\"}" ;;
  "PUT ${team_path}/memberships/"*)
    login="${path##*/}"
    if [[ -s "${S}/put_state" ]]; then
      cp "${S}/put_state" "${S}/members/${login}"
    else
      echo active > "${S}/members/${login}"
    fi
    out "{\"state\":\"$(cat "${S}/members/${login}")\"}" ;;
  "DELETE ${team_path}/memberships/"*)
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
  echo active > "${STATE}/members/${WRITE}"
}

# run_step runs ensure_status_team with the stub; sets OUT and RC.
run_step() {
  RC=0
  OUT=$(PATH="${SHIM_DIR}:${PATH}" STUB_STATE="${STATE}" STUB_ORG="${ORG}" \
    STUB_TEAM="${TEAM}" STUB_OPERATOR="${OPERATOR}" \
    TEST_ACTOR_WRITE_PAT="pat-value-must-not-leak" \
    bash -c 'source "$1"; ensure_status_team "$2" "$3" "$4" "$5"' _ \
    "${SETUP}" "${ORG}" "${WRITE}" "${TRIAGE}" "${OUTSIDER}" 2>&1) || RC=$?
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
if [[ "$(cat "${STATE}/members/${WRITE}" 2>/dev/null)" == "active" ]]; then
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
echo pending > "${STATE}/members/${WRITE}"
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
echo pending > "${STATE}/put_state"
run_step
if [[ "${RC}" -ne 0 ]] && grep -q "not active" <<< "${OUT}"; then
  pass "pending after add: fails"
else
  fail "pending after add: rc=${RC}: ${OUT}"
fi

# --- 7. excluded actors on the team: fail ---------------------------------
for actor in "${TRIAGE}" "${OUTSIDER}"; do
  new_state
  provision
  echo active > "${STATE}/members/${actor}"
  run_step
  if [[ "${RC}" -ne 0 ]] && grep -q "${actor} is on ${ORG}/${TEAM}" <<< "${OUT}"; then
    pass "excluded ${actor} on team: fails"
  else
    fail "excluded ${actor} on team: rc=${RC}: ${OUT}"
  fi
done

if [[ "${FAILURES}" -gt 0 ]]; then
  echo "${FAILURES} test(s) failed" >&2
  exit 1
fi
echo "all tests passed"
