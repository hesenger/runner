#!/usr/bin/env bash
# End-to-end test: runner deploys v1 of a stub app from a fake GitHub API,
# then switches to v2 under constant load, and must not drop a single request.
#
# Usage: bash e2e/run.sh   (from any directory)
# Env:   E2E_PORT (default 18000)  external port runner listens on
#        E2E_GITHUB_PORT (default 18100)  port of the fake GitHub API
#        E2E_DRAIN_SECONDS (default 2)  drainDelay written to config.json
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXTERNAL_PORT="${E2E_PORT:-18000}"
GITHUB_PORT="${E2E_GITHUB_PORT:-18100}"
DRAIN_SECONDS="${E2E_DRAIN_SECONDS:-2}"
V1_ID=101
V2_ID=102

TMP_ROOT="${TMPDIR:-/tmp}"
WORKDIR="$(mktemp -d "${TMP_ROOT%/}/runner-e2e.XXXXXX")"
PIDS=()
RUNNER_PID=""

log() { printf '[e2e] %s\n' "$*"; }

cleanup() {
	local status=$?
	touch "$WORKDIR/stop-load" 2>/dev/null || true
	# runner never reaps or kills its children on exit, so stop them first
	if [[ -n "$RUNNER_PID" ]]; then
		pkill -9 -P "$RUNNER_PID" 2>/dev/null || true
	fi
	for pid in "${PIDS[@]+"${PIDS[@]}"}"; do
		kill -9 "$pid" 2>/dev/null || true
	done
	# stub apps whose pid was seen in a response (covers apps orphaned by runner)
	if [[ -f "$WORKDIR/app-pids" ]]; then
		while read -r pid; do kill -9 "$pid" 2>/dev/null || true; done < "$WORKDIR/app-pids"
	fi
	wait 2>/dev/null || true
	rm -rf "$WORKDIR"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# make_zip <zip> <file>: store <file> at the zip root, like a GitHub Actions artifact.
# `zip` is not installed on every Linux image, so fall back to Python's zipfile CLI.
make_zip() {
	if command -v zip >/dev/null 2>&1; then
		zip -q -j "$1" "$2"
	else
		python3 -m zipfile -c "$1" "$2"
	fi
}

# body <url>: response body, or empty on any failure
body() { curl -fsS --max-time 2 "$1" 2>/dev/null || true; }

# wait_for_body <url> <substring> <timeout-seconds>
wait_for_body() {
	local deadline=$((SECONDS + $3)) out
	while ((SECONDS < deadline)); do
		out="$(body "$1")"
		if [[ "$out" == *"$2"* ]]; then
			printf '%s\n' "$out"
			return 0
		fi
		sleep 0.2
	done
	return 1
}

port_open() { curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$1/health"; }

FAILURES=0
pass() { printf 'PASS  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; FAILURES=$((FAILURES + 1)); }

dump_logs() {
	echo "---- runner.log (last 40 lines) ----"
	tail -n 40 "$WORKDIR/runner.log" 2>/dev/null || true
	echo "---- fakegithub.log (last 10 lines) ----"
	tail -n 10 "$WORKDIR/fakegithub.log" 2>/dev/null || true
}

abort() {
	fail "$*"
	dump_logs
	exit 1
}

# ---------------------------------------------------------------- build
log "workdir $WORKDIR"
log "building runner, fakegithub, stubapp v1 and v2"
mkdir -p "$WORKDIR/artifacts" "$WORKDIR/build/v1" "$WORKDIR/build/v2"
(
	cd "$REPO_ROOT"
	go build -o "$WORKDIR/runner" .
	go build -o "$WORKDIR/fakegithub" ./e2e/fakegithub
	go build -ldflags "-X main.version=v1" -o "$WORKDIR/build/v1/stubapp" ./e2e/stubapp
	go build -ldflags "-X main.version=v2" -o "$WORKDIR/build/v2/stubapp" ./e2e/stubapp
)
make_zip "$WORKDIR/artifacts/stubapp-v1.zip" "$WORKDIR/build/v1/stubapp"
make_zip "$WORKDIR/artifacts/stubapp-v2.zip" "$WORKDIR/build/v2/stubapp"

for port in "$EXTERNAL_PORT" "$GITHUB_PORT"; do
	if port_open "$port"; then
		abort "port $port is already in use (set E2E_PORT / E2E_GITHUB_PORT)"
	fi
done

# ---------------------------------------------------------------- start
GITHUB="http://127.0.0.1:$GITHUB_PORT"
"$WORKDIR/fakegithub" --port "$GITHUB_PORT" --dir "$WORKDIR/artifacts" > "$WORKDIR/fakegithub.log" 2>&1 &
PIDS+=($!)

deadline=$((SECONDS + 10))
until curl -fsS -o /dev/null "$GITHUB/repos/test/stubapp/actions/artifacts" 2>/dev/null; do
	((SECONDS < deadline)) || abort "fakegithub did not start"
	sleep 0.1
done

publish() {
	curl -fsS -X POST "$GITHUB/publish?file=$1&id=$2&name=stubapp" > /dev/null
}
publish stubapp-v1.zip "$V1_ID"
log "published v1 (artifact $V1_ID)"

cat > "$WORKDIR/config.json" <<JSON
{
  "githubApi": "$GITHUB",
  "pollInterval": "1s",
  "healthCheckInterval": "500ms",
  "drainDelay": "${DRAIN_SECONDS}s",
  "apps": [
    {
      "externalPort": $EXTERNAL_PORT,
      "repo": "test/stubapp",
      "binName": "stubapp",
      "token": "dummy-token"
    }
  ]
}
JSON

(cd "$WORKDIR" && exec ./runner) > "$WORKDIR/runner.log" 2>&1 &
RUNNER_PID=$!
PIDS+=("$RUNNER_PID")
log "runner started (pid $RUNNER_PID) on :$EXTERNAL_PORT"

URL="http://127.0.0.1:$EXTERNAL_PORT/"

# ---------------------------------------------------------------- v1
V1_BODY="$(wait_for_body "$URL" "stubapp v1" 30)" || abort "v1 was not served on :$EXTERNAL_PORT within 30s"
V1_PORT="$(sed -n 's/.*port=\([0-9]*\).*/\1/p' <<< "$V1_BODY")"
sed -n 's/.*pid=\([0-9]*\).*/\1/p' <<< "$V1_BODY" >> "$WORKDIR/app-pids"
pass "v1 served through proxy: $V1_BODY"

# ---------------------------------------------------------------- load
# Two request loops: a tight one, and one holding each request open for 1s so
# there is always a request in flight to the old version when traffic switches
# (it only succeeds if runner waits drainDelay before killing the old process).
# Every non-200, including connection failures, is logged.
: > "$WORKDIR/load-ok"
: > "$WORKDIR/load-failed"
load_loop() {
	local code
	while [[ ! -f "$WORKDIR/stop-load" ]]; do
		code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$1" || true)"
		if [[ "$code" == "200" ]]; then
			echo . >> "$WORKDIR/load-ok"
		else
			echo "$(date +%T) $1 status=${code:-none}" >> "$WORKDIR/load-failed"
		fi
	done
}
load_loop "$URL" &
LOAD_PIDS=($!)
load_loop "${URL}?delay=1s" &
LOAD_PIDS+=($!)
PIDS+=("${LOAD_PIDS[@]}")
log "load loops started"
sleep 1

# ---------------------------------------------------------------- v2
publish stubapp-v2.zip "$V2_ID"
log "published v2 (artifact $V2_ID)"
V2_BODY="$(wait_for_body "$URL" "stubapp v2" 30)" || {
	touch "$WORKDIR/stop-load"
	abort "v2 was not served on :$EXTERNAL_PORT within 30s"
}
sed -n 's/.*pid=\([0-9]*\).*/\1/p' <<< "$V2_BODY" >> "$WORKDIR/app-pids"
pass "v2 served through proxy: $V2_BODY"

# keep the load going until the old version has been drained and killed
sleep $((DRAIN_SECONDS + 2))
touch "$WORKDIR/stop-load"
wait "${LOAD_PIDS[@]}" || true

# ---------------------------------------------------------------- assertions
OK_COUNT="$(wc -l < "$WORKDIR/load-ok" | tr -d ' ')"
FAILED_COUNT="$(wc -l < "$WORKDIR/load-failed" | tr -d ' ')"

if ((OK_COUNT == 0)); then
	fail "load loop made no successful requests"
elif ((FAILED_COUNT == 0)); then
	pass "zero failed requests during the switch ($OK_COUNT requests)"
else
	fail "$FAILED_COUNT of $((OK_COUNT + FAILED_COUNT)) requests failed during the switch"
	head -n 5 "$WORKDIR/load-failed"
fi

# eventually-checks: allow a few seconds for runner to finish cleaning up
eventually() {
	local deadline=$((SECONDS + 5))
	until "$@"; do
		((SECONDS < deadline)) || return 1
		sleep 0.2
	done
}

if [[ -z "$V1_PORT" ]]; then
	fail "could not parse v1 port from '$V1_BODY'"
elif eventually bash -c "! curl -s -o /dev/null --max-time 1 http://127.0.0.1:$V1_PORT/health"; then
	pass "v1 internal port $V1_PORT no longer accepts connections"
else
	fail "v1 is still listening on internal port $V1_PORT"
fi

bin_dirs() { find "$WORKDIR/bin" -mindepth 1 -maxdepth 1 -type d -name 'stubapp-*' | sort; }
only_v2_dir() { [[ "$(bin_dirs)" == "$WORKDIR/bin/stubapp-$V2_ID" ]]; }
if eventually only_v2_dir; then
	pass "exactly one version dir under bin/: stubapp-$V2_ID"
else
	fail "expected only bin/stubapp-$V2_ID, found: $(bin_dirs | xargs -n1 basename 2>/dev/null | tr '\n' ' ')"
fi

if kill -0 "$RUNNER_PID" 2>/dev/null; then
	pass "runner is still running"
else
	fail "runner exited"
fi

echo
if ((FAILURES > 0)); then
	echo "E2E FAILED ($FAILURES check(s))"
	dump_logs
	exit 1
fi
echo "E2E PASSED"
