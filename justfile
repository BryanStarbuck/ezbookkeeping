# ezBookkeeping — build and run on localhost (no Docker)
#
#   just build   -> Go backend binary (./ezbookkeeping) + Vue frontend (./dist) + ezbk CLI (./cli/bin/ezbk)
#                   + the MCP server (./mcp/dist), after syncing the vendored errfile copies
#   just run     -> (re)starts the server in the background at http://localhost:8080/: stops whatever
#                   ezBookkeeping server is running (just run, ezbk up, a foreground run), starts the
#                   current build, waits until it is healthy and checks the machine plane is armed.
#                   So `just build && just run` always serves your latest build.
#   just run-fg  -> the same restart, but in the foreground with logs in the terminal (Ctrl+C stops)
#   just stop / just status / just logs -> stop it, one-line status, follow the server log
#   just url     -> prints the full URL to open in the browser
#   just users   -> lists the sign-in users (username, email) in the local database
#   just reset-password NAME -> sets a new password for NAME (prompted, never echoed)
#   just lint    -> go vet (+ errfile/catch-must-report) for both Go modules, then the two eslint runs
#   just test    -> every suite: root Go, cli Go, vitest, mcp vitest
#   just check-errors -> builds bin/errfilecheck and runs the error-file coverage script (pm/error_err.mdx §13.3)
#
# Runtime state lives OUTSIDE the repo (it holds real financial data once statements are imported):
#   ~/T/_ezbookkeeping/data/     sqlite db (ezbookkeeping.db) + upstream's secret_key
#   ~/T/_ezbookkeeping/log/      ezbookkeeping.log
#   ~/T/_ezbookkeeping/storage/  uploaded files (avatars, pictures)
# EZBK_STATE_DIR overrides the base. `just run` and `ezbk up` move a db left in ./data/ on first start.
# Every fault from every runtime goes to ~/T/ezbookkeeping/error.err (pm/error_err.mdx).

set shell := ["bash", "-euo", "pipefail", "-c"]

port := env_var_or_default("EBK_PORT", "8080")
# the machine plane's tiers at boot (apis.mdx §9.1): writes on by default so ezbk and the MCP can
# categorise and import (every write still needs a dry run and a confirm token); admin off.
# Override per run: EZBK_MACHINE_ALLOW_WRITE=0 just run
allow_write := env_var_or_default("EZBK_MACHINE_ALLOW_WRITE", "1")
allow_admin := env_var_or_default("EZBK_MACHINE_ALLOW_ADMIN", "0")
# runtime state (database, logs, uploads) lives OUTSIDE the repo — CLAUDE.md "Runtime state location"
state := env_var_or_default("EZBK_STATE_DIR", env_var("HOME") + "/T/_ezbookkeeping")
db := state + "/data/ezbookkeeping.db"
bin := "ezbookkeeping"
url := "http://localhost:" + port + "/"
server := "ROOT=\"" + justfile_directory() + "\" PORT=" + port + " STATE=\"" + state + "\" ALLOW_WRITE=" + allow_write + " ALLOW_ADMIN=" + allow_admin + " bash scripts/server.sh"

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
    @echo "Build complete. (Re)start the server on it with: just run"
    @if curl -fsS -m 2 "http://127.0.0.1:{{port}}/healthz.json" >/dev/null 2>&1; then echo "Note: a server is running the PREVIOUS build right now — 'just run' restarts it on this one."; fi
    @echo "Then open: {{url}}   (restart Claude Code to load a new MCP build)"

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

# (Re)start the server in the background on the current build, wait until healthy, verify the plane
run:
    @{{server}} start

# Same as run
restart:
    @{{server}} start

# Stop any running server, then run in the foreground with logs in the terminal (Ctrl+C stops)
run-fg:
    @{{server}} fg

# Stop the server (whether just run or ezbk up started it)
stop:
    @{{server}} stop

# Is it up? pid, URL and the machine plane's tiers
status:
    @{{server}} status

# Follow the server log (~/T/_ezbookkeeping/server.log)
logs:
    @{{server}} logs

# List the sign-in users in the local database (username, email, disabled?) — pm/accounts.mdx §2
users:
    @[ -f "{{db}}" ] || { echo "No database yet ({{db}}). Run: just run, then create an account at {{url}}"; exit 0; }
    @command -v sqlite3 >/dev/null || { echo "Error: sqlite3 is required"; exit 127; }
    @n=$(sqlite3 "{{db}}" "select count(*) from user where deleted=0"); \
     if [ "$n" = "0" ]; then echo "No users yet. Open {{url}} and click 'Create an account'."; \
     else sqlite3 -header -column "{{db}}" "select username, email, disabled from user where deleted=0 order by uid"; fi

# Set a new password for USERNAME without email (prompted, never echoed or kept in history) — pm/accounts.mdx §4
reset-password USERNAME:
    @[ -x ./{{bin}} ] || { echo "Error: backend not built. Run: just build"; exit 1; }
    @read -r -s -p "New password for {{USERNAME}} (6-128 chars): " p1; echo; \
     read -r -s -p "Repeat it: " p2; echo; \
     [ "$p1" = "$p2" ] || { echo "Error: the two passwords differ; nothing changed"; exit 1; }; \
     EBK_WORK_DIR="$PWD" EBKCFP_SECURITY_SECRET_KEY="{{state}}/data/.secret_key" EBK_DATABASE_DB_PATH="{{db}}" \
     ./{{bin}} --conf-path conf/ezbookkeeping.ini userdata user-modify-password --username "{{USERNAME}}" --password "$p1"; \
     echo "Done. Log in at {{url}} as {{USERNAME}}."

# Run the Vite dev server (hot reload, :8081) — needs the server up (just run)
dev:
    npm run serve

# Remove build outputs (keeps data/)
clean:
    rm -f {{bin}}
    rm -rf dist cli/bin bin mcp/dist
