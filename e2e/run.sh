#!/usr/bin/env bash
# End-to-end test: runner deploys v1 of a stub app from a fake GitHub API,
# downloads it only once, ignores newer artifacts it must not deploy (expired,
# other branch, other name), gives up for good on a version whose /health hangs
# without leaving zombies, then under constant load skips a corrupt zip without
# leftovers and switches to v2 without dropping a single request, and finally
# stops its apps and exits 0 on SIGTERM. A second app whose external port is
# taken must log the listen error without affecting the first one.
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
EXPIRED_ID=102
OTHER_BRANCH_ID=103
OTHER_NAME_ID=104
HANGING_ID=105
CORRUPT_ID=106
V2_ID=107

TMP_ROOT="${TMPDIR:-/tmp}"
WORKDIR="$(mktemp -d "${TMP_ROOT%/}/runner-e2e.XXXXXX")"
PIDS=()
RUNNER_PID=""

log() { printf '[e2e] %s\n' "$*"; }

cleanup() {
	local status=$?
	touch "$WORKDIR/stop-load" 2>/dev/null || true
	# runner stops its children on SIGTERM, but not if it failed or was killed
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

# proc_state <pid>: ps state of <pid> (Z for a zombie), empty once it is gone
proc_state() { ps -o stat= -p "$1" 2>/dev/null | tr -d ' ' || true; }

# zombies: children of runner that exited and were never reaped
zombies() { ps -A -o ppid=,stat= | awk -v p="$RUNNER_PID" '$1 == p && $2 ~ /^Z/' | wc -l | tr -d ' '; }

# eventually-checks: allow a few seconds for runner to finish cleaning up
eventually() {
	local deadline=$((SECONDS + 5))
	until "$@"; do
		((SECONDS < deadline)) || return 1
		sleep 0.2
	done
}

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
mkdir -p "$WORKDIR/artifacts" "$WORKDIR/build/v1" "$WORKDIR/build/v2" "$WORKDIR/build/hanging"
(
	cd "$REPO_ROOT"
	go build -o "$WORKDIR/runner" .
	go build -o "$WORKDIR/fakegithub" ./e2e/fakegithub
	go build -ldflags "-X main.version=v1" -o "$WORKDIR/build/v1/stubapp" ./e2e/stubapp
	go build -ldflags "-X main.version=v2" -o "$WORKDIR/build/v2/stubapp" ./e2e/stubapp
	go build -ldflags "-X main.version=hanging -X main.hangHealth=true" -o "$WORKDIR/build/hanging/stubapp" ./e2e/stubapp
)
make_zip "$WORKDIR/artifacts/stubapp-v1.zip" "$WORKDIR/build/v1/stubapp"
make_zip "$WORKDIR/artifacts/stubapp-v2.zip" "$WORKDIR/build/v2/stubapp"
make_zip "$WORKDIR/artifacts/stubapp-hanging.zip" "$WORKDIR/build/hanging/stubapp"
printf 'not a zip' > "$WORKDIR/artifacts/stubapp-corrupt.zip"

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

# publish <zip> <id> [extra query]: make <zip> the newest artifact
publish() {
	curl -fsS -X POST "$GITHUB/publish?file=$1&id=$2&name=stubapp${3:+&$3}" > /dev/null
}

# downloads <id>: how many times runner downloaded artifact <id>
downloads() { curl -fsS "$GITHUB/stats?id=$1"; }
publish stubapp-v1.zip "$V1_ID"
log "published v1 (artifact $V1_ID)"

cat > "$WORKDIR/config.json" <<JSON
{
  "githubApi": "$GITHUB",
  "pollInterval": "1s",
  "healthCheckInterval": "300ms",
  "healthCheckTimeout": "200ms",
  "drainDelay": "${DRAIN_SECONDS}s",
  "stopTimeout": "2s",
  "apps": [
    {
      "externalPort": $EXTERNAL_PORT,
      "repo": "test/stubapp",
      "binName": "stubapp",
      "branch": "main",
      "artifactName": "stubapp",
      "token": "dummy-token"
    },
    {
      "externalPort": $GITHUB_PORT,
      "repo": "test/taken-port",
      "binName": "stubapp",
      "artifactName": "never-published",
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
V1_PID="$(sed -n 's/.*pid=\([0-9]*\).*/\1/p' <<< "$V1_BODY")"
echo "$V1_PID" >> "$WORKDIR/app-pids"
pass "v1 served through proxy: $V1_BODY"

# list_dir <dir>: entries of <dir> on one line, for failure messages
list_dir() { ls -A "$1" 2>/dev/null | tr '\n' ' ' || true; }

V1_DIR="$WORKDIR/bin/stubapp-$V1_ID"
V2_DIR="$WORKDIR/bin/stubapp-$V2_ID"
if [[ -x "$V1_DIR/stubapp" ]]; then
	pass "v1 binary extracted to bin/stubapp-$V1_ID/stubapp"
else
	fail "v1 binary missing at bin/stubapp-$V1_ID/stubapp; bin/ contains: $(list_dir "$WORKDIR/bin")"
fi

# the second app's external port is fakegithub's, so it must not run
if grep -q "\[test/taken-port\] Failed to listen on external port $GITHUB_PORT" "$WORKDIR/runner.log"; then
	pass "app with a taken external port ($GITHUB_PORT) logged the listen error"
else
	fail "app with a taken external port ($GITHUB_PORT) did not log a listen error"
fi

# polls that find the running artifact again must not download it
sleep 3
V1_DOWNLOADS="$(downloads "$V1_ID")"
if [[ "$V1_DOWNLOADS" == "1" ]]; then
	pass "v1 downloaded exactly once over several polls"
else
	fail "v1 downloaded $V1_DOWNLOADS times over several polls, expected 1"
fi

# ---------------------------------------------------------------- ignored artifacts
# newer artifacts runner must skip: expired, built from another branch, other name
publish stubapp-v2.zip "$EXPIRED_ID" "expired=true"
publish stubapp-v2.zip "$OTHER_BRANCH_ID" "branch=feature"
curl -fsS -X POST "$GITHUB/publish?file=stubapp-v2.zip&id=$OTHER_NAME_ID&name=docs" > /dev/null
log "published expired ($EXPIRED_ID), other-branch ($OTHER_BRANCH_ID) and other-name ($OTHER_NAME_ID) artifacts"
sleep 4

STILL_BODY="$(body "$URL")"
if [[ "$STILL_BODY" == *"stubapp v1 pid=$V1_PID "* ]]; then
	pass "runner stayed on v1 (pid $V1_PID) with expired/other-branch/other-name artifacts published"
else
	fail "runner did not stay on v1 (pid $V1_PID); proxy returned: ${STILL_BODY:-nothing}"
fi

IGNORED_DOWNLOADS="$EXPIRED_ID=$(downloads "$EXPIRED_ID") $OTHER_BRANCH_ID=$(downloads "$OTHER_BRANCH_ID") $OTHER_NAME_ID=$(downloads "$OTHER_NAME_ID")"
if [[ "$IGNORED_DOWNLOADS" == "$EXPIRED_ID=0 $OTHER_BRANCH_ID=0 $OTHER_NAME_ID=0" ]]; then
	pass "ignored artifacts were never downloaded"
else
	fail "ignored artifacts were downloaded (id=count): $IGNORED_DOWNLOADS"
fi

# ---------------------------------------------------------------- hanging health check
# a version whose /health never answers: every attempt must time out, runner
# must give up on it, keep serving v1, and not retry it on later polls.
publish stubapp-hanging.zip "$HANGING_ID"
log "published version with hanging /health ($HANGING_ID)"
gave_up_count() { grep -c "Failed health check" "$WORKDIR/runner.log" || true; }
gave_up() { (($(gave_up_count) >= 1)); }
deadline=$((SECONDS + 20))
until gave_up || ((SECONDS >= deadline)); do sleep 0.2; done
# a couple of polls with the failed version still the newest
sleep 2

HANGING_BODY="$(body "$URL")"
if ! gave_up; then
	fail "runner did not give up on the version with hanging /health within 20s"
elif [[ "$HANGING_BODY" == *"stubapp v1 pid=$V1_PID "* ]]; then
	pass "runner timed out the hanging /health of $HANGING_ID and kept serving v1 (pid $V1_PID)"
else
	fail "runner gave up on $HANGING_ID but v1 (pid $V1_PID) is not served; proxy returned: ${HANGING_BODY:-nothing}"
fi

HANGING_DOWNLOADS="$(downloads "$HANGING_ID")"
if [[ "$HANGING_DOWNLOADS" == "1" && "$(gave_up_count)" == "1" ]]; then
	pass "failed version $HANGING_ID was downloaded and health checked only once over several polls"
else
	fail "failed version $HANGING_ID was downloaded $HANGING_DOWNLOADS times and health checked $(gave_up_count) times, expected 1"
fi

no_zombies() { [[ "$(zombies)" == "0" ]]; }
if eventually no_zombies; then
	pass "no zombie children of runner after giving up on $HANGING_ID"
else
	fail "$(zombies) zombie children of runner after giving up on $HANGING_ID"
fi

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

# ---------------------------------------------------------------- corrupt zip
# an artifact that is not a valid zip: runner must give up on it without
# leaving its zip or a partial extraction behind, and keep serving v1
publish stubapp-corrupt.zip "$CORRUPT_ID"
log "published corrupt zip ($CORRUPT_ID)"
extract_failed() { grep -q "Failed to decompress zip file" "$WORKDIR/runner.log"; }
deadline=$((SECONDS + 10))
until extract_failed || ((SECONDS >= deadline)); do sleep 0.2; done
# a couple of polls with the failed version still the newest
sleep 2

CORRUPT_BODY="$(body "$URL")"
if ! extract_failed; then
	fail "runner did not log an extraction failure for corrupt zip $CORRUPT_ID within 10s"
elif [[ "$CORRUPT_BODY" == *"stubapp v1 pid=$V1_PID "* ]]; then
	pass "runner failed to extract corrupt zip $CORRUPT_ID and kept serving v1 (pid $V1_PID)"
else
	fail "runner failed to extract $CORRUPT_ID but v1 (pid $V1_PID) is not served; proxy returned: ${CORRUPT_BODY:-nothing}"
fi

# runner used to go on and start bin/stubapp from an empty extraction path
if grep -q "failed to apply execute permissions" "$WORKDIR/runner.log"; then
	fail "runner tried to start a process for corrupt zip $CORRUPT_ID"
else
	pass "runner did not try to start a process for corrupt zip $CORRUPT_ID"
fi

CORRUPT_DOWNLOADS="$(downloads "$CORRUPT_ID")"
if [[ "$CORRUPT_DOWNLOADS" == "1" ]]; then
	pass "corrupt zip $CORRUPT_ID downloaded only once over several polls"
else
	fail "corrupt zip $CORRUPT_ID downloaded $CORRUPT_DOWNLOADS times, expected 1"
fi

if [[ -z "$(list_dir "$WORKDIR/tmp")" ]]; then
	pass "nothing left in tmp/ after the failed extraction"
else
	fail "left in tmp/ after the failed extraction: $(list_dir "$WORKDIR/tmp")"
fi

if [[ "$(list_dir "$WORKDIR/bin")" == "stubapp-$V1_ID " ]]; then
	pass "bin/ only holds v1 after the failed versions"
else
	fail "bin/ should only hold stubapp-$V1_ID, contains: $(list_dir "$WORKDIR/bin")"
fi

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

if [[ -z "$V1_PORT" ]]; then
	fail "could not parse v1 port from '$V1_BODY'"
elif eventually bash -c "! curl -s -o /dev/null --max-time 1 http://127.0.0.1:$V1_PORT/health"; then
	pass "v1 internal port $V1_PORT no longer accepts connections"
else
	fail "v1 is still listening on internal port $V1_PORT"
fi

# the old version must get SIGTERM, shut down, and be reaped (not left a zombie)
v1_gone() { [[ -z "$(proc_state "$V1_PID")" ]]; }
if eventually v1_gone; then
	pass "v1 process $V1_PID is gone and was reaped"
else
	fail "v1 process $V1_PID still exists with state $(proc_state "$V1_PID")"
fi

if grep -q "stubapp v1 shutting down" "$WORKDIR/runner.log"; then
	pass "v1 shut down gracefully on SIGTERM"
else
	fail "v1 never logged 'stubapp v1 shutting down'"
fi

# the old version's files must be deleted and the new version's must survive
# (sharing a directory between versions used to delete the new binary)
v1_dir_gone() { [[ ! -e "$V1_DIR" ]]; }
if eventually v1_dir_gone; then
	pass "v1 dir bin/stubapp-$V1_ID was removed"
else
	fail "v1 dir bin/stubapp-$V1_ID still exists, containing: $(list_dir "$V1_DIR")"
fi

if [[ -x "$V2_DIR/stubapp" ]]; then
	pass "v2 binary still present at bin/stubapp-$V2_ID/stubapp"
else
	fail "v2 binary missing at bin/stubapp-$V2_ID/stubapp; bin/ contains: $(list_dir "$WORKDIR/bin")"
fi

# a poll may be mid-download of the running version's zip, so retry briefly
no_zips() { [[ -z "$(find "$WORKDIR/tmp" -name '*.zip' 2>/dev/null)" ]]; }
if eventually no_zips; then
	pass "no leftover zips in tmp/"
else
	fail "leftover zips in tmp/: $(list_dir "$WORKDIR/tmp")"
fi

V2_DOWNLOADS="$(downloads "$V2_ID")"
if [[ "$V2_DOWNLOADS" == "1" ]]; then
	pass "v2 downloaded exactly once"
else
	fail "v2 downloaded $V2_DOWNLOADS times, expected 1"
fi

if kill -0 "$RUNNER_PID" 2>/dev/null; then
	pass "runner is still running"
else
	fail "runner exited"
fi

# ---------------------------------------------------------------- shutdown
# SIGTERM must make runner stop its app processes and exit 0
APP_CHILDREN="$(pgrep -P "$RUNNER_PID" | xargs || true)"
kill -TERM "$RUNNER_PID"
# kill runner if it does not exit in time, so `wait` cannot hang
( sleep 10; kill -KILL "$RUNNER_PID" 2>/dev/null ) &
WATCHDOG_PID=$!
RUNNER_STATUS=0
wait "$RUNNER_PID" || RUNNER_STATUS=$?
kill "$WATCHDOG_PID" 2>/dev/null || true

if ((RUNNER_STATUS == 0)); then
	pass "runner exited 0 on SIGTERM"
else
	fail "runner exited with status $RUNNER_STATUS on SIGTERM, want 0"
fi

children_gone() {
	local pid
	for pid in $APP_CHILDREN; do
		[[ -z "$(proc_state "$pid")" ]] || return 1
	done
}
if [[ -z "$APP_CHILDREN" ]]; then
	fail "runner had no app child processes before SIGTERM"
elif eventually children_gone; then
	pass "runner's app processes ($APP_CHILDREN) stopped on SIGTERM"
else
	fail "runner's app processes still running after SIGTERM: $(ps -o pid=,stat=,command= -p "${APP_CHILDREN// /,}" 2>/dev/null | tr '\n' ';')"
fi

if grep -q "stubapp v2 shutting down" "$WORKDIR/runner.log"; then
	pass "v2 shut down gracefully on SIGTERM to runner"
else
	fail "v2 never logged 'stubapp v2 shutting down'"
fi

echo
if ((FAILURES > 0)); then
	echo "E2E FAILED ($FAILURES check(s))"
	dump_logs
	exit 1
fi
echo "E2E PASSED"
