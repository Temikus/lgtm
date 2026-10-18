# LGTM justfile

default:
    @just --list

# Build the lgtm binary
build:
    go build -trimpath -o lgtm ./cmd/lgtm

# Lint Go code
lint:
    test -z "$(gofmt -l .)" || (gofmt -l . && exit 1)
    go vet ./...
    golangci-lint run

# Format Go code
fmt:
    gofmt -w .

# Run tests: all (default), unit, site, or integration.
# `all` leaves out integration: it calls the live Jev API and bills the key.
test kind="all":
    #!/usr/bin/env bash
    set -euo pipefail
    case "{{kind}}" in
      all)  just test unit; if [ -d website ]; then just test site; fi ;;
      unit) go test -race -count=1 ./... ;;
      site) cd website && npm ci && npx tsc --noEmit && npx vitest run ;;
      integration)
        : "${OPENROUTER_API_KEY:?set OPENROUTER_API_KEY}"
        LGTM_INTEGRATION=1 go test -tags integration -race -count=1 -v ./... ;;
      *) echo "unknown test kind: {{kind}}" >&2; exit 2 ;;
    esac

# Run security scans: all (default) or vuln
scan kind="all":
    #!/usr/bin/env bash
    set -euo pipefail
    case "{{kind}}" in
      all|vuln) govulncheck ./... ;;
      *) echo "unknown scan kind: {{kind}}" >&2; exit 2 ;;
    esac

# Lint, then every test except integration
check: lint test

# Run the website locally
site-dev:
    cd website && npm ci && npx wrangler dev

# Deploy the website by hand (CI deploys on push to main)
site-deploy:
    cd website && npm ci && npx wrangler d1 migrations apply lgtm-waitlist --remote && npx wrangler deploy

# Start the release workflow for the next patch, minor, or major version.
# Release tags sit on commits off main, so read the highest tag, not git describe.
release bump:
    #!/usr/bin/env bash
    set -euo pipefail
    git fetch -q --tags origin
    last=$(git tag --list 'v*.*.*' --sort=-v:refname | head -1)
    last=${last:-v0.0.0}
    IFS=. read -r major minor patch <<<"${last#v}"
    case "{{bump}}" in
      patch) patch=$((patch+1)) ;;
      minor) minor=$((minor+1)); patch=0 ;;
      major) major=$((major+1)); minor=0; patch=0 ;;
      *) echo "usage: just release [patch|minor|major]" >&2; exit 2 ;;
    esac
    next="v${major}.${minor}.${patch}"
    gh workflow run release.yml --ref main -f version="$next"
    echo "Started the $next release: $(gh run list --workflow release.yml --limit 1 --json url --jq '.[0].url')"

# Build release archives locally without publishing
release-dry:
    goreleaser release --snapshot --clean --skip=sign,sbom
