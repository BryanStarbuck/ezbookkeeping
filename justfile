# ezBookkeeping — build and run on localhost (no Docker)
#
#   just build   -> Go backend binary (./ezbookkeeping) + Vue frontend (./dist) + ezbk CLI (./cli/bin/ezbk)
#   just run     -> serves both at http://localhost:8080/
#
# Runtime state stays in the repo root, all git-ignored:
#   data/    sqlite db (data/ezbookkeeping.db) + generated secret key
#   log/     log/ezbookkeeping.log
#   storage/ uploaded files (avatars, pictures)

set shell := ["bash", "-euo", "pipefail", "-c"]

port := env_var_or_default("EBK_PORT", "8080")
bin := "ezbookkeeping"

# List recipes
default:
    @just --list

# Build backend + frontend + the ezbk CLI
build: build-backend build-frontend build-cli
    @echo ""
    @echo "Build complete. Start it with: just run"

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
    @echo "  ezBookkeeping: http://localhost:{{port}}/"
    @echo "=============================================="
    @echo ""
    EBK_WORK_DIR="$PWD" \
    EBK_SERVER_HTTP_ADDR=127.0.0.1 \
    EBK_SERVER_HTTP_PORT={{port}} \
    EBK_SERVER_DOMAIN=localhost \
    EBK_SERVER_STATIC_ROOT_PATH=dist \
    EBKCFP_SECURITY_SECRET_KEY="$PWD/data/.secret_key" \
    ./{{bin}} --conf-path conf/ezbookkeeping.ini server run

# Run the Vite dev server (hot reload, :8081) — needs `just run` in another terminal
dev:
    npm run serve

# Remove build outputs (keeps data/)
clean:
    rm -f {{bin}}
    rm -rf dist cli/bin
