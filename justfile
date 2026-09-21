# ezBookkeeping — build and run on localhost (no Docker)
#
#   just build   -> Go backend binary (./ezbookkeeping) + Vue frontend (./dist) + ezbk CLI (./cli/bin/ezbk)
#                   + the MCP server (./mcp/dist), after syncing the vendored errfile copies
#   just run     -> serves both at http://localhost:8080/ (prints the full URL)
#   just url     -> prints the full URL to open in the browser
#   just users   -> lists the sign-in users (username, email) in the local database
#   just reset-password NAME -> sets a new password for NAME (prompted, never echoed)
#   just lint    -> go vet (+ errfile/catch-must-report) for both Go modules, then the two eslint runs
#   just test    -> every suite: root Go, cli Go, vitest, mcp vitest
#   just check-errors -> builds bin/errfilecheck and runs the error-file coverage script (pm/error_err.mdx §13.3)
#
# Runtime state stays in the repo root, all git-ignored:
#   data/    sqlite db (data/ezbookkeeping.db) + generated secret key
#   log/     log/ezbookkeeping.log
#   storage/ uploaded files (avatars, pictures)
# Every fault from every runtime goes to ~/T/ezbookkeeping/error.err (pm/error_err.mdx).

set shell := ["bash", "-euo", "pipefail", "-c"]

port := env_var_or_default("EBK_PORT", "8080")
bin := "ezbookkeeping"
url := "http://localhost:" + port + "/"

# Print the URL, then list recipes
default:
    @echo ""
    @echo "  ezBookkeeping: {{url}}"
    @echo "  (start it with: just run — sign-in help: pm/accounts.mdx)"
    @echo ""
    @just --list

# Print the full URL to open in the browser
url:
    @echo "{{url}}"

# Build backend + frontend + the ezbk CLI + the MCP server (after syncing the vendored errfile copies)
build: sync-errfile build-backend build-frontend build-cli build-mcp
    @echo ""
    @echo "Build complete. Start it with: just run"
    @echo "Then open: {{url}}"

# Regenerate cli/internal/errfile and mcp/src/errfile from pkg/errfile and src/lib/errfile (pm/error_err.mdx §4.1, §5.5)
sync-errfile:
    @bash scripts/sync-errfile.sh

# Build the Go analyzer errfile/catch-must-report into ./bin/errfilecheck (pm/error_err.mdx §13.1)
build-errfilecheck:
    @command -v go >/dev/null || { echo "Error: go is required"; exit 127; }
    @mkdir -p bin
    cd scripts/errfilecheck && go build -o ../../bin/errfilecheck .

# Prove every source file reports its faults: builds bin/errfilecheck, then runs the coverage script (pm/error_err.mdx §13.3)
check-errors *ARGS: build-errfilecheck
    @command -v node >/dev/null || { echo "Error: node is required"; exit 127; }
    node scripts/error-file-coverage.mjs {{ARGS}}

# go vet (+ errfile/catch-must-report) for both Go modules, then eslint for the web app and the MCP
lint: build-errfilecheck
    @echo "==> go vet ./... (root)"
    go vet ./...
    @echo "==> go vet ./... (cli)"
    cd cli && go vet ./...
    @echo "==> go vet -vettool=bin/errfilecheck ./... (root)"
    go vet -vettool="{{justfile_directory()}}/bin/errfilecheck" ./...
    @echo "==> go vet -vettool=bin/errfilecheck ./... (cli)"
    cd cli && go vet -vettool="{{justfile_directory()}}/bin/errfilecheck" ./...
    @echo "==> npm run lint"
    NODE_OPTIONS=--max-old-space-size=8192 npm run lint
    @if [ -f mcp/package.json ]; then echo "==> mcp: npm run lint"; cd mcp && npm run lint; fi

# Build the MCP server (its own package.json) into ./mcp/dist
build-mcp:
    @command -v npm >/dev/null || { echo "Error: node/npm is required"; exit 127; }
    @[ -f mcp/package.json ] || { echo "mcp/package.json not found; skipping the MCP build"; exit 0; }
    @echo "==> Building the MCP server..."
    cd mcp && npm ci --no-audit --no-fund && npm run build

# Print the `claude mcp add` line for the MCP server (pm/mcp.mdx §5.1); `just install-mcp yes` runs it
install-mcp CONFIRM="":
    @[ -f mcp/dist/index.js ] || { echo "Error: MCP not built. Run: just build-mcp"; exit 1; }
    @echo 'claude mcp add --scope user ezbookkeeping -- "{{justfile_directory()}}/mcp/dist/index.js" serve'
    @if [ "{{CONFIRM}}" = "yes" ]; then claude mcp add --scope user ezbookkeeping -- "{{justfile_directory()}}/mcp/dist/index.js" serve; else echo "(not run: pass 'yes' to register it: just install-mcp yes)"; fi

# Run every suite: root Go, cli Go, the web app's vitest, and the MCP's vitest
test:
    @echo "==> go test ./... (root)"
    go test ./...
    @echo "==> go test ./... (cli)"
    cd cli && go test ./...
    @echo "==> npm test"
    npm test
    @if [ -f mcp/package.json ]; then echo "==> mcp: npm test"; cd mcp && npm test; fi

# Build the Go backend binary (sqlite needs cgo)
build-backend:
    @command -v go >/dev/null || { echo "Error: go is required"; exit 127; }
    @echo "==> Building backend..."
    CGO_ENABLED=1 go build -trimpath \
      -ldflags "-X main.Version=$(grep '"version":' package.json | head -1 | tr -d ' ",' | cut -d: -f2) -X main.CommitHash=$(git rev-parse --short=7 HEAD)" \
      -o {{bin}} ezbookkeeping.go

# Build the Vue frontend into ./dist
build-frontend:
    @command -v npm >/dev/null || { echo "Error: node/npm is required"; exit 127; }
    @echo "==> Installing frontend dependencies..."
    npm install --no-audit --no-fund
    @echo "==> Building frontend..."
    npm run build

# Build the ezbk CLI (its own Go module under cli/, stdlib only) into ./cli/bin/ezbk
build-cli:
    @command -v go >/dev/null || { echo "Error: go is required"; exit 127; }
    @echo "==> Building ezbk CLI..."
    cd cli && go build -trimpath -o bin/ezbk ./cmd/ezbk

# Print the line that puts ezbk on PATH (nothing is changed; add it to ~/.zshrc yourself)
install-cli:
    @[ -x ./cli/bin/ezbk ] || { echo "Error: ezbk not built. Run: just build-cli"; exit 1; }
    @echo 'export PATH="{{justfile_directory()}}/cli/bin:$PATH"'

# Run the server on localhost (foreground; Ctrl+C to stop)
run:
    @[ -x ./{{bin}} ] || { echo "Error: backend not built. Run: just build"; exit 1; }
    @[ -f ./dist/index.html ] || { echo "Error: frontend not built. Run: just build"; exit 1; }
    @mkdir -p data log storage
    @[ -s data/.secret_key ] || { openssl rand -hex 24 | tr -d '\n' > data/.secret_key; echo "Generated data/.secret_key"; }
    @echo ""
    @echo "=============================================="
    @echo "  ezBookkeeping: {{url}}"
    @echo "=============================================="
    @echo ""
    EBK_WORK_DIR="$PWD" \
    EBK_SERVER_HTTP_ADDR=127.0.0.1 \
    EBK_SERVER_HTTP_PORT={{port}} \
    EBK_SERVER_DOMAIN=localhost \
    EBK_SERVER_STATIC_ROOT_PATH=dist \
    EBKCFP_SECURITY_SECRET_KEY="$PWD/data/.secret_key" \
    ./{{bin}} --conf-path conf/ezbookkeeping.ini server run

# List the sign-in users in the local database (username, email, disabled?) — pm/accounts.mdx §2
users:
    @[ -f data/ezbookkeeping.db ] || { echo "No database yet (data/ezbookkeeping.db). Run: just run, then create an account at {{url}}"; exit 0; }
    @command -v sqlite3 >/dev/null || { echo "Error: sqlite3 is required"; exit 127; }
    @n=$(sqlite3 data/ezbookkeeping.db "select count(*) from user where deleted=0"); \
     if [ "$n" = "0" ]; then echo "No users yet. Open {{url}} and click 'Create an account'."; \
     else sqlite3 -header -column data/ezbookkeeping.db "select username, email, disabled from user where deleted=0 order by uid"; fi

# Set a new password for USERNAME without email (prompted, never echoed or kept in history) — pm/accounts.mdx §4
reset-password USERNAME:
    @[ -x ./{{bin}} ] || { echo "Error: backend not built. Run: just build"; exit 1; }
    @read -r -s -p "New password for {{USERNAME}} (6-128 chars): " p1; echo; \
     read -r -s -p "Repeat it: " p2; echo; \
     [ "$p1" = "$p2" ] || { echo "Error: the two passwords differ; nothing changed"; exit 1; }; \
     EBK_WORK_DIR="$PWD" EBKCFP_SECURITY_SECRET_KEY="$PWD/data/.secret_key" \
     ./{{bin}} --conf-path conf/ezbookkeeping.ini userdata user-modify-password --username "{{USERNAME}}" --password "$p1"; \
     echo "Done. Log in at {{url}} as {{USERNAME}}."

# Run the Vite dev server (hot reload, :8081) — needs `just run` in another terminal
dev:
    npm run serve

# Remove build outputs (keeps data/)
clean:
    rm -f {{bin}}
    rm -rf dist cli/bin bin mcp/dist
