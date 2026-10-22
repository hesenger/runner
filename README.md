# Runner

[![CI](https://github.com/hesenger/runner/actions/workflows/ci.yml/badge.svg)](https://github.com/hesenger/runner/actions/workflows/ci.yml)

Runner is a reverse proxy that keeps your most recent version with zero downtime.

Binaries are downloaded from Github Actions in the repository and spawn a process
in an specific port with --port flag - this is a requirement for the application.
The port is a free one the OS assigns on `127.0.0.1` for each start. The app must
bind it (on any interface; runner connects through `127.0.0.1`). Another program
could take the port between runner picking it and the app binding it; the health
check then fails and that version is not deployed.

## Configuration

Place a `config.json` file in the root directory:

```json
{
  "apps": [
    {
      "externalPort": 8000,
      "repo": "hesenger/pethello",
      "binName": "pethello",
      "token": "ghp_yourPersonalAccessToken"
    }
  ]
}
```

All top-level fields except `apps` are optional. Intervals and timeouts must be greater
than 0; `drainDelay` may be 0:

| Field                 | Default                  | Description                                                        |
| --------------------- | ------------------------ | ------------------------------------------------------------------ |
| `githubApi`           | `https://api.github.com` | Base URL of the GitHub REST API (pointed at a fake API in tests)    |
| `pollInterval`        | `10s`                    | Wait between checks for a new artifact                              |
| `healthCheckInterval` | `10s`                    | Wait before each of the 5 `GET /health` attempts on a new version   |
| `drainDelay`          | `10s`                    | Time the old version keeps running after traffic is switched away   |
| `apiTimeout`          | `30s`                    | Timeout of each GitHub API request, and of waiting for a download's response headers |
| `downloadTimeout`     | `10m`                    | Timeout of each artifact download, including the zip body           |
| `healthCheckTimeout`  | `5s`                     | Timeout of each `GET /health` attempt                               |
| `stopTimeout`         | `10s`                    | Time an app process gets to exit after `SIGTERM` before it is killed |
| `restartBackoff`      | `1s`                     | Wait before restarting the running version after it exits unexpectedly; doubles after each failed restart |
| `restartBackoffMax`   | `1m`                     | Cap of `restartBackoff`; a version that stays up this long starts over from `restartBackoff` |

Durations use Go syntax, e.g. `"500ms"`, `"2s"`, `"1m"`. `restartBackoffMax` must not
be less than `restartBackoff`.

Runner stops an app process (the old version after draining, or a new version that
fails its health check) with `SIGTERM`, and with `SIGKILL` if it is still running
after `stopTimeout`. When runner itself receives `SIGINT` or `SIGTERM`, it stops all
app processes this way, in parallel, and exits 0.

If the running version exits on its own (a crash, panic or OOM kill), runner logs its
exit status and restarts the same version after `restartBackoff`, on the same internal
port when it is still free (otherwise on a new one from the OS, switching the proxy to
it), and health checks it. Requests fail until it is back up. A restart that fails to start or fails its
health check is retried with the wait doubled each time, up to `restartBackoffMax`, for
as long as it takes: it is the only version known to work, so it is never marked as
failed. A newer artifact is still deployed meanwhile and replaces the crashed version.
Processes that runner stops itself are never restarted.

A version whose artifact fails to extract (for example a corrupt zip or an entry
outside the extraction directory), fails to start or fails its health check is
cleaned up and not deployed again: runner keeps the current version until a newer
artifact is published. Failed artifact IDs are kept in memory, so they are retried
once after runner restarts. A failed download is retried on the next poll. If an
app's `externalPort` cannot be bound, runner logs it and does not run that app;
other apps keep running. Each app needs its own `externalPort`. Until an app's first
version is deployed, its external port answers `503` with a `Retry-After` header.

Each app can also set these optional fields:

| Field          | Description                                                                   |
| -------------- | ----------------------------------------------------------------------------- |
| `branch`       | Only deploy artifacts built from this branch (`workflow_run.head_branch`)     |
| `artifactName` | Only deploy artifacts with this name                                          |

Every poll lists the 30 most recent artifacts and picks the newest one that is not
expired and matches `branch` and `artifactName` when set. It is only downloaded when
its ID differs from the running version. If nothing matches, runner logs it and keeps
the current version until the next poll. Unknown fields in `config.json` are rejected.

```plaintext
[Public Traffic] ──> :8000 (Runner Proxy)
                               │
            ┌──────────────────┴──────────────────┐
            ▼ (Before Upgrade)                    ▼ (After Upgrade & Health Check)
 127.0.0.1:<free port> [Version 1]     127.0.0.1:<free port> [Version 2]
     (Active -> Killed)                    (Spawned -> Promoted)
```

## Testing

```sh
go vet ./...
bash e2e/run.sh
```

`e2e/run.sh` runs a full upgrade locally in about 25 seconds; it needs Go, curl and
either `zip` or `python3`, and can be started from any directory. It builds runner,
a fake GitHub artifacts API (`e2e/fakegithub`), a program holding ports
(`e2e/portblocker`) and versions of a stub app (`e2e/stubapp`), then in a
temporary directory:

1. publishes stub v1, holds external port +10 and +11 (ports runner used to pick
   for app processes) with `e2e/portblocker`, and starts runner against the fake
   API with short timings, with a second app whose external port is taken (the fake
   API's) and a third app (18050) whose artifacts never match;
2. checks the third app answers `503` with `Retry-After`, waits until v1 is served
   on the external port (18000) on a port not held by portblocker, checks the second
   app logged its listen error and, after a few polls, that v1 was downloaded
   exactly once;
3. publishes newer artifacts runner must ignore (expired, built from another branch,
   with another name) and checks it stays on the same v1 process without downloading
   them;
4. publishes a version whose `/health` never answers and checks runner times out its
   health checks, gives up on it and keeps serving v1, downloads it only once over
   several polls, and does not leave it as a zombie process;
5. keeps two request loops running against the proxy (one fast, one holding each
   request open for 1s), publishes a corrupt zip and checks runner fails to extract
   it without starting it, keeps serving v1, downloads it only once, and leaves
   nothing in `tmp/` and only v1 in `bin/`;
6. publishes v2 and waits until v2 is served, on a port not held by portblocker,
   and drained;
7. sends `SIGTERM` to runner and checks it exits 0 after stopping v2.

It also checks that v1's binary was extracted to `bin/stubapp-<id>/` once v1 is
served. After the switch it checks that no request failed, that v1's internal port no
longer accepts connections, that v1's process shut down on `SIGTERM` and was reaped
(not a zombie), that v1's directory was deleted while v2's binary is still
in place, that no zips are left in `tmp/`, that v2 was downloaded exactly once, and
that runner is still running. After the `SIGTERM` it checks that runner's app
processes are gone and that v2 shut down gracefully.
Each check prints PASS/FAIL; on failure the tail of runner's log is printed and the
script exits non-zero. Ports and drain delay can be overridden with `E2E_PORT`, `E2E_GITHUB_PORT` and `E2E_DRAIN_SECONDS`.

CI (`.github/workflows/ci.yml`) runs `go vet`, `go test -race` and the e2e test on
every push to `main` and every pull request.

## License

MIT, see [LICENSE](LICENSE).
