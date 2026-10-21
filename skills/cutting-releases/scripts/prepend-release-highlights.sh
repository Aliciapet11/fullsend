#!/usr/bin/env bash
# Prepend user-facing highlights to a published release's notes.
#
# Usage: prepend-release-highlights.sh <tag> <<'HIGHLIGHTS_EOF'
#        <highlights>
#        HIGHLIGHTS_EOF
#
# Arguments:
#   tag    Release tag (e.g. v0.5.0)
#   stdin  Markdown highlights only; pass them in a quoted heredoc so the
#          shell expands nothing
#
# The script fetches the existing notes itself, and they move between gh and
# disk through files only. v0.33.0 shipped a literal
# `$(cat /tmp/release-body-existing.txt)` in place of its changelog because
# the notes were pasted into a quoted heredoc by hand (#8366). After the edit
# the notes are re-read and must match byte for byte; otherwise the original
# notes are restored and the script fails.
set -euo pipefail

usage="Usage: prepend-release-highlights.sh <tag> <<'HIGHLIGHTS_EOF' ... HIGHLIGHTS_EOF"
if [[ $# -ne 1 ]]; then
  echo "$usage" >&2
  exit 1
fi
TAG="$1"
if [[ ! "$TAG" =~ ^v[0-9] ]]; then
  echo "ERROR: Tag must start with 'v' followed by a digit (e.g. v0.5.0)" >&2
  exit 1
fi
if [[ -t 0 ]]; then
  echo "ERROR: pass the highlights on stdin; $usage" >&2
  exit 1
fi

WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

HIGHLIGHTS="$WORKDIR/highlights.md"
cat > "$HIGHLIGHTS"
if ! grep -q '[^[:space:]]' "$HIGHLIGHTS"; then
  echo "ERROR: highlights on stdin are empty or blank" >&2
  exit 1
fi

# Placeholders that mean the highlights were templated instead of written:
# a line that is only a shell expansion (the v0.33.0 signature), any
# `$(cat ...)`, or the old skill template's <existing body>/<highlights>.
# Inline expansions in prose (e.g. `${OPENAI_API_KEY}` in v0.38.0) are fine.
# shellcheck disable=SC2016  # the pattern matches a literal, unexpanded $(
placeholder='^[[:space:]]*\$(\(.*\)|\{.*\})[[:space:]]*$|\$\(cat[[:space:]]|<(existing body|highlights)>'
if grep -Eq "$placeholder" "$HIGHLIGHTS"; then
  echo "ERROR: highlights contain an unexpanded shell expression or template placeholder" >&2
  exit 1
fi
if grep -q '^## Changelog' "$HIGHLIGHTS"; then
  echo "ERROR: highlights must not contain the changelog; pass highlights only" >&2
  exit 1
fi

# --template prints the stored body byte for byte; `-q .body` would append
# a newline, and $(...) would drop trailing ones.
fetch_body() {
  gh release view "$TAG" --repo fullsend-ai/fullsend --json body --template '{{.body}}' > "$1"
}

fetch_body "$WORKDIR/old.md"
# GoReleaser bodies start with exactly this line. Refuse anything else, and
# say which case it is so the releaser knows what to look at.
first_line=$(head -n 1 "$WORKDIR/old.md")
if [[ "$first_line" != "## Changelog" ]]; then
  if ! grep -q '## Changelog' "$WORKDIR/old.md"; then
    reason="have no '## Changelog' line (empty changelog, or not a GoReleaser body)"
  elif [[ "$first_line" == *"## Changelog"* ]]; then
    reason="start with '## Changelog' plus stray characters (carriage return or byte-order mark?)"
  else
    reason="have text above '## Changelog' (highlights already added?)"
  fi
  echo "ERROR: release $TAG notes $reason; refusing to edit" >&2
  exit 1
fi

{
  cat "$HIGHLIGHTS"
  # Blank line before --- even without a final newline, or the last
  # highlights line would render as a setext heading.
  printf '\n\n---\n\n'
  cat "$WORKDIR/old.md"
} > "$WORKDIR/new.md"

# An edit can fail after GitHub applied it (e.g. a timeout), so a failure
# falls through to the check below instead of exiting unverified.
if ! gh release edit "$TAG" --repo fullsend-ai/fullsend --notes-file "$WORKDIR/new.md" > /dev/null; then
  echo "WARNING: gh release edit failed; checking what the release now holds" >&2
fi

if ! fetch_body "$WORKDIR/after.md" || ! cmp -s "$WORKDIR/new.md" "$WORKDIR/after.md"; then
  echo "ERROR: release $TAG notes do not match what was sent; restoring the original" >&2
  if ! gh release edit "$TAG" --repo fullsend-ai/fullsend --notes-file "$WORKDIR/old.md" > /dev/null; then
    trap - EXIT
    echo "ERROR: restore failed; original notes saved at $WORKDIR/old.md" >&2
  fi
  exit 1
fi

echo "Prepended highlights to release $TAG"
