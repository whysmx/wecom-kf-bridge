#!/usr/bin/env bash
# Print the CHANGELOG.md section for a version (e.g. v0.1.2). Fails if the
# section is missing or empty, so a release can never be published without
# update notes.
set -euo pipefail
ver="${1:?usage: release_notes.sh vX.Y.Z [CHANGELOG.md]}"
file="${2:-CHANGELOG.md}"
notes="$(awk -v v="$ver" '
  /^## / { if (found) exit; if ($2 == v || $2 == "[" v "]") { found=1; next } }
  found { print }
' "$file")"
if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
  echo "release_notes: no CHANGELOG section for $ver" >&2
  exit 1
fi
printf '%s\n' "$notes"
