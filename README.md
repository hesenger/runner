# Runner

[![CI](https://github.com/hesenger/runner/actions/workflows/ci.yml/badge.svg)](https://github.com/hesenger/runner/actions/workflows/ci.yml)

Runner is a reverse proxy that keeps your most recent version with zero downtime.

Binaries are downloaded from Github Actions in the repository and spawn a process
in an specific port with --port flag - this is a requirement for the application.

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

Durations use Go syntax, e.g. `"500ms"`, `"2s"`, `"1m"`.

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
     :8010 [Version 1]                     :8011 [Version 2]
     (Active -> Killed)                    (Spawned -> Promoted)
```

## Testing

```sh
go vet ./...
bash e2e/run.sh
```

`e2e/run.sh` runs a full upgrade locally in about 30 seconds; it needs Go, curl and
either `zip` or `python3`, and can be started from any directory. It builds runner,
a fake GitHub artifacts API (`e2e/fakegithub`) and two versions of a stub app
(`e2e/stubapp`), then in a temporary directory:

1. publishes stub v1 and starts runner against the fake API with short timings;
2. waits until v1 is served on the external port (18000) and checks, after a few
   polls, that v1 was downloaded exactly once;
3. publishes newer artifacts runner must ignore (expired, built from another branch,
   with another name) and checks it stays on the same v1 process without downloading
   them;
4. publishes a version whose `/health` never answers and checks runner times out its
   health checks, gives up on it and keeps serving v1;
5. keeps two request loops running against the proxy (one fast, one holding each
   request open for 1s), publishes v2 and waits until v2 is served and drained.

It also checks that v1's binary was extracted to `bin/stubapp-<id>/` once v1 is
served. After the switch it checks that no request failed, that v1's internal port no
longer accepts connections, that v1's directory was deleted while v2's binary is still
in place, that no zips are left in `tmp/`, that v2 was downloaded exactly once, and
that runner is still running.
Each check prints PASS/FAIL; on failure the tail of runner's log is printed and the
script exits non-zero. Ports and drain delay can be overridden with `E2E_PORT`, `E2E_GITHUB_PORT` and `E2E_DRAIN_SECONDS`.

CI (`.github/workflows/ci.yml`) runs `go vet`, `go test -race` and the e2e test on
every push to `main` and every pull request.

## License

MIT, see [LICENSE](LICENSE).
