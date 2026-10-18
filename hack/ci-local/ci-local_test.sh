#!/usr/bin/env bash
# ci-local_test.sh — Tests for hack/ci-local/ci-local against a stub container
# engine: target validation, fail-closed targets, engine argv (isolation,
# user mapping, SELinux labels), git worktree mounts, and reading tool versions
# from the checkout's go.mod and lint.yml (#8479).
#
# Run from the repo root:
#   bash hack/ci-local/ci-local_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CI_LOCAL="${SCRIPT_DIR}/ci-local"
FAILURES=0

pass() { echo "ok       $*"; }
fail() {
  echo "FAIL     $*" >&2
  FAILURES=$((FAILURES + 1))
}

WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT
export HOME="${WORK}/home" TMPDIR="${WORK}/tmp"
mkdir -p "${HOME}" "${TMPDIR}"
# A host secret that must never reach the engine's argv.
export CI_LOCAL_TEST_SECRET="s3cr3t-must-not-leak"
unset CI_LOCAL_ENGINE CI_LOCAL_DNS

ENGINE_LOG="${WORK}/engine.log"
export ENGINE_LOG

# Stub engine: logs each call (one arg per line after a CALL marker).
# `image inspect` exits STUB_IMAGE_RC (non-zero = image missing, forcing a
# build). `run` executes the container command on the host when STUB_EXEC=1,
# so the target logic itself is tested; otherwise it exits with STUB_RC.
write_engine() {
  local dir="${WORK}/bin-$1"
  mkdir -p "${dir}"
  cat > "${dir}/$1" <<'STUB'
#!/usr/bin/env bash
{ echo CALL; printf 'ARG %s\n' "$@"; } >> "${ENGINE_LOG}"
case "$1" in
  image) exit "${STUB_IMAGE_RC:-0}" ;;
  info) echo "${STUB_OS:-}"; exit 0 ;;
  # clean: STUB_IMAGES/STUB_VOLUMES are the listings; STUB_BUSY is refused as in use.
  # STUB_LIST_RC fails both listings, as a stopped engine does.
  images) printf '%s\n' "${STUB_IMAGES:-}"; exit "${STUB_LIST_RC:-0}" ;;
  rmi) [ "$2" != "${STUB_BUSY:-}" ]; exit ;;
  volume)
    [ "$2" = ls ] && { printf '%s\n' "${STUB_VOLUMES:-}"; exit "${STUB_LIST_RC:-0}"; }
    [ "$2" != rm ] || [ "$3" != "${STUB_BUSY:-}" ]; exit ;;
  build) exit 0 ;;
  run)
    # Docker's root volume helper (--user 0) exits STUB_HELPER_RC.
    for a in "$@"; do [ "$a" = 0 ] && [ "${prev:-}" = --user ] && exit "${STUB_HELPER_RC:-0}"; prev=$a; done
    [ "${STUB_EXEC:-0}" = 1 ] && exec bash -c "${*: -1}"
    exit "${STUB_RC:-0}" ;;
esac
exit 0
STUB
  chmod +x "${dir}/$1"
  echo "${dir}/$1"
}
PODMAN=$(write_engine podman)
DOCKER=$(write_engine docker)

# Shims for tools the container command calls when STUB_EXEC=1.
SHIMS="${WORK}/shims"
mkdir -p "${SHIMS}"

# Stub scutil: prints STUB_SCUTIL_OUT (empty = no resolvers), so DNS argv
# never depends on the machine running the suite. On PATH everywhere, so
# Linux runs take the macOS parsing path too.
mkdir -p "${WORK}/bin-scutil"
cat > "${WORK}/bin-scutil/scutil" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "${STUB_SCUTIL_OUT:-}"
STUB
chmod +x "${WORK}/bin-scutil/scutil"
export PATH="${WORK}/bin-scutil:${PATH}"

# Args of the last `run` call that is not Docker's root volume chmod.
last_run_args() {
  awk '/^CALL$/{n++} {a[n]=a[n] $0 "\n"} END{for(i=n;i>0;i--) if (a[i] ~ /\nARG run\n/ && a[i] !~ /ARG --user\nARG 0\n/) {printf "%s", a[i]; exit}}' "${ENGINE_LOG}" |
    sed -n 's/^ARG //p'
}

# Args of the last `build` call.
last_build_args() {
  awk '/^CALL$/{n++} {a[n]=a[n] $0 "\n"} END{for(i=n;i>0;i--) if (a[i] ~ /\nARG build\n/) {printf "%s", a[i]; exit}}' "${ENGINE_LOG}" |
    sed -n 's/^ARG //p'
}

# Test checkouts carry the real CI config, so a lint.yml change that ci-local
# can no longer parse fails this suite in the same PR.
ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
add_ci_config() {
  mkdir -p "$1/.github/workflows"
  cp "${ROOT}/.github/workflows/lint.yml" "$1/.github/workflows/lint.yml"
  cp "${ROOT}/go.mod" "$1/go.mod"
}

REPO="${WORK}/repo"
mkdir -p "${REPO}/.git"
add_ci_config "${REPO}"

run_ci_local() { # engine, then ci-local args; sets RC and OUT
  local engine=$1
  shift
  : > "${ENGINE_LOG}"
  set +e
  OUT=$(CI_LOCAL_ENGINE="${engine}" "${CI_LOCAL}" -C "${REPO}" "$@" 2>&1)
  RC=$?
  set -e
}

# --- target validation ---
STUB_IMAGE_RC=1 run_ci_local "${PODMAN}" no-such-target
if [ "${RC}" -eq 2 ] && ! grep -qxE 'ARG (run|build)' "${ENGINE_LOG}"; then
  pass "unknown target exits 2 without building or starting a container"
else
  fail "unknown target: rc=${RC}, engine log: $(tr '\n' ' ' < "${ENGINE_LOG}")"
fi

run_ci_local "${PODMAN}" go-vet no-such-target
if [ "${RC}" -eq 2 ] && ! grep -qx 'ARG run' "${ENGINE_LOG}"; then
  pass "an unknown target anywhere in the list stops before any target runs"
else
  fail "mixed valid/unknown targets: rc=${RC}"
fi

# build-image, shell and clean must run alone: extra targets were silently dropped.
bad_alone=""
for args in "build-image go-test" "build-image no-such-target" "go-test clean" "shell lint" "clean clean"; do
  # shellcheck disable=SC2086 # split the case into arguments
  STUB_IMAGE_RC=1 run_ci_local "${PODMAN}" ${args}
  if [ "${RC}" -ne 2 ] || grep -qxE 'ARG (run|build|images|volume)' "${ENGINE_LOG}" ||
    ! grep -q 'must be the only target' <<< "${OUT}"; then
    bad_alone="${bad_alone} [${args}: rc=${RC}]"
  fi
done
if [ -z "${bad_alone}" ]; then
  pass "build-image/shell/clean with other targets exit 2 before touching the engine"
else
  fail "maintenance targets not alone:${bad_alone}"
fi

# Targets that look like echo options must still be rejected, not swallowed.
bad_dash=""
for a in -n -e -E; do
  STUB_IMAGE_RC=1 run_ci_local "${PODMAN}" "${a}"
  if [ "${RC}" -ne 2 ] || grep -qxE 'ARG (run|build)' "${ENGINE_LOG}"; then bad_dash="${bad_dash} [${a}: rc=${RC}]"; fi
done
if [ -z "${bad_dash}" ]; then
  pass "-n/-e/-E are rejected as unknown targets (exit 2, no build or run)"
else
  fail "echo-option-shaped targets:${bad_dash}"
fi

# --- versions come from the checkout's CI config ---
STUB_IMAGE_RC=1 run_ci_local "${PODMAN}" go-vet
BUILD=$(last_build_args)
arg() { grep -A1 -x -- '--build-arg' <<< "${BUILD}" | sed -n "s/^$1=//p"; }
want_go=$(awk '$1 == "go" {print $2; exit}' "${REPO}/go.mod")
missing=""
for v in GO_VERSION PYTHON_VERSION NODE_VERSION PIP_PACKAGES LYCHEE_VERSION LYCHEE_SHA256_AMD64 PINACT_VERSION PINACT_SHA256_AMD64; do
  [ -n "$(arg "$v")" ] || missing="${missing} $v"
done
if [ -z "${missing}" ] && [ "$(arg GO_VERSION)" = "${want_go}" ] &&
  grep -q "python-version: \"$(arg PYTHON_VERSION)\"" "${REPO}/.github/workflows/lint.yml" &&
  grep -q "$(arg LYCHEE_SHA256_AMD64)  /tmp/lychee.tar.gz" "${REPO}/.github/workflows/lint.yml" &&
  [[ "$(arg PINACT_SHA256_AMD64)" =~ ^[0-9a-f]{64}$ ]]; then
  pass "all build args are read from the real go.mod and lint.yml"
else
  fail "build args from CI config: missing=[${missing}] args=$(tr '\n' ' ' <<< "${BUILD}")"
fi
TAG1=$(grep -A1 -x -- '-t' <<< "${BUILD}" | tail -1)

case "$(uname -m)" in arm64 | aarch64) native=linux/arm64 ;; *) native=linux/amd64 ;; esac
if grep -A1 -x -- '--platform' <<< "${BUILD}" | grep -qx "${native}" &&
  grep -A1 -x -- '--platform' <<< "$(last_run_args)" | grep -qx "${native}" &&
  [[ "${TAG1}" == fullsend-ci-local:* ]]; then
  pass "a native build and run name the host platform (${native}) explicitly"
else
  fail "native platform not passed: build=$(tr '\n' ' ' <<< "${BUILD}")"
fi

sed -i.bak -E 's/(python-version: )"[0-9.]+"/\1"3.99"/' "${REPO}/.github/workflows/lint.yml"
STUB_IMAGE_RC=1 run_ci_local "${PODMAN}" go-vet
TAG2=$(grep -A1 -x -- '-t' <<< "$(last_build_args)" | tail -1)
if [ -n "${TAG1}" ] && [ "${TAG1}" != "${TAG2}" ] && grep -qx 'PYTHON_VERSION=3.99' <<< "$(last_build_args)"; then
  pass "a version bump in lint.yml changes the image tag, forcing a rebuild"
else
  fail "image tag did not follow the version bump: ${TAG1} -> ${TAG2}"
fi

sed -i.bak -E '/python-version:/d' "${REPO}/.github/workflows/lint.yml"
STUB_IMAGE_RC=1 run_ci_local "${PODMAN}" go-vet
if [ "${RC}" -ne 0 ] && grep -q 'cannot read PYTHON_VERSION from lint.yml' <<< "${OUT}" &&
  ! grep -qxE 'ARG (run|build)' "${ENGINE_LOG}"; then
  pass "an unreadable version fails before any build or run"
else
  fail "missing python-version must fail closed: rc=${RC} out=${OUT}"
fi

rm "${REPO}/.github/workflows/lint.yml"
run_ci_local "${PODMAN}" go-vet
if [ "${RC}" -ne 0 ] && grep -q 'missing .*lint.yml' <<< "${OUT}"; then
  pass "a checkout without lint.yml fails closed"
else
  fail "missing lint.yml must fail: rc=${RC} out=${OUT}"
fi
add_ci_config "${REPO}"

run_ci_local "${PODMAN}" build-image
if grep -qx -- '--no-cache' <<< "$(last_build_args)"; then
  pass "build-image rebuilds with --no-cache to refresh floating versions"
else
  fail "build-image did not pass --no-cache"
fi

# Hosts that share an image store must not reuse each other's native image.
tag_for_arch() { # uname -m value -> image tag
  printf '#!/bin/sh\necho %s\n' "$1" > "${SHIMS}/uname"
  chmod +x "${SHIMS}/uname"
  PATH="${SHIMS}:${PATH}" STUB_IMAGE_RC=1 run_ci_local "${PODMAN}" go-vet
  rm -f "${SHIMS}/uname"
  grep -A1 -x -- '-t' <<< "$(last_build_args)" | tail -1
}
ARM_TAG=$(tag_for_arch aarch64)
X86_TAG=$(tag_for_arch x86_64)
if [ -n "${ARM_TAG}" ] && [ "${ARM_TAG}" != "${X86_TAG}" ]; then
  pass "native arm64 and amd64 builds get different image tags"
else
  fail "native arches share an image tag: ${ARM_TAG} vs ${X86_TAG}"
fi

# The fetch/sumof helpers are copied into the image, so changing one must
# change the tag. Run a copy of hack/ci-local so the real files stay untouched.
COPY_DIR="${WORK}/ci-local-copy"
mkdir -p "${COPY_DIR}"
cp "${SCRIPT_DIR}/ci-local" "${SCRIPT_DIR}/Containerfile" "${SCRIPT_DIR}/fetch" "${SCRIPT_DIR}/sumof" "${COPY_DIR}/"
copy_tag() {
  : > "${ENGINE_LOG}"
  CI_LOCAL_ENGINE="${PODMAN}" STUB_IMAGE_RC=1 "${COPY_DIR}/ci-local" -C "${REPO}" go-vet >/dev/null 2>&1 || true
  grep -A1 -x -- '-t' <<< "$(last_build_args)" | tail -1
}
T_BEFORE=$(copy_tag)
echo "# changed" >> "${COPY_DIR}/sumof"
T_SUMOF=$(copy_tag)
echo "# changed" >> "${COPY_DIR}/fetch"
T_FETCH=$(copy_tag)
if [ -n "${T_BEFORE}" ] && [ "${T_BEFORE}" != "${T_SUMOF}" ] && [ "${T_SUMOF}" != "${T_FETCH}" ]; then
  pass "changing fetch or sumof changes the image tag, forcing a rebuild"
else
  fail "helper change did not change the tag: ${T_BEFORE} / ${T_SUMOF} / ${T_FETCH}"
fi

# --- sumof: checksum-file formats (local file:// URLs, no network) ---
SUMS="${WORK}/sums"
mkdir -p "${SUMS}"
printf '%s  other.tar.gz\n%s  tool-linux.tar.gz\n' "$(printf 'a%.0s' {1..64})" "$(printf 'b%.0s' {1..64})" > "${SUMS}/two-space.txt"
printf '%s *tool-linux.tar.gz\n' "$(printf 'c%.0s' {1..64})" > "${SUMS}/binary-mode.txt"
printf '%s\n' "$(printf 'd%.0s' {1..64})" > "${SUMS}/lone.sha256"
sumof_out() { "${SCRIPT_DIR}/sumof" "file://${SUMS}/$1" "$2" 2> /dev/null; }
if [ "$(sumof_out two-space.txt tool-linux.tar.gz)" = "$(printf 'b%.0s' {1..64})" ] &&
  [ "$(sumof_out binary-mode.txt tool-linux.tar.gz)" = "$(printf 'c%.0s' {1..64})" ] &&
  [ "$(sumof_out lone.sha256 tool-linux.tar.gz)" = "$(printf 'd%.0s' {1..64})" ]; then
  pass "sumof reads '<sum>  <name>', '<sum> *<name>' and lone-'<sum>' checksum files"
else
  fail "sumof formats: two-space=$(sumof_out two-space.txt tool-linux.tar.gz) binary=$(sumof_out binary-mode.txt tool-linux.tar.gz) lone=$(sumof_out lone.sha256 tool-linux.tar.gz)"
fi
set +e
"${SCRIPT_DIR}/sumof" "file://${SUMS}/two-space.txt" missing.tar.gz > /dev/null 2>&1; rc_missing=$?
"${SCRIPT_DIR}/sumof" "file://${SUMS}/two-space.txt" > /dev/null 2>&1; rc_usage=$?
set -e
if [ "${rc_missing}" -ne 0 ] && [ "${rc_usage}" -eq 2 ]; then
  pass "sumof fails for an unlisted name and on wrong usage"
else
  fail "sumof must fail: missing-name rc=${rc_missing}, usage rc=${rc_usage}"
fi

# --- fail-closed targets (command executed on the host via the stub) ---
cat > "${SHIMS}/git" <<'SHIM'
#!/usr/bin/env bash
case "$1" in
  config) exit 0 ;;
  rev-list) echo "fatal: bad revision 'origin/main..HEAD'" >&2; exit 128 ;;
esac
exit 0
SHIM
chmod +x "${SHIMS}/git"
PATH="${SHIMS}:${PATH}" STUB_EXEC=1 run_ci_local "${PODMAN}" commit-lint
if [ "${RC}" -ne 0 ] && grep -q 'commit-lint *FAIL' <<< "${OUT}"; then
  pass "commit-lint fails when git rev-list fails (missing origin/main)"
else
  fail "commit-lint with failing rev-list must FAIL: rc=${RC} out=${OUT}"
fi

cat > "${SHIMS}/make" <<'SHIM'
#!/usr/bin/env bash
echo "suite a ok"
echo "make: [Makefile:1: script-test] Error 1 (ignored)"
echo "suite b: 2 cases (ignored) by filter"
echo "suite c ok"
echo "make: [Makefile:3: script-test] Error 1 (ignored)"
echo "make[1]: [sub.mk:9: inner] Error 2 (ignored)"
exit 0
SHIM
chmod +x "${SHIMS}/make"
PATH="${SHIMS}:${PATH}" STUB_EXEC=1 run_ci_local "${PODMAN}" script-test
if [ "${RC}" -ne 0 ] && grep -q 'suite c ok' <<< "${OUT}" &&
  grep -q 'script-test: 3 suite(s) failed' <<< "${OUT}"; then
  pass "script-test runs every suite and counts only make's (ignored) error lines"
else
  fail "script-test failure reporting: rc=${RC} out=${OUT}"
fi

printf '#!/usr/bin/env bash\necho "suite a ok"\nexit 0\n' > "${SHIMS}/make"
PATH="${SHIMS}:${PATH}" STUB_EXEC=1 run_ci_local "${PODMAN}" script-test
if [ "${RC}" -eq 0 ] && grep -q 'script-test: 0 suite(s) failed' <<< "${OUT}"; then
  pass "script-test passes when no suite fails"
else
  fail "script-test clean run: rc=${RC} out=${OUT}"
fi

printf '#!/usr/bin/env bash\necho "make: crashed" >&2\nexit 2\n' > "${SHIMS}/make"
PATH="${SHIMS}:${PATH}" STUB_EXEC=1 run_ci_local "${PODMAN}" script-test
if [ "${RC}" -ne 0 ] && grep -q 'script-test: make did not run' <<< "${OUT}"; then
  pass "script-test fails when make itself cannot run"
else
  fail "script-test with broken make must FAIL: rc=${RC} out=${OUT}"
fi

printf '#!/usr/bin/env bash\necho "main.go:3:1: expected declaration" >&2\nexit 2\n' > "${SHIMS}/gofmt"
chmod +x "${SHIMS}/gofmt"
PATH="${SHIMS}:${PATH}" STUB_EXEC=1 run_ci_local "${PODMAN}" go-fmt
if [ "${RC}" -ne 0 ] && grep -q 'gofmt: failed to run' <<< "${OUT}" && ! grep -q 'gofmt: clean' <<< "${OUT}"; then
  pass "go-fmt fails when gofmt itself fails (no stdout), not 'clean'"
else
  fail "go-fmt with failing gofmt must FAIL: rc=${RC} out=${OUT}"
fi

# coverage: make go-test succeeds, but the base-branch diff fails.
printf '#!/usr/bin/env bash\nexit 0\n' > "${SHIMS}/make"
cat > "${SHIMS}/git" <<'SHIM'
#!/usr/bin/env bash
case "$1" in
  config) exit 0 ;;
  diff) echo "fatal: bad revision 'origin/main...HEAD'" >&2; exit 128 ;;
esac
exit 0
SHIM
PATH="${SHIMS}:${PATH}" STUB_EXEC=1 run_ci_local "${PODMAN}" coverage
if [ "${RC}" -ne 0 ] && grep -q 'coverage *FAIL' <<< "${OUT}"; then
  pass "coverage fails when the origin/main diff fails"
else
  fail "coverage without origin/main must FAIL: rc=${RC} out=${OUT}"
fi
rm -f "${SHIMS}/git" "${SHIMS}/make" "${SHIMS}/gofmt"

# --- engine argv: isolation ---
run_ci_local "${PODMAN}" go-vet
ARGS=$(last_run_args)
envs=$(grep -A1 -x -- '-e' <<< "${ARGS}" | grep -vx -- '-e' | grep -vx -- '--' || true)
if [ "${envs}" = "HOME=/home/runner" ]; then
  pass "only HOME is passed into the container environment"
else
  fail "unexpected -e values: ${envs}"
fi
if ! grep -qE -- '--env-host|--env-file|--env-merge' <<< "${ARGS}" && ! grep -q "${CI_LOCAL_TEST_SECRET}" "${ENGINE_LOG}"; then
  pass "host environment is not forwarded (no --env-host/--env-file, secret absent)"
else
  fail "host environment leaked into engine argv"
fi
mounts=$(grep -A1 -x -- '-v' <<< "${ARGS}" | grep -vx -- '-v' | grep -vx -- '--' || true)
bad=$(grep -vE "^(${REPO}:/src|fullsend-ci-local-[a-z-]+:/home/runner/[^ ]+|fullsend-ci-local-nm-[a-z]+-[0-9a-f]{12}:/src/.*node_modules)$" <<< "${mounts}" || true)
if [ -z "${bad}" ] && ! grep -qE '\.ssh|\.gitconfig|SSH_AUTH_SOCK|docker\.sock|\.config/gh|\.aws|gcloud' <<< "${ARGS}"; then
  pass "mounts are only the checkout and named volumes; no credentials"
else
  fail "unexpected mounts: ${bad}"
fi
if grep -qx -- '--http-proxy=false' <<< "${ARGS}"; then
  pass "podman runs with --http-proxy=false (host proxy vars not copied in)"
else
  fail "podman run is missing --http-proxy=false"
fi
CMD=$(tail -1 <<< "${ARGS}")
if grep -q 'safe.directory /src;' <<< "${CMD}" && grep -q 'safe.directory /gitcommon;' <<< "${CMD}" &&
  ! grep -q 'safe.directory "\*"' <<< "${CMD}"; then
  pass "safe.directory trusts only /src and /gitcommon, not every repo"
else
  fail "safe.directory scope: ${CMD}"
fi
run_ci_local "${PODMAN}" docs
DOCS_CMD=$(tail -1 <<< "$(last_run_args)")
if grep -q 'VPL_MVB_BRANCH="$sha"' <<< "${DOCS_CMD}" && grep -q 'GITHUB_ACTIONS=true' <<< "${DOCS_CMD}" &&
  grep -q 'git submodule update --init; GITHUB_ACTIONS=true' <<< "${DOCS_CMD}" &&
  grep -q 'git submodule update --init' "${ROOT}/.github/workflows/site-build.yml"; then
  pass "docs initializes submodules and passes VPL_MVB_BRANCH/GITHUB_ACTIONS like site-build.yml"
else
  fail "docs target diverges from site-build.yml (submodules, VPL_MVB_BRANCH, GITHUB_ACTIONS)"
fi

# N2: node_modules volumes are per checkout; download caches are shared.
nm_vols() { grep -A1 -x -- '-v' <<< "$(last_run_args)" | grep -E '^fullsend-ci-local-nm-' | sort; }
cache_vols() { grep -A1 -x -- '-v' <<< "$(last_run_args)" | grep -E '^fullsend-ci-local-(go|gobuild|precommit|uv|npm):' | sort; }
OTHER="${WORK}/other-checkout"
mkdir -p "${OTHER}/.git"
add_ci_config "${OTHER}"
run_ci_local "${PODMAN}" go-vet; NM_A=$(nm_vols); CACHE_A=$(cache_vols)
run_ci_local "${PODMAN}" go-vet; NM_A2=$(nm_vols)
SAVED_REPO="${REPO}"; REPO="${OTHER}"
run_ci_local "${PODMAN}" go-vet; NM_B=$(nm_vols); CACHE_B=$(cache_vols)
REPO="${SAVED_REPO}"
if [ "$(wc -l <<< "${NM_A}")" -eq 2 ] && [ "${NM_A}" = "${NM_A2}" ] && [ "${NM_A}" != "${NM_B}" ] &&
  [ -n "${CACHE_A}" ] && [ "${CACHE_A}" = "${CACHE_B}" ]; then
  pass "node_modules volumes are per checkout (stable per path); download caches are shared"
else
  fail "volume scoping: A=[${NM_A}] A2=[${NM_A2}] B=[${NM_B}] cacheA=[${CACHE_A}] cacheB=[${CACHE_B}]"
fi

# N3: podman builds also run with --http-proxy=false; docker builds don't get the podman-only flag.
STUB_IMAGE_RC=1 run_ci_local "${PODMAN}" go-vet
PB=$(last_build_args)
STUB_IMAGE_RC=1 run_ci_local "${DOCKER}" go-vet
DB=$(last_build_args)
if grep -qx -- '--http-proxy=false' <<< "${PB}" && ! grep -q -- '--http-proxy' <<< "${DB}" && [ -n "${DB}" ]; then
  pass "podman builds run with --http-proxy=false; docker builds get no podman-only flag"
else
  fail "build proxy flag: podman=[$(tr '\n' ' ' <<< "${PB}")] docker=[$(tr '\n' ' ' <<< "${DB}")]"
fi
# Docker: every proxy variable is set empty on the build, the volume chmod run
# and the main run, so ~/.docker/config.json proxies never apply. Podman
# (--http-proxy=false) gets none of them.
PROXY_VARS="HTTP_PROXY= http_proxy= HTTPS_PROXY= https_proxy= FTP_PROXY= ftp_proxy= NO_PROXY= no_proxy= ALL_PROXY= all_proxy="
proxy_vals() { grep -A1 -x -- "$1" | grep -vx -- "$1" | grep -vx -- '--' | grep -i proxy | tr '\n' ' ' | sed 's/ $//' || true; }
run_ci_local "${DOCKER}" go-vet
RUN_PROXY=$(last_run_args | proxy_vals -e)
PER_RUN=$(sed -n 's/^ARG //p' "${ENGINE_LOG}" | { grep -ix '[a-z_]*proxy=' || true; } | sort | uniq -c | awk '{print $1}' | sort -u)
if [ "$(proxy_vals --build-arg <<< "${DB}")" = "${PROXY_VARS}" ] && [ "${RUN_PROXY}" = "${PROXY_VARS}" ] &&
  [ "${PER_RUN}" = 2 ] && [ -z "$(proxy_vals --build-arg <<< "${PB}")" ] &&
  ! grep -qiE 'ARG [a-z_]*proxy=.' "${ENGINE_LOG}"; then
  pass "docker: all proxy variables empty on build and both runs; podman builds get none"
else
  fail "docker proxies: build=[$(proxy_vals --build-arg <<< "${DB}")] run=[${RUN_PROXY}] per-run=[${PER_RUN}]"
fi

# Both lint targets skip the hook lint.yml skips (it starts its own container).
LINT_SKIP=$(sed -n 's/^ *SKIP: *//p' "${ROOT}/.github/workflows/lint.yml" | head -1)
run_ci_local "${PODMAN}" lint
LINT_CMD=$(last_run_args | tail -1)
run_ci_local "${PODMAN}" lint-all
LINT_ALL_CMD=$(last_run_args | tail -1)
if [ -n "${LINT_SKIP}" ] && [[ "${LINT_CMD}" == *"; SKIP=${LINT_SKIP} make lint" ]] &&
  [[ "${LINT_ALL_CMD}" == *"; SKIP=${LINT_SKIP} make lint-all" ]]; then
  pass "lint and lint-all both skip ${LINT_SKIP}, as lint.yml does"
else
  fail "lint skip: lint.yml=[${LINT_SKIP}] lint=[${LINT_CMD##*; }] lint-all=[${LINT_ALL_CMD##*; }]"
fi

# Docker builds runner with the host uid:gid it later runs as; podman (keep-id) passes neither.
if grep -qx "RUNNER_UID=$(id -u)" <<< "${DB}" && grep -qx "RUNNER_GID=$(id -g)" <<< "${DB}" &&
  ! grep -q 'RUNNER_' <<< "${PB}"; then
  pass "docker builds pass RUNNER_UID/RUNNER_GID = host uid:gid; podman builds don't"
else
  fail "runner ids: docker=[$(grep RUNNER_ <<< "${DB}" | tr '\n' ' ')] podman=[$(grep RUNNER_ <<< "${PB}" | tr '\n' ' ')]"
fi

# --- engine argv: user mapping and SELinux ---
if grep -qx -- '--userns=keep-id' <<< "${ARGS}" && ! grep -qx -- '--user' <<< "${ARGS}"; then
  pass "podman uses --userns=keep-id"
else
  fail "podman user mapping: ${ARGS}"
fi

run_ci_local "${DOCKER}" go-vet
ARGS=$(last_run_args)
if grep -A1 -x -- '--user' <<< "${ARGS}" | grep -qx "$(id -u):$(id -g)" && ! grep -q 'keep-id' <<< "${ARGS}" &&
  ! grep -q -- '--http-proxy' <<< "${ARGS}"; then
  pass "docker uses --user uid:gid, no keep-id, no podman-only flags"
else
  fail "docker user mapping: ${ARGS}"
fi
if grep -qx 'ARG 0' "${ENGINE_LOG}" && grep -q 'chmod 1777' "${ENGINE_LOG}"; then
  pass "docker opens up its named volumes before the run"
else
  fail "docker volume chmod missing"
fi
# A failed root helper fails the target before the main container starts.
STUB_HELPER_RC=1 run_ci_local "${DOCKER}" go-vet
if [ "${RC}" -ne 0 ] && grep -q 'go-vet *FAIL' <<< "${OUT}" &&
  grep -q 'could not open up the Docker volumes' <<< "${OUT}" && [ -z "$(last_run_args)" ]; then
  pass "docker: a failed volume helper fails the target without starting the main container"
else
  fail "failed volume helper: rc=${RC} main-run=[$(last_run_args | tr '\n' ' ')] out=[${OUT}]"
fi
run_ci_local "${DOCKER}" go-vet
# The root helper must not reach the checkout or run anything from a volume.
HELPER=$(awk '/^CALL$/{n++} {a[n]=a[n] $0 "\n"} END{for(i=1;i<=n;i++) if (a[i] ~ /\nARG --user\nARG 0\n/) {printf "%s", a[i]; exit}}' "${ENGINE_LOG}" |
  sed -n 's/^ARG //p')
helper_mounts=$(grep -A1 -x -- '-v' <<< "${HELPER}" | grep -vx -- '-v' | grep -vx -- '--' || true)
if [ -n "${helper_mounts}" ] && ! grep -q '^/' <<< "${helper_mounts}" &&
  grep -A1 -x -- '-e' <<< "${HELPER}" | grep -qx 'PATH=/usr/sbin:/usr/bin:/sbin:/bin' &&
  grep -A1 -x -- '--network' <<< "${HELPER}" | grep -qx 'none' &&
  [ "$(tail -3 <<< "${HELPER}" | head -2 | tr '\n' ' ')" = "/bin/sh -c " ] && [[ "$(tail -1 <<< "${HELPER}")" == "/bin/chmod 1777 "* ]]; then
  pass "docker root helper: named volumes only, fixed PATH, no network, absolute /bin/sh and /bin/chmod"
else
  fail "docker root helper: $(tr '\n' ' ' <<< "${HELPER}")"
fi

if ! grep -q ':/src:z' <<< "${ARGS}"; then
  pass "no :z label without SELinux"
else
  fail ":z added without SELinux"
fi
printf '#!/bin/sh\necho Enforcing\n' > "${SHIMS}/getenforce"
chmod +x "${SHIMS}/getenforce"
PATH="${SHIMS}:${PATH}" run_ci_local "${PODMAN}" go-vet
ARGS=$(last_run_args)
if grep -qx "${REPO}:/src:z" <<< "${ARGS}"; then
  pass ":z label added when SELinux is enforcing"
else
  fail "SELinux enforcing: checkout mount not labelled"
fi
rm -f "${SHIMS}/getenforce"

# --- --amd64: platform on build and run, separate image and volumes ---
STUB_IMAGE_RC=1 run_ci_local "${PODMAN}" --amd64 go-vet
BUILD=$(last_build_args)
ARGS=$(last_run_args)
vols=$(grep -A1 -x -- '-v' <<< "${ARGS}" | grep '^fullsend-ci-local' || true)
if grep -A1 -x -- '--platform' <<< "${BUILD}" | grep -qx linux/amd64 &&
  grep -A1 -x -- '--platform' <<< "${ARGS}" | grep -qx linux/amd64 &&
  grep -A1 -x -- '-t' <<< "${BUILD}" | tail -1 | grep -q '^fullsend-ci-local-amd64:' &&
  [ -n "${vols}" ] && ! grep -qv '^fullsend-ci-local-amd64-' <<< "${vols}"; then
  pass "--amd64 builds and runs linux/amd64 with its own image and volumes"
else
  fail "--amd64 plumbing: build=$(tr '\n' ' ' <<< "${BUILD}") run=$(tr '\n' ' ' <<< "${ARGS}")"
fi

# --- git worktree mounts ---
MAIN="${WORK}/main"
git init -q "${MAIN}"
add_ci_config "${MAIN}"
git -C "${MAIN}" add .
git -C "${MAIN}" -c user.name=t -c user.email=t@t commit -q -m init
git -C "${MAIN}" worktree add -q "${WORK}/wt" -b wt
REPO="${WORK}/wt"
run_ci_local "${PODMAN}" go-vet
ARGS=$(last_run_args)
COMMON=$(git -C "${REPO}" rev-parse --path-format=absolute --git-common-dir)
PTR="${HOME}/.cache/ci-local/gitfile-$(printf '%s' "${REPO}" | { sha256sum 2>/dev/null || shasum -a 256; } | cut -c1-12)"
if grep -qx "${COMMON}:/gitcommon" <<< "${ARGS}" && grep -qx "${PTR}:/src/.git:ro" <<< "${ARGS}" &&
  [ "$(cat "${PTR}")" = "gitdir: /gitcommon/worktrees/wt" ]; then
  pass "worktree: git common dir at /gitcommon and .git overlaid with a pointer"
else
  fail "worktree mounts: ${ARGS}"
fi

# A newline in the checkout path stays inside one -v argument, not a new engine option.
NL_REPO="${WORK}/new"$'\n'"line"
mkdir -p "${NL_REPO}/.git"
add_ci_config "${NL_REPO}"
SAVED_REPO="${REPO}"; REPO="${NL_REPO}"
run_ci_local "${PODMAN}" go-vet
REPO="${SAVED_REPO}"
# The stub logs one "ARG <value>" per argument, so a split argument would log "ARG line:/src".
if [ "${RC}" -eq 0 ] && grep -qx 'line:/src' "${ENGINE_LOG}" && ! grep -qx 'ARG line:/src' "${ENGINE_LOG}"; then
  pass "a newline in the checkout path stays inside its -v argument"
else
  fail "newline in checkout path split into engine arguments: rc=${RC}"
fi

# --- DNS: host resolvers in order; CI_LOCAL_DNS overrides; Docker Desktop adds use-vc ---
SCUTIL_SAMPLE='DNS configuration

resolver #1
  search domain[0] : corp.example
  nameserver[0] : 192.0.2.1
  nameserver[1] : 2001:db8::53
  nameserver[2] : 203.0.113.1
  nameserver[3] : 127.0.0.1

resolver #2
  domain   : corp.example
  nameserver[0] : 198.51.100.9

DNS configuration (for scoped queries)

resolver #1
  nameserver[0] : 198.51.100.1'
dns_of() { last_run_args | awk 'p=="--dns"{printf "dns=%s ", $0} p=="--dns-option"{printf "opt=%s ", $0} {p=$0}'; }

STUB_SCUTIL_OUT="${SCUTIL_SAMPLE}" run_ci_local "${PODMAN}" go-vet
if [ "$(dns_of)" = "dns=192.0.2.1 dns=203.0.113.1 " ]; then
  pass "podman: host's default resolvers in order (no IPv6, loopback, per-domain or scoped)"
else
  fail "podman host resolvers: [$(dns_of)]"
fi
STUB_SCUTIL_OUT="${SCUTIL_SAMPLE}" STUB_OS="Docker Desktop" run_ci_local "${DOCKER}" go-vet
if [ "$(dns_of)" = "dns=192.0.2.1 dns=203.0.113.1 opt=use-vc " ]; then
  pass "Docker Desktop: host resolvers plus --dns-option use-vc (DNS over TCP)"
else
  fail "Docker Desktop DNS: [$(dns_of)]"
fi
STUB_SCUTIL_OUT="${SCUTIL_SAMPLE}" STUB_OS="Ubuntu 24.04.1 LTS" run_ci_local "${DOCKER}" go-vet
if [ "$(dns_of)" = "dns=192.0.2.1 dns=203.0.113.1 " ]; then
  pass "Docker Engine (not Desktop): host resolvers, no use-vc"
else
  fail "Docker Engine DNS: [$(dns_of)]"
fi
STUB_SCUTIL_OUT="${SCUTIL_SAMPLE}" CI_LOCAL_DNS="198.51.100.7 198.51.100.8" run_ci_local "${PODMAN}" go-vet
if [ "$(dns_of)" = "dns=198.51.100.7 dns=198.51.100.8 " ]; then
  pass "CI_LOCAL_DNS list overrides the host's resolvers, in order"
else
  fail "CI_LOCAL_DNS override: [$(dns_of)]"
fi
STUB_SCUTIL_OUT="${SCUTIL_SAMPLE}" CI_LOCAL_DNS='' STUB_OS="Docker Desktop" run_ci_local "${DOCKER}" go-vet
if [ -z "$(dns_of)" ]; then
  pass "empty CI_LOCAL_DNS keeps the engine's own DNS (no --dns, no use-vc)"
else
  fail "empty CI_LOCAL_DNS: [$(dns_of)]"
fi
run_ci_local "${PODMAN}" go-vet
if [ -z "$(dns_of)" ]; then
  pass "no usable host resolver: no --dns"
else
  fail "no host resolver: [$(dns_of)]"
fi

# --- clean: old labelled images and unsuffixed node_modules volumes only, never forced ---
run_ci_local "${PODMAN}" go-vet
CUR=$(last_run_args | grep -m1 -E '^fullsend-ci-local:')
removed() { awk '/^CALL$/{n++} {a[n]=a[n] $0 " "} END{for(i=1;i<=n;i++) if (a[i] ~ /^CALL ARG (rmi|volume ARG rm) /) print a[i]}' "${ENGINE_LOG}" |
  sed -E 's/^CALL ARG (rmi|volume ARG rm) ARG //; s/ $//' | sort | tr '\n' ' '; }
STUB_IMAGES="aaa111 localhost/${CUR}
bbb222 localhost/fullsend-ci-local:0ldtag000000
ccc333 <none>:<none>
ddd444 localhost/fullsend-ci-local-amd64:0ldtag000000
eee555 localhost/fullsend-ci-local:busytag00000" \
STUB_VOLUMES="fullsend-ci-local-nm-root
fullsend-ci-local-nm-root-0123456789ab
fullsend-ci-local-nm-workersrc
fullsend-ci-local-go
fullsend-ci-local-amd64-nm-root" \
STUB_BUSY="fullsend-ci-local:busytag00000" run_ci_local "${PODMAN}" clean
if [ "${RC}" -eq 0 ] && [ -n "${CUR}" ] &&
  [ "$(removed)" = "ccc333 fullsend-ci-local-nm-root fullsend-ci-local-nm-workersrc fullsend-ci-local:0ldtag000000 fullsend-ci-local:busytag00000 " ] &&
  grep -q "kept image fullsend-ci-local:busytag00000 (in use" <<< "${OUT}" &&
  ! grep -qE 'ARG (-f|--force)$' "${ENGINE_LOG}" && ! grep -qxE 'ARG (run|build)' "${ENGINE_LOG}"; then
  pass "clean: removes old/untagged images and unsuffixed node_modules volumes; keeps current, amd64, per-checkout, caches; never forced"
else
  fail "clean: rc=${RC} current=[${CUR}] removed=[$(removed)] out=[${OUT}]"
fi
STUB_IMAGES="ddd444 localhost/fullsend-ci-local-amd64:0ldtag000000
bbb222 localhost/fullsend-ci-local:0ldtag000000" \
STUB_VOLUMES="fullsend-ci-local-nm-root
fullsend-ci-local-amd64-nm-root" run_ci_local "${PODMAN}" --amd64 clean
if [ "$(removed)" = "fullsend-ci-local-amd64-nm-root fullsend-ci-local-amd64:0ldtag000000 " ]; then
  pass "clean --amd64: only the amd64 images and volumes"
else
  fail "clean --amd64: removed=[$(removed)]"
fi
STUB_IMAGES="bbb222 localhost/fullsend-ci-local:0ldtag000000" STUB_VOLUMES="fullsend-ci-local-nm-root" \
STUB_LIST_RC=125 run_ci_local "${PODMAN}" clean
if [ "${RC}" -ne 0 ] && [ -z "$(removed)" ] && grep -q 'cannot list images; is .*podman running?' <<< "${OUT}" &&
  ! grep -q 'kept)' <<< "${OUT}"; then
  pass "clean: a failed listing (engine stopped) exits non-zero and removes nothing"
else
  fail "clean with failed listing: rc=${RC} removed=[$(removed)] out=[${OUT}]"
fi

if [ "${FAILURES}" -ne 0 ]; then
  echo "${FAILURES} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
