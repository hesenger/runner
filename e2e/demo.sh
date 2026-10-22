#!/usr/bin/env bash
# Demo of the monitoring UI: runs runner in the foreground with two stub apps
# deployed from a local fake GitHub API, so the admin UI has something to
# show. After 30s it publishes v2 of the first app, and after 60s a corrupt
# artifact of the second one, so a successful and a failed deploy show up.
# Ctrl-C stops runner, its apps and the fake API, and deletes the temp dir.
#
# Usage: bash e2e/demo.sh   (from any directory)
# Ports: apps on 18000 and 18001, admin UI on 18080, fake GitHub API on 18100
set -euo pipefail
# job control puts each background job in its own process group, so Ctrl-C
# only reaches this script, which then stops runner with SIGTERM like a
# service manager would
set -m

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
API_PORT=18000
WEB_PORT=18001
ADMIN_PORT=18080
GITHUB_PORT=18100
GITHUB="http://127.0.0.1:$GITHUB_PORT"

TMP_ROOT="${TMPDIR:-/tmp}"
WORKDIR="$(mktemp -d "${TMP_ROOT%/}/runner-demo.XXXXXX")"
PIDS=()
RUNNER_PID=""

log() { printf '[demo] %s\n' "$*"; }

cleanup() {
	local status=$?
	if [[ -n "$RUNNER_PID" ]] && kill -0 "$RUNNER_PID" 2>/dev/null; then
		kill -TERM "$RUNNER_PID" 2>/dev/null || true
		wait "$RUNNER_PID" 2>/dev/null || true
	fi
	# each job is a process group: also stops the sleep of the publisher
	for pid in "${PIDS[@]+"${PIDS[@]}"}"; do
		kill -- "-$pid" 2>/dev/null || true
	done
	wait 2>/dev/null || true
	rm -rf "$WORKDIR"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# make_zip <zip> <file>: store <file> at the zip root, like a GitHub Actions artifact
make_zip() {
	if command -v zip >/dev/null 2>&1; then
		zip -q -j "$1" "$2"
	else
		python3 -m zipfile -c "$1" "$2"
	fi
}

for port in "$API_PORT" "$WEB_PORT" "$ADMIN_PORT" "$GITHUB_PORT"; do
	if curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$port/"; then
		log "port $port is already in use"
		exit 1
	fi
done

log "building runner, fakegithub and stubapp v1, v2"
mkdir -p "$WORKDIR/artifacts" "$WORKDIR/build/v1" "$WORKDIR/build/v2"
(
	cd "$REPO_ROOT"
	go build -ldflags "-X main.version=demo" -o "$WORKDIR/runner" .
	go build -o "$WORKDIR/fakegithub" ./e2e/fakegithub
	go build -ldflags "-X main.version=v1" -o "$WORKDIR/build/v1/stubapp" ./e2e/stubapp
	go build -ldflags "-X main.version=v2" -o "$WORKDIR/build/v2/stubapp" ./e2e/stubapp
)
make_zip "$WORKDIR/artifacts/stubapp-v1.zip" "$WORKDIR/build/v1/stubapp"
make_zip "$WORKDIR/artifacts/stubapp-v2.zip" "$WORKDIR/build/v2/stubapp"
printf 'not a zip' > "$WORKDIR/artifacts/stubapp-corrupt.zip"

"$WORKDIR/fakegithub" --port "$GITHUB_PORT" --dir "$WORKDIR/artifacts" > "$WORKDIR/fakegithub.log" 2>&1 &
PIDS+=($!)
deadline=$((SECONDS + 10))
until curl -fsS -o /dev/null "$GITHUB/repos/demo/api/actions/artifacts" 2>/dev/null; do
	((SECONDS < deadline)) || { log "fakegithub did not start"; exit 1; }
	sleep 0.1
done

# publish <zip> <id> <name>: make <zip> the newest artifact named <name>.
# fakegithub serves the same list for every repo, so apps tell their
# artifacts apart by artifactName.
publish() {
	curl -fsS -X POST "$GITHUB/publish?file=$1&id=$2&name=$3" > /dev/null
	log "published $1 as artifact $2 ($3)"
}
publish stubapp-v1.zip 1 api
publish stubapp-v1.zip 2 web

cat > "$WORKDIR/config.json" <<JSON
{
  "githubApi": "$GITHUB",
  "pollInterval": "2s",
  "healthCheckInterval": "500ms",
  "drainDelay": "1s",
  "adminPort": $ADMIN_PORT,
  "adminUser": "admin",
  "adminPassword": "admin",
  "apps": [
    {
      "externalPort": $API_PORT,
      "repo": "demo/api",
      "binName": "stubapp",
      "artifactName": "api",
      "token": "dummy-token"
    },
    {
      "externalPort": $WEB_PORT,
      "repo": "demo/web",
      "binName": "stubapp",
      "artifactName": "web",
      "token": "dummy-token"
    }
  ]
}
JSON

# later deploys, so the UI shows a successful and a failed one
(
	sleep 30
	publish stubapp-v2.zip 3 api
	sleep 30
	publish stubapp-corrupt.zip 4 web
) &
PIDS+=($!)

echo
log "admin UI: http://localhost:$ADMIN_PORT  (user admin, password admin)"
log "apps:     http://localhost:$API_PORT  http://localhost:$WEB_PORT"
log "v2 of demo/api is published in 30s, a corrupt artifact of demo/web in 60s"
log "press Ctrl-C to stop"
echo

(cd "$WORKDIR" && exec ./runner) &
RUNNER_PID=$!
# Ctrl-C makes runner stop its apps and exit; wait returns early whenever a
# trapped signal arrives, so wait until runner is gone
trap 'kill -TERM "$RUNNER_PID" 2>/dev/null || true' INT TERM
while kill -0 "$RUNNER_PID" 2>/dev/null; do
	wait "$RUNNER_PID" 2>/dev/null || true
done
