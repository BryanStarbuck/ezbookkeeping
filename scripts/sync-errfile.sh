#!/usr/bin/env bash
# sync-errfile.sh — regenerate the two vendored copies of the error-file libraries
# (pm/error_err.mdx §4.1, §5.5). Idempotent; safe to re-run at any time; `just build` runs it.
#
#   (a) pkg/errfile/*.go (tests included; NOT server/, NOT vendor_drift_test.go) + testdata/*.json
#       →  cli/internal/errfile/
#       each .go file prefixed with:  // GENERATED from pkg/errfile — run scripts/sync-errfile.sh
#   (b) src/lib/errfile/{index,core,describe,redact,fold,format}.ts       →  mcp/src/errfile/
#       each prefixed with:           // GENERATED from src/lib/errfile — run scripts/sync-errfile.sh
#       (skipped silently while src/lib/errfile does not exist yet)
#
# A file in a target directory that no longer has a source is removed, so "copy" never means "drift":
# pkg/errfile/vendor_drift_test.go and mcp/src/errfile/vendor-drift.test.ts fail on any difference.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_HEADER='// GENERATED from pkg/errfile — run scripts/sync-errfile.sh'
TS_HEADER='// GENERATED from src/lib/errfile — run scripts/sync-errfile.sh'

# write_if_changed <target> reads the new content on stdin and only touches the file when it differs
write_if_changed() {
    local target="$1" tmp
    tmp="$(mktemp "${target}.XXXXXX")"
    cat > "$tmp"

    if [ -f "$target" ] && cmp -s "$tmp" "$target"; then
        rm -f "$tmp"
    else
        mv -f "$tmp" "$target"
        echo "  wrote $target"
    fi
}

# ── (a) Go: pkg/errfile → cli/internal/errfile ────────────────────────────────────────────────

GO_SRC="$ROOT/pkg/errfile"
GO_DST="$ROOT/cli/internal/errfile"
mkdir -p "$GO_DST/testdata"
echo "==> Go: pkg/errfile -> cli/internal/errfile"

go_keep=" "

for src in "$GO_SRC"/*.go; do
    name="$(basename "$src")"

    # the drift test compares the two directories; it lives with the source only
    if [ "$name" = "vendor_drift_test.go" ]; then
        continue
    fi

    go_keep="$go_keep$name "
    { printf '%s\n' "$GO_HEADER"; cat "$src"; } | write_if_changed "$GO_DST/$name"
done

for src in "$GO_SRC"/testdata/*.json; do
    [ -e "$src" ] || continue
    name="testdata/$(basename "$src")"
    go_keep="$go_keep$name "
    write_if_changed "$GO_DST/$name" < "$src"
done

for existing in "$GO_DST"/*.go "$GO_DST"/testdata/*.json; do
    [ -e "$existing" ] || continue
    rel="${existing#"$GO_DST"/}"

    case "$go_keep" in
        *" $rel "*) ;;
        *)
        rm -f "$existing"
        echo "  removed $existing (no source)"
        ;;
    esac
done

# ── (b) TypeScript: src/lib/errfile → mcp/src/errfile ──────────────────────────────────────────

TS_SRC="$ROOT/src/lib/errfile"
TS_DST="$ROOT/mcp/src/errfile"

if [ -d "$TS_SRC" ]; then
    echo "==> TS: src/lib/errfile -> mcp/src/errfile"
    mkdir -p "$TS_DST"

    for name in index.ts core.ts describe.ts redact.ts fold.ts format.ts; do
        if [ -f "$TS_SRC/$name" ]; then
            { printf '%s\n' "$TS_HEADER"; cat "$TS_SRC/$name"; } | write_if_changed "$TS_DST/$name"
        fi
    done
fi

echo "sync-errfile: done"
