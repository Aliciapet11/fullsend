#!/usr/bin/env bash
# count-fix-commits-test.sh — Tests for count-fix-commits.jq
#
# Run from the repo root:
#   bash .github/scripts/count-fix-commits-test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FILTER="${SCRIPT_DIR}/count-fix-commits.jq"
FAILURES=0

# commit AUTHOR MESSAGE_JSON — one commit as the API returns it.
commit() { printf '{"commit":{"author":{"name":"%s"},"message":%s}}' "$1" "$2"; }

# run_test NAME EXPECTED PAGE... — PAGE is a JSON array of commits.
run_test() {
  local name="$1" expected="$2"
  shift 2
  local actual
  actual="$(printf '%s\n' "$@" | jq -s -f "${FILTER}" 2>&1)" || true
  if [[ "${actual}" == "${expected}" ]]; then
    echo "PASS: ${name}"
  else
    echo "FAIL: ${name} — expected ${expected}, got '${actual}'"
    FAILURES=$((FAILURES + 1))
  fi
}

run_test "no-commits" 0 '[]'
run_test "author-name" 1 "[$(commit fullsend-fix '"fix: a"')]"
run_test "trailer-in-last-paragraph" 1 "[$(commit 'app[bot]' '"fix: a\n\nAuthored-by: fullsend-fix <b@x>"')]"
run_test "trailer-with-trailing-whitespace" 1 "[$(commit 'app[bot]' '"fix: a\n\nAuthored-by: fullsend-fix <b@x>\n\n  \n"')]"
run_test "trailer-quoted-in-body" 0 "[$(commit human '"docs: q\n\nAuthored-by: fullsend-fix <b@x>\n\nquoted above."')]"
run_test "code-agent-trailer" 0 "[$(commit 'app[bot]' '"feat: c\n\nAuthored-by: fullsend-code <b@x>"')]"
run_test "empty-message" 0 "[$(commit human '""')]"
run_test "null-message" 0 "[$(commit human 'null')]"
run_test "pages-are-summed" 3 "[$(commit fullsend-fix '"fix: a"'),$(commit human '"x"')]" "[$(commit 'app[bot]' '"fix: b\n\nAuthored-by: fullsend-fix <b@x>"'),$(commit fullsend-fix '""')]"

echo
if [[ "${FAILURES}" -gt 0 ]]; then
  echo "${FAILURES} test(s) failed"
  exit 1
fi
echo "All tests passed"
