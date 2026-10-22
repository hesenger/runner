#!/usr/bin/env bash
# End-to-end test: runner deploys v1 of a stub app from a fake GitHub API,
# downloads it only once, ignores newer artifacts it must not deploy (expired,
# other branch, other name), gives up for good on a version whose /health hangs
# without leaving zombies, then under constant load skips a corrupt zip without
# leftovers and switches to v2 without dropping a single request, restarts v2
# when it crashes (backing off while it keeps crashing on start, and on another
# port once its port is taken), deploys v3 instead of restarting a crashed v2,
# and finally stops its apps and exits 0 on SIGTERM. A second app whose external port is
# taken must log the listen error without affecting the first one. Ports that
# used to be internal ports (external port +10 and +11) are held by another
# program, so versions must still deploy on other ports, and a third app that
# never deploys must answer 503 rather than 502. The admin UI must require
# basic auth and report the deployed versions and failed artifacts.
#
# Usage: bash e2e/run.sh   (from any directory)
# Env:   E2E_PORT (default 18000)  external port runner listens on
#        E2E_GITHUB_PORT (default 18100)  port of the fake GitHub API
#        E2E_DRAIN_SECONDS (default 2)  drainDelay written to config.json
#        admin UI on E2E_PORT + 80
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXTERNAL_PORT="${E2E_PORT:-18000}"
GITHUB_PORT="${E2E_GITHUB_PORT:-18100}"
DRAIN_SECONDS="${E2E_DRAIN_SECONDS:-2}"
# external port of an app whose artifacts never match, so it never deploys
IDLE_PORT=$((EXTERNAL_PORT + 50))
ADMIN_PORT=$((EXTERNAL_PORT + 80))
ADMIN_AUTH="admin:e2e-secret"
# runner used to rotate internal ports through external port +10..+20
BLOCKED_PORTS=($((EXTERNAL_PORT + 10)) $((EXTERNAL_PORT + 11)))
V1_ID=101
EXPIRED_ID=102
OTHER_BRANCH_ID=103
OTHER_NAME_ID=104
HANGING_ID=105
CORRUPT_ID=106
V2_ID=107
V3_ID=108

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
mkdir -p "$WORKDIR/artifacts" "$WORKDIR/build/v1" "$WORKDIR/build/v2" "$WORKDIR/build/v3" "$WORKDIR/build/hanging"
(
	cd "$REPO_ROOT"
	go build -o "$WORKDIR/runner" .
	go build -o "$WORKDIR/fakegithub" ./e2e/fakegithub
	go build -o "$WORKDIR/portblocker" ./e2e/portblocker
	go build -ldflags "-X main.version=v1" -o "$WORKDIR/build/v1/stubapp" ./e2e/stubapp
	go build -ldflags "-X main.version=v2 -X main.crashOnStartFile=$WORKDIR/crash-on-start" -o "$WORKDIR/build/v2/stubapp" ./e2e/stubapp
	go build -ldflags "-X main.version=v3" -o "$WORKDIR/build/v3/stubapp" ./e2e/stubapp
	go build -ldflags "-X main.version=hanging -X main.hangHealth=true" -o "$WORKDIR/build/hanging/stubapp" ./e2e/stubapp
)
make_zip "$WORKDIR/artifacts/stubapp-v1.zip" "$WORKDIR/build/v1/stubapp"
make_zip "$WORKDIR/artifacts/stubapp-v2.zip" "$WORKDIR/build/v2/stubapp"
make_zip "$WORKDIR/artifacts/stubapp-v3.zip" "$WORKDIR/build/v3/stubapp"
make_zip "$WORKDIR/artifacts/stubapp-hanging.zip" "$WORKDIR/build/hanging/stubapp"
printf 'not a zip' > "$WORKDIR/artifacts/stubapp-corrupt.zip"

for port in "$EXTERNAL_PORT" "$GITHUB_PORT" "$IDLE_PORT" "$ADMIN_PORT" "${BLOCKED_PORTS[@]}"; do
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

# another program holding ports runner used to pick as internal ports
BLOCKED_LIST="$(IFS=,; echo "${BLOCKED_PORTS[*]}")"
"$WORKDIR/portblocker" --ports "$BLOCKED_LIST" > "$WORKDIR/portblocker.log" 2>&1 &
PIDS+=($!)
for port in "${BLOCKED_PORTS[@]}"; do
	deadline=$((SECONDS + 10))
	until port_open "$port"; do
		((SECONDS < deadline)) || abort "portblocker did not start on :$port"
		sleep 0.1
	done
done
log "portblocker holding ports $BLOCKED_LIST"

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
  "restartBackoff": "300ms",
  "restartBackoffMax": "2s",
  "adminPort": $ADMIN_PORT,
  "adminUser": "${ADMIN_AUTH%%:*}",
  "adminPassword": "${ADMIN_AUTH#*:}",
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
    },
    {
      "externalPort": $IDLE_PORT,
      "repo": "test/idle",
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

# ---------------------------------------------------------------- not deployed yet
# an app without a deployed version must answer 503 with Retry-After; runner
# used to proxy to external port +10, giving a 502 or another program's answer
IDLE_URL="http://127.0.0.1:$IDLE_PORT/"
idle_status() { curl -s -o /dev/null -D "$WORKDIR/idle-headers" -w '%{http_code}' --max-time 2 "$IDLE_URL" || true; }
deadline=$((SECONDS + 10))
IDLE_STATUS="$(idle_status)"
until [[ "$IDLE_STATUS" != "000" ]] || ((SECONDS >= deadline)); do
	sleep 0.1
	IDLE_STATUS="$(idle_status)"
done
if [[ "$IDLE_STATUS" == "503" ]] && grep -qi '^retry-after:' "$WORKDIR/idle-headers"; then
	pass "app with no deployed version answered 503 with Retry-After on :$IDLE_PORT"
else
	fail "app with no deployed version answered $IDLE_STATUS on :$IDLE_PORT, want 503 with Retry-After; headers: $(tr -d '\r' < "$WORKDIR/idle-headers" 2>/dev/null | tr '\n' ';')"
fi

# ---------------------------------------------------------------- v1
V1_BODY="$(wait_for_body "$URL" "stubapp v1" 30)" || abort "v1 was not served on :$EXTERNAL_PORT within 30s"
V1_PORT="$(sed -n 's/.*port=\([0-9]*\).*/\1/p' <<< "$V1_BODY")"
V1_PID="$(sed -n 's/.*pid=\([0-9]*\).*/\1/p' <<< "$V1_BODY")"
echo "$V1_PID" >> "$WORKDIR/app-pids"
pass "v1 served through proxy: $V1_BODY"

# is_blocked <port>: whether <port> is one of the ports held by portblocker
is_blocked() { [[ " ${BLOCKED_PORTS[*]} " == *" $1 "* ]]; }
if [[ -n "$V1_PORT" ]] && ! is_blocked "$V1_PORT"; then
	pass "v1 deployed on internal port $V1_PORT with ports $BLOCKED_LIST held by another program"
else
	fail "v1 internal port '$V1_PORT' is one of the blocked ports $BLOCKED_LIST"
fi

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

# ---------------------------------------------------------------- admin UI
ADMIN="http://127.0.0.1:$ADMIN_PORT"
# admin_status: /status JSON of the admin UI, empty on any failure
admin_status() { curl -fsS --max-time 2 -u "$ADMIN_AUTH" "$ADMIN/status" 2>/dev/null || true; }

ADMIN_NOAUTH="$(curl -s -o /dev/null -D "$WORKDIR/admin-headers" -w '%{http_code}' --max-time 2 "$ADMIN/status" || true)"
if [[ "$ADMIN_NOAUTH" == "401" ]] && grep -qi '^www-authenticate: Basic realm="runner"' "$WORKDIR/admin-headers"; then
	pass "admin UI answered 401 with WWW-Authenticate without credentials"
else
	fail "admin UI answered $ADMIN_NOAUTH without credentials, want 401 with WWW-Authenticate"
fi

# the top-level artifactId (deploy records have the time after it)
V1_STATUS="$(admin_status)"
if [[ "$V1_STATUS" == *"\"state\":\"running\""* && "$V1_STATUS" == *"\"artifactId\":$V1_ID,\"artifactName\":\"stubapp\""* ]]; then
	pass "admin /status reports v1 (artifact $V1_ID) running"
else
	fail "admin /status does not report v1 (artifact $V1_ID) running: ${V1_STATUS:-nothing}"
fi

# the log stream replays the app's output; curl stops it after 2s
V1_LOGS="$(curl -sN --max-time 2 -u "$ADMIN_AUTH" "$ADMIN/logs?app=$EXTERNAL_PORT" || true)"
if [[ "$V1_LOGS" == *"stubapp v1 listening on :$V1_PORT"* ]]; then
	pass "admin /logs replayed the output of v1"
else
	fail "admin /logs did not replay 'stubapp v1 listening on :$V1_PORT': ${V1_LOGS:0:300}"
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
V2_PORT="$(sed -n 's/.*port=\([0-9]*\).*/\1/p' <<< "$V2_BODY")"
V2_PID="$(sed -n 's/.*pid=\([0-9]*\).*/\1/p' <<< "$V2_BODY")"
echo "$V2_PID" >> "$WORKDIR/app-pids"
pass "v2 served through proxy: $V2_BODY"
if [[ -n "$V2_PORT" ]] && ! is_blocked "$V2_PORT"; then
	pass "v2 deployed on internal port $V2_PORT with ports $BLOCKED_LIST held by another program"
else
	fail "v2 internal port '$V2_PORT' is one of the blocked ports $BLOCKED_LIST"
fi

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

V2_STATUS="$(admin_status)"
if [[ "$V2_STATUS" == *"\"artifactId\":$V2_ID,\"artifactName\""* && "$V2_STATUS" == *"\"failedArtifactIds\":[$HANGING_ID,$CORRUPT_ID]"* ]]; then
	pass "admin /status reports v2 (artifact $V2_ID) and failed artifacts $HANGING_ID, $CORRUPT_ID"
else
	fail "admin /status does not report v2 (artifact $V2_ID) and failed $HANGING_ID, $CORRUPT_ID: ${V2_STATUS:-nothing}"
fi

if kill -0 "$RUNNER_PID" 2>/dev/null; then
	pass "runner is still running"
else
	fail "runner exited"
fi

# ---------------------------------------------------------------- crash
# v2 exits on its own under load: runner must restart the same version and
# serve it again within a few seconds. Requests fail only while it is down.
# crash <port>: make the stub app on internal <port> exit 1
crash() { curl -s -o /dev/null --max-time 2 "http://127.0.0.1:$1/crash" || true; }

# wait_for_restart <old pid> <timeout-seconds>: wait until v2 is served by
# another process and print its response
wait_for_restart() {
	local deadline=$((SECONDS + $2)) out
	while ((SECONDS < deadline)); do
		out="$(body "$URL")"
		if [[ "$out" == *"stubapp v2 pid="* && "$out" != *"pid=$1 "* ]]; then
			printf '%s\n' "$out"
			return 0
		fi
		sleep 0.1
	done
	return 1
}

# pid_of <body>, port_of <body>: fields of a stub app response
pid_of() { sed -n 's/.*pid=\([0-9]*\).*/\1/p' <<< "$1"; }
port_of() { sed -n 's/.*port=\([0-9]*\).*/\1/p' <<< "$1"; }

rm -f "$WORKDIR/stop-load"
: > "$WORKDIR/load-ok"
: > "$WORKDIR/load-failed"
load_loop "$URL" &
LOAD_PIDS=($!)
PIDS+=("${LOAD_PIDS[@]}")
sleep 0.5

CRASH_AT=$SECONDS
crash "$V2_PORT"
log "crashed v2 (pid $V2_PID)"
RESTARTED_BODY="$(wait_for_restart "$V2_PID" 10)" || {
	touch "$WORKDIR/stop-load"
	abort "v2 was not served again within 10s after it crashed"
}
RECOVERY_SECONDS=$((SECONDS - CRASH_AT))
FAILED_AT_RECOVERY="$(wc -l < "$WORKDIR/load-failed" | tr -d ' ')"
RESTARTED_PID="$(pid_of "$RESTARTED_BODY")"
echo "$RESTARTED_PID" >> "$WORKDIR/app-pids"
# keep the load going to check nothing fails once it is back
sleep 1
touch "$WORKDIR/stop-load"
wait "${LOAD_PIDS[@]}" || true
OK_COUNT="$(wc -l < "$WORKDIR/load-ok" | tr -d ' ')"
FAILED_COUNT="$(wc -l < "$WORKDIR/load-failed" | tr -d ' ')"

if ((RECOVERY_SECONDS <= 3)); then
	pass "v2 served again by a new process within ${RECOVERY_SECONDS}s of the crash: $RESTARTED_BODY"
else
	fail "v2 took ${RECOVERY_SECONDS}s to be served again after the crash, want at most 3s"
fi

if [[ "$(port_of "$RESTARTED_BODY")" == "$V2_PORT" ]]; then
	pass "v2 restarted on its free internal port $V2_PORT"
else
	fail "v2 restarted on port $(port_of "$RESTARTED_BODY"), want its free internal port $V2_PORT"
fi

if grep -q "Process $V2_PID (artifact $V2_ID) exited unexpectedly, restarting in 300ms" "$WORKDIR/runner.log" &&
	grep -q "Restarted artifact $V2_ID on port $(port_of "$RESTARTED_BODY")" "$WORKDIR/runner.log"; then
	pass "runner logged the unexpected exit of $V2_PID and the restart of artifact $V2_ID"
else
	fail "runner did not log the unexpected exit of $V2_PID and the restart of artifact $V2_ID"
fi

if ((OK_COUNT == 0)); then
	fail "load loop made no successful requests around the crash"
elif ((FAILED_COUNT == FAILED_AT_RECOVERY)); then
	pass "$FAILED_COUNT of $((OK_COUNT + FAILED_COUNT)) requests failed while v2 was down, none after it was back"
else
	fail "$((FAILED_COUNT - FAILED_AT_RECOVERY)) requests failed after v2 was back"
	tail -n 5 "$WORKDIR/load-failed"
fi

# a version that keeps crashing on start: runner must back off between
# restarts, doubling the wait, and serve it again once it starts. Staying up
# longer than restartBackoffMax (2s) resets the backoff to 300ms first.
# Meanwhile another program takes its internal port, so runner must restart it
# on another one (restarts crash on start before binding, so there is no race).
# v2 has been up since before the 1s of load above; its health check passed
# up to healthCheckInterval after it was served, hence 2s more.
sleep 2
touch "$WORKDIR/crash-on-start"
crash "$V2_PORT"
"$WORKDIR/portblocker" --ports "$V2_PORT" >> "$WORKDIR/portblocker.log" 2>&1 &
PIDS+=($!)
log "crashed v2 (pid $RESTARTED_PID) with every restart crashing on start, holding its port $V2_PORT"
restart_attempts() { grep -c "Restarting artifact $V2_ID" "$WORKDIR/runner.log" || true; }
backed_off() { grep -q "Failed to restart artifact $V2_ID: .*, retrying in 1.2s" "$WORKDIR/runner.log"; }
RESTARTS_BEFORE="$(restart_attempts)"
deadline=$((SECONDS + 15))
until backed_off || ((SECONDS >= deadline)); do sleep 0.2; done
CRASH_LOOP_RESTARTS=$(($(restart_attempts) - RESTARTS_BEFORE))
rm -f "$WORKDIR/crash-on-start"

if ! backed_off; then
	fail "runner did not back off to 1.2s within 15s of restarts crashing on start"
elif grep -q "Process $RESTARTED_PID (artifact $V2_ID) exited unexpectedly, restarting in 300ms" "$WORKDIR/runner.log" &&
	grep -q "Failed to restart artifact $V2_ID: .*, retrying in 600ms" "$WORKDIR/runner.log" &&
	((CRASH_LOOP_RESTARTS == 2)); then
	pass "restarts crashing on start backed off 300ms, 600ms, 1.2s ($CRASH_LOOP_RESTARTS restarts)"
else
	fail "restarts crashing on start did not back off 300ms, 600ms, 1.2s ($CRASH_LOOP_RESTARTS restarts):"
	grep -E "exited unexpectedly|retrying in" "$WORKDIR/runner.log" | tail -n 5
fi

RECOVERED_BODY="$(wait_for_restart "$RESTARTED_PID" 10)" || abort "v2 was not served again within 10s after it stopped crashing on start"
RECOVERED_PID="$(pid_of "$RECOVERED_BODY")"
RECOVERED_PORT="$(port_of "$RECOVERED_BODY")"
echo "$RECOVERED_PID" >> "$WORKDIR/app-pids"
pass "v2 served again once it stopped crashing on start: $RECOVERED_BODY"

if [[ "$RECOVERED_PORT" != "$V2_PORT" ]] &&
	grep -q "Port $V2_PORT is taken, restarting on port " "$WORKDIR/runner.log"; then
	pass "v2 restarted on port $RECOVERED_PORT since its port $V2_PORT was taken"
else
	fail "v2 restarted on port $RECOVERED_PORT, want another port than the taken $V2_PORT, logged as taken"
fi

# ---------------------------------------------------------------- newer version while crashed
# a newer artifact published while a restart of the crashed version is
# pending must be deployed, and the crashed version must not be restarted
touch "$WORKDIR/crash-on-start"
crash "$RECOVERED_PORT"
pending() { grep -q "Process $RECOVERED_PID (artifact $V2_ID) exited unexpectedly, restarting in" "$WORKDIR/runner.log"; }
deadline=$((SECONDS + 5))
until pending || ((SECONDS >= deadline)); do sleep 0.1; done
pending || abort "runner did not log the unexpected exit of $RECOVERED_PID within 5s"
publish stubapp-v3.zip "$V3_ID"
log "crashed v2 (pid $RECOVERED_PID) and published v3 (artifact $V3_ID) while its restart is pending"
V3_BODY="$(wait_for_body "$URL" "stubapp v3" 10)" || abort "v3 was not served within 10s while v2 was waiting to restart"
V3_PID="$(pid_of "$V3_BODY")"
echo "$V3_PID" >> "$WORKDIR/app-pids"
RESTARTS_AT_V3="$(grep -c "Restarting artifact" "$WORKDIR/runner.log" || true)"
pass "v3 deployed while v2 was waiting to restart: $V3_BODY"

# runner drains for DRAIN_SECONDS after the switch; the cancelled restart was
# due by then (restartBackoffMax is 2s)
sleep "$DRAIN_SECONDS.5"
rm -f "$WORKDIR/crash-on-start"
STILL_V3_BODY="$(body "$URL")"
RESTARTS_AFTER_V3="$(grep -c "Restarting artifact" "$WORKDIR/runner.log" || true)"
if ((RESTARTS_AFTER_V3 == RESTARTS_AT_V3)) && [[ "$STILL_V3_BODY" == *"stubapp v3 pid=$V3_PID "* ]]; then
	pass "the pending restart of v2 was cancelled; v3 (pid $V3_PID) keeps serving"
else
	fail "runner restarted after v3 was deployed ($((RESTARTS_AFTER_V3 - RESTARTS_AT_V3)) restarts) or v3 (pid $V3_PID) is not served; proxy returned: ${STILL_V3_BODY:-nothing}"
	grep "Restart" "$WORKDIR/runner.log" | tail -n 3
fi

if kill -0 "$RUNNER_PID" 2>/dev/null; then
	pass "runner is still running after the crashes"
else
	fail "runner exited after the crashes"
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

if grep -q "stubapp v3 shutting down" "$WORKDIR/runner.log"; then
	pass "v3 shut down gracefully on SIGTERM to runner"
else
	fail "v3 never logged 'stubapp v3 shutting down'"
fi

echo
if ((FAILURES > 0)); then
	echo "E2E FAILED ($FAILURES check(s))"
	dump_logs
	exit 1
fi
echo "E2E PASSED"
