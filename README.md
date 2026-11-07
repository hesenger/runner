# Runner

[![CI](https://github.com/hesenger/runner/actions/workflows/ci.yml/badge.svg)](https://github.com/hesenger/runner/actions/workflows/ci.yml)

Runner deploys the latest GitHub Actions build of your app to a single machine and swaps versions behind a reverse proxy without dropping requests.

It is one Go binary with no dependencies outside the standard library.

## Why

You have a small HTTP service and one VM. CI already builds a binary on every push. You want each new build running on the VM a minute later, with no failed requests during the switch, and without running Docker, a container registry or Kubernetes.

Runner pulls instead of being pushed to. It polls the GitHub Actions artifacts API, downloads the new binary, starts it next to the old one, checks `/health`, switches the proxy and stops the old process. CI needs no SSH access to the VM. The VM needs only outbound HTTPS to GitHub.

How it compares to common alternatives:

| | Zero-downtime switch | Needs Docker / registry | How a deploy starts | Hosts | What you maintain |
| --- | --- | --- | --- | --- | --- |
| systemd `Restart=` + copy binary | No: stop, then start | No | CI pushes over SSH | 1 | Unit file, deploy script |
| Kamal | Yes (kamal-proxy) | Yes | CI or laptop pushes over SSH | Many | Dockerfile, registry, `deploy.yml` |
| Caddy/nginx + scripts | Possible, if you script the upstream swap and health check | No | CI pushes over SSH | 1+ | Proxy config, deploy scripts |
| Kubernetes | Yes (rolling update, readiness probes) | Yes | `kubectl` / GitOps | Many | Cluster, manifests, registry |
| Runner | Yes | No | VM polls GitHub | 1 | `config.json` |

Runner is the right fit for one host, few apps, and a binary built by GitHub Actions. If you need multiple hosts, containers, TLS management or autoscaling, use one of the others.

## How it works

```mermaid
sequenceDiagram
    participant R as runner
    participant G as GitHub API
    participant N as new version
    participant P as proxy
    participant O as old version

    loop every pollInterval
        R->>G: GET /repos/{repo}/actions/artifacts
        G-->>R: latest 30 artifacts
    end
    R->>G: download zip of the latest run's matching artifact
    R->>R: extract to bin/name-id
    R->>N: start binName with --port and a free port
    loop up to 5 attempts, every healthCheckInterval
        R->>N: GET /health
        N-->>R: 200 OK
    end
    R->>P: switch target to new port
    P->>N: new requests
    Note over P,O: in-flight requests on old version finish during drainDelay
    R->>O: SIGTERM (SIGKILL after stopTimeout)
    R->>R: delete the old version's directory
```

Each app in `config.json` runs its own loop:

1. **Poll.** Every `pollInterval`, list the 30 most recent artifacts of `repo` and pick the one from the most recently triggered workflow run (highest run ID) that is not expired and matches `branch` and `artifactName` when set. Artifact IDs are not used for this: when runs overlap, an older run that finishes later gets a higher artifact ID.
2. **Skip.** Do nothing if that artifact is the running version, or one that already failed.
3. **Download and extract.** Download the zip to `tmp/<name>-<id>.zip` and extract it to `bin/<name>-<id>/`. Entries outside that directory (Zip Slip) fail the artifact. The zip is always deleted.
4. **Start.** Ask the OS for a free port on `127.0.0.1` and run `bin/<name>-<id>/<binName> --port <port>`. Runner sets the execute bit, so the zip does not need to preserve it. App stdout and stderr are logged with a `[<repo>]` prefix.
5. **Health check.** Wait `healthCheckInterval`, then `GET http://127.0.0.1:<port>/health`. Up to 5 attempts, each bounded by `healthCheckTimeout`. Only `200` passes.
6. **Switch.** Point the proxy on `externalPort` at the new port. The swap is a pointer change under a `sync.RWMutex`; requests already in flight keep their backend.
7. **Drain.** Keep the old version running for `drainDelay` so in-flight requests finish.
8. **Stop old.** Send `SIGTERM` to the old process, `SIGKILL` if it is still running after `stopTimeout`, and delete its `bin/` directory.

If extraction, start or health check fails, runner stops the new process, deletes its directory, keeps serving the current version and never retries that artifact ID. A failed download is retried on the next poll. Until the first version is deployed, `externalPort` answers `503` with `Retry-After: 10`.

### Crash restart with backoff

If the running version exits on its own (crash, panic, OOM kill), runner logs the exit status and restarts the same binary after `restartBackoff`. It reuses the same internal port when it is still free, otherwise it takes a new one and switches the proxy. The restart is health checked like a deploy.

A failed restart is retried with the wait doubled each time, capped at `restartBackoffMax`, for as long as it takes. It is the only version known to work, so it is never marked as failed. A version that stays up for `restartBackoffMax` starts over from `restartBackoff` the next time it crashes. A newer artifact is still deployed while a restart is pending, and it cancels the restart. Requests fail while the app is down.

### Graceful shutdown

On `SIGINT` or `SIGTERM`, runner stops all app processes in parallel (`SIGTERM`, then `SIGKILL` after `stopTimeout`), waits until they are reaped and exits 0. A second signal exits right away. Processes runner stops itself are never restarted.

## Quick start

### 1. Install runner

```sh
go install github.com/hesenger/runner@latest
```

This needs Go 1.26.4 or newer (the `go` line in `go.mod`); the `go` command downloads it if your toolchain is older. Tagged versions also have prebuilt binaries on the [Releases page](https://github.com/hesenger/runner/releases).

### 2. Make your app follow the contract

The app must:

- listen on the port passed as `--port` (runner connects through `127.0.0.1`, so binding `127.0.0.1` is enough);
- answer `GET /health` with `200` once it is ready to serve;
- exit on `SIGTERM`. Shutting down gracefully is optional, since traffic was switched away `drainDelay` earlier.

Apps are ephemeral: runner deletes a version's directory when it replaces it, and starts each version from a fresh copy of the artifact. Keep state in external services (a database, object storage), not next to the binary.

Runner starts the app in its `configDir` when one is set (see [App config directory](#app-config-directory)), otherwise in runner's own directory. Either way the working directory is not where the binary is, so load files bundled in the artifact (templates, static assets) relative to the executable's directory, not to the working directory.

A minimal Go app:

```go
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
)

func main() {
	port := flag.Int("port", 8080, "port to listen on")
	flag.Parse()

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "hello from myapp")
	})

	log.Fatal(http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), nil))
}
```

### 3. Upload the binary as an artifact

Build for the VM's OS and architecture and upload the binary with `actions/upload-artifact`. GitHub zips it; runner extracts it and runs `binName` from the zip root.

```yaml
# .github/workflows/build.yml
name: Build

on:
  push:
    branches: [main]

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - name: Build
        run: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o myapp .

      - uses: actions/upload-artifact@v4
        with:
          name: myapp
          path: myapp
          retention-days: 7
```

### 4. Create a token

Create a [fine-grained personal access token](https://github.com/settings/personal-access-tokens/new) with access to the app's repository only and the repository permission **Actions: Read-only**. Listing and downloading artifacts needs nothing else. A token is required for public repositories too, because GitHub only serves artifact downloads to authenticated requests.

### 5. Configure and run

Create `config.json` in the directory runner runs from:

```json
{
  "apps": [
    {
      "externalPort": 8000,
      "repo": "you/myapp",
      "binName": "myapp",
      "artifactName": "myapp",
      "branch": "main",
      "token": "github_pat_xxx",
      "env": {
        "HOME": "/home/myapp",
        "DATABASE_URL": "postgres://myapp:secret@localhost/myapp"
      }
    }
  ]
}
```

```sh
chmod 600 config.json
runner
```

Runner reads `config.json` from the working directory and creates `bin/` and `tmp/` there. It reads the file again every 3 minutes and applies edits without a restart, see [Reloading config](#reloading-config). Run it under systemd or another supervisor so it starts on boot. Its output for a first deploy and an upgrade (from a run against the e2e fake GitHub API):

```text
[MAIN] Starting app you/myapp:8000
[you/myapp] Downloading artifact myapp (1)
[you/myapp] Decompressed artifact to bin/myapp-1
[you/myapp] Starting process on port 49808
[you/myapp] New version is up on port 49808
[you/myapp] Switched proxy target to port 49808, cleaning up old process
[you/myapp] Downloading artifact myapp (2)
[you/myapp] Decompressed artifact to bin/myapp-2
[you/myapp] Starting process on port 49813
[you/myapp] New version is up on port 49813
[you/myapp] Switched proxy target to port 49813, cleaning up old process
```

After an upload, runner notices the artifact within `pollInterval` and switches no earlier than one `healthCheckInterval` after starting it. With the defaults that is 10 seconds to a little over 3 minutes plus download time, if the first health check passes.

## Configuration

`config.json` has top-level settings, shared by all apps, and a list of `apps`. Only `apps` is required at the top level. Durations are strings in Go syntax, such as `"500ms"`, `"2s"` or `"1m"`. Unknown fields are rejected.

| Field | Default | Meaning |
| --- | --- | --- |
| `githubApi` | `https://api.github.com` | Base URL of the GitHub REST API. The e2e test points it at a fake API. |
| `pollInterval` | `3m` | Wait between checks for a new artifact. Each check is one GitHub API request per app, 20 per hour at the default. |
| `healthCheckInterval` | `10s` | Wait before each of the 5 `GET /health` attempts on a new or restarted version. |
| `healthCheckTimeout` | `5s` | Timeout of each `GET /health` attempt. |
| `drainDelay` | `10s` | Time the old version keeps running after traffic is switched away. May be `0`. |
| `stopTimeout` | `10s` | Time an app process gets to exit after `SIGTERM` before it gets `SIGKILL`. |
| `apiTimeout` | `30s` | Timeout of each GitHub API request, and of waiting for a download's response headers. |
| `downloadTimeout` | `10m` | Timeout of each artifact download, including the zip body. |
| `restartBackoff` | `1s` | Wait before restarting a version that exited unexpectedly. Doubles after each failed restart. |
| `restartBackoffMax` | `1m` | Cap of the restart wait. A version that stays up this long starts over from `restartBackoff`. Must not be less than `restartBackoff`. |
| `configReloadInterval` | `3m` | How often `config.json` is read again; changes are applied when its content changed. Every app's `configDir` is checked at the same time. `0` disables reloading. See [Reloading config](#reloading-config). |
| `adminPort` | `0` | Port of the read-only [monitoring UI](#monitoring-ui). `0` disables it. Must differ from every `externalPort`. |
| `adminAddress` | all interfaces | IP address the monitoring UI listens on, e.g. `127.0.0.1`. |
| `adminUser` | none | Basic auth user of the monitoring UI. Required when `adminPort` is set, unless `adminAddress` is a loopback address and `adminPassword` is empty too. |
| `adminPassword` | none | Basic auth password of the monitoring UI. Required with `adminUser`. |
| `dataDir` | none | Directory where runner keeps its log, each app's output and per-minute metrics, in one file per UTC day. See [Logs and metrics](#logs-and-metrics). Empty keeps nothing on disk. |
| `retentionDays` | `30` | Files in `dataDir` older than this many days are deleted. At least 1. |
| `accessLog` | `false` | Also write one row per request to an access log. Needs `dataDir`. |
| `accessLogRetentionDays` | `7` | Access logs older than this many days are deleted. At least 1. |
| `apps` | none | List of apps to deploy, described below. |

Every duration except `drainDelay` and `configReloadInterval` must be greater than 0. `drainDelay` and `configReloadInterval` must not be negative.

Each entry in `apps`:

| Field | Required | Meaning |
| --- | --- | --- |
| `externalPort` | yes | Port the proxy listens on, on all interfaces. Must be unique across apps. If it cannot be bound, runner logs the error and skips this app; other apps keep running. |
| `repo` | yes | Repository whose artifacts are deployed, as `owner/name`. |
| `binName` | yes | Path of the executable inside the artifact zip. |
| `token` | yes | GitHub token with Actions read access to `repo`. |
| `artifactName` | no | Only deploy artifacts with this name. Empty means any name. |
| `branch` | no | Only deploy artifacts built from this branch (`workflow_run.head_branch`). Empty means any branch. |
| `env` | no | Environment variables of the app's processes, as an object of strings. The app gets only these: nothing from runner's own environment, not even `PATH` or `HOME`. Names must be non-empty and contain no `=` or NUL; values contain no NUL. |
| `configDir` | no | Absolute path of the app's working directory and the home of its config files. A change of their content redeploys the running version without downtime. Must be unique across apps and not inside runner's `bin/` or `tmp/`. If it does not exist when the app starts, runner logs the error and skips this app; other apps keep running. See [App config directory](#app-config-directory). |
| `configIgnore` | no | Files in `configDir` the app writes itself, as patterns relative to `configDir`: `path.Match` syntax (`*.tmp`, `logs/*.log`) matched against the whole relative path, and a trailing `/` (`cache/`) for a directory and everything in it. Ignored files never trigger a redeploy. Needs `configDir`. |

Set `artifactName` and `branch` when the repository uploads other artifacts or builds pull requests, so runner never deploys a test build.

List every variable the app needs in `env`, since it starts with an empty environment. Some runtimes need `HOME`: ASP.NET Core apps, for example, keep their data protection keys under it. Runner never logs `env` values, only the names of variables that changed. They sit in `config.json` next to the GitHub token, so keep it `chmod 600`.

An app with a config directory:

```json
{
  "apps": [
    {
      "externalPort": 8000,
      "repo": "owner/orders-api",
      "binName": "orders-api",
      "artifactName": "orders-api",
      "branch": "main",
      "token": "github_pat_xxx",
      "env": { "HOME": "/home/orders-api" },
      "configDir": "/etc/orders-api",
      "configIgnore": ["cache/", "*.tmp"]
    }
  ]
}
```

## App config directory

`configDir` is where an app's config files live on the host, outside runner's `bin/`, so they survive deploys. Runner starts every process of the app (deploys, restarts, redeploys) with `configDir` as its working directory, so the app finds its config where its framework looks by default. Runner adds nothing to the app's environment: the app gets only its `env`.

| Stack | Where it reads config, relative to the working directory |
| --- | --- |
| .NET | `appsettings.json` and `appsettings.{Environment}.json`; the content root is the working directory. |
| Spring Boot | `application.properties` or `application.yml` in `./` and `./config/`. |
| Node.js | `.env` with dotenv, `./config/` with node-config. |
| Go | Any relative path the app opens resolves against `configDir`. |

The catch: files bundled in the artifact are not in the working directory any more. Load them relative to the executable: `AppContext.BaseDirectory` in .NET, `os.Executable()` in Go, `__dirname` in Node.js, the jar's location in Java.

Runner only needs to read `configDir`. Keep secrets readable by the app's and runner's user only: `chmod 700` on the directory and `chmod 600` on files with secrets.

When the content of a file in `configDir` changes, runner redeploys the running version within one `configReloadInterval`, like an `env` change: see [Reloading config](#reloading-config). Files the app writes itself, such as caches or uploads, must be listed in `configIgnore`; otherwise every write looks like a config change. If `configDir` changes again on the first check after such a redeploy, runner does not redeploy and logs `configDir changed right after a redeploy (<files>); is the app writing there? Add those paths to configIgnore`.

## Reloading config

Every `configReloadInterval` (default `3m`), runner reads `config.json` again. It does nothing while the content is unchanged. A changed file is parsed and validated exactly like at startup. If it cannot be read or is invalid, runner logs the error once for that content and keeps running with the last good config; no app is stopped. Write the file through a rename (`mv config.json.new config.json`) if you do not want a half-written file to be logged as invalid; the next read picks up the full one anyway.

Apps are matched by `externalPort`. Each reload is logged with a summary, such as `[MAIN] Reloaded config.json: added you/other:8001; updated you/myapp:8000 (token, env DATABASE_URL); changed pollInterval`. Values are never logged.

| Change | Applied |
| --- | --- |
| App added | Started like at startup: binds `externalPort`, deploys, shows in the monitoring UI. |
| App removed | Its port is closed, including open connections, its process is stopped (`SIGTERM`, `SIGKILL` after `stopTimeout`), its `bin/` directory deleted and it leaves the monitoring UI. |
| `repo` or `binName` of an app | The app is removed and added again. That app answers nothing while it stops, then `503` until the new version is deployed. Other apps are not affected. |
| `token`, `branch`, `artifactName` | From the app's next poll. The running process keeps running. |
| `env` | The running version is started again next to the running process with the new env, health checked, and the proxy switched, then the old process is drained and stopped, like a deploy: no downtime. If the new process fails its health check, the old one keeps serving with the old env. When no process is running (not deployed yet, or waiting to restart after a crash), runner logs `Env changed (<names>), applying it on the next start` and the next deploy or restart uses the new env. |
| `configDir`, `configIgnore` | Like `env`: the running version is redeployed without downtime, from the new directory's current content. |
| `githubApi`, `pollInterval`, `healthCheckInterval`, `drainDelay`, timeouts, `restartBackoff`, `restartBackoffMax` | From the next operation of each app. A new `pollInterval` counts from the reload. New timeouts rebuild the HTTP clients. |
| `configReloadInterval` | From the next read. Setting it to `0` stops reloading until runner restarts. |
| `adminPort`, `adminAddress`, `adminUser`, `adminPassword` | Not applied: runner logs that they need a runner restart, and the monitoring UI keeps its settings. |
| `dataDir`, `retentionDays`, `accessLog`, `accessLogRetentionDays` | Not applied: runner logs that they need a runner restart. Apps added later use the settings runner started with. |

Each app handles a reload between operations: a deploy, restart or env redeploy in progress finishes with the config it started with, and the change applies right after. Removing or replacing an app waits for that too.

### Changes in configDir

After reading `config.json`, the same tick checks every app's `configDir`:

1. Runner compares the path, size, modification time and mode of every file that is not ignored with the last check. Nothing changed: done.
2. Something changed: runner waits 2 seconds and looks again. Still changing (a file being written): it tries again on the next tick.
3. Settled: runner hashes the content (SHA-256). Same content, e.g. after `touch`: nothing happens.
4. Different content: runner logs `[<repo>] configDir changed (<files>), redeploying` and redeploys the running version like an `env` change, with zero downtime. If the new process fails its health check, the old one keeps serving. When no process is running, runner logs `configDir changed, applying it on the next start`.

An edit is served by a new process within one `configReloadInterval`, plus 2 seconds, plus the health check. The content runner starts with is hashed before the app's first start, so a restart of runner does not redeploy. `configReloadInterval: 0` disables these checks too.

## Monitoring UI

![Monitoring UI showing runner memory, two apps and the live runner log](docs/monitoring-ui.png)

Set `adminPort`, `adminUser` and `adminPassword` to serve a read-only page that shows each app and streams its logs:

```json
{
  "adminPort": 8080,
  "adminUser": "admin",
  "adminPassword": "a-long-unique-password",
  "apps": [ ... ]
}
```

Open `http://<host>:8080/` and log in. The page fetches the status and charts once a minute, a few seconds after each minute's metrics are written, and shows, per app: state (`waiting` for the first deploy, `running`, `crashed` waiting to restart, `restarting`, `failed` when its external port is taken, `stopped`), the artifact being deployed, the running artifact ID, name and branch, its internal port, PID, uptime and restart count, the last and next poll with the last poll error, failed artifact IDs, memory (Linux only), requests in the last full minute and since runner started, the config row for an app with `configDir` (the first 8 hex digits of the content hash and when it last changed, or why `configDir` cannot be read), and the last 20 deploys with result, reason and duration. A redeploy of the running version for a changed `env` or `configDir` shows as a deploy of the same artifact with reason `env` or `config`. Each app is one row, its details on the left and its charts on the right, so the charts take most of the page width. Charts show the last 12 or 24 hours per minute, picked at the top (12 by default, remembered by the browser): requests (with `5xx` responses as red bars), p95 latency and memory (Linux only). Hovering a chart shows the values of that minute in all three. The charts are kept in memory, so they work without `dataDir`. With `dataDir`, runner loads the last 24 hours from the metrics files when it starts, so the charts survive a restart; without it they start over. On Linux, the top right of the page shows the memory of runner itself and the total of runner and every process it started (the apps, their children and versions still draining). A Logs tab streams runner's own log or an app's output; the stream stays open while the Apps tab is shown, so no lines are missed.

To leave the login to something in front of runner, such as a Cloudflare Tunnel with Cloudflare Access or an SSH tunnel, bind the UI to loopback and leave out the user and password. Runner refuses an empty login on any other address:

```json
{
  "adminPort": 9000,
  "adminAddress": "127.0.0.1",
  "apps": [ ... ]
}
```

Every route requires basic auth, when it is configured:

| Route | Returns |
| --- | --- |
| `GET /` | The page, embedded in the binary. |
| `GET /status` | Runner version, start time, the last memory sample of runner and of runner with its apps (`memory`, `null` outside Linux) and the status of every app, as JSON. |
| `GET /metrics?app=<externalPort>` | The app's last 1440 minutes (loaded from `dataDir` on startup when set), oldest first, as JSON `{"points": [{"time", "requests", "status5xx", "p95Ms", "rssKb"}]}`; `rssKb` is `null` when memory is not read. Minutes without a deployed version and without requests are left out. |
| `GET /metrics?app=runner` | The last 1440 minutes of memory, as JSON `{"points": [{"time", "runnerKb", "totalKb"}]}`: runner alone and runner with every process it started. Empty outside Linux. |
| `GET /logs?app=<externalPort>` | Server-Sent Events: the last 1000 lines of that app's output, then new lines as they come. `app=runner` or no `app` streams runner's own log. |

Failed logins are throttled per client address (IPv4 address, or IPv6 /64), taken from the connection only, never from headers such as `X-Forwarded-For`. After 5 failures within 15 minutes, each further failure blocks the address for 1 second, doubling up to 5 minutes, and a blocked address gets `429` with `Retry-After` without its password being checked. Runner tracks at most 10,000 addresses (about 1 MB); beyond that, all other addresses share one throttle, so an attack from many addresses can briefly lock out new visitors too. Behind a proxy, every client has the proxy's address and shares its throttle.

Deploy history and the logs shown are kept in memory only; set `dataDir` to keep logs on disk. A client that cannot keep up is disconnected rather than slowing down the apps; the page reconnects.

The UI is plain HTTP. The password and the logs travel unencrypted, so use a password you use nowhere else, and limit who can reach the port: a firewall, an SSH tunnel, or a TLS proxy in front. The password sits in `config.json` next to the GitHub token: use a fine-grained token with **Actions: Read-only** on that one repository only, and `chmod 600 config.json`.

To try it locally, `bash e2e/demo.sh` builds runner, the stub app and the fake GitHub API, deploys two apps on ports 18000 and 18001 and runs runner in the foreground with the UI on `http://localhost:18080` (user `admin`, password `admin`). After 30 seconds it publishes a new version of the first app, after 60 seconds a broken one of the second, so a successful and a failed deploy show up. Ctrl-C stops everything and deletes the temporary directory. Logs and metrics go to `~/.cache/runner-demo` (or `DEMO_DATA`), which is kept, so the charts show earlier runs after a restart.

## Logs and metrics

Runner sees every request, since it is the proxy, and samples the memory of each running app every 30 seconds. Set `dataDir` to keep both on disk, with the logs, as plain files you can open in a spreadsheet, `grep` or DuckDB:

```
data/
  runner-2026-11-02.log
  memory-2026-11-02.csv       Linux only
  you_myapp-8000/
    output-2026-11-02.log
    metrics-2026-11-02.csv
    access-2026-11-02.csv     only with accessLog
```

Each app has a directory named after its repo and external port. There is one file per UTC day; files older than `retentionDays` (access logs: `accessLogRetentionDays`) are deleted when a new day starts. Log lines start with a UTC timestamp. A day's output or access log stops at 256 MB, with a last line saying so, so a noisy app cannot fill the disk. When a file cannot be written (disk full, permissions), runner logs it once, drops the lines and keeps running; the apps are not slowed down or stopped.

`metrics-<day>.csv` has one row per minute, written on the minute and on shutdown, while a version is deployed or requests come in:

| Column | Meaning |
| --- | --- |
| `time` | Start of the minute, UTC. |
| `artifact_id`, `pid` | The running version and process at the end of the minute, to compare numbers before and after a deploy. |
| `requests` | Requests through the proxy, including the `503`s before the first deploy and `502`s while the app is down. Health checks go to the internal port and are not counted, nor is the monitoring UI. |
| `status_2xx` ... `status_5xx` | Requests by status class. |
| `bytes_out` | Response body bytes sent. |
| `p50_ms`, `p95_ms` | Latency percentiles, as the upper bound of a histogram bucket (5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000 ms): `p95_ms` = `100` means 95% of requests took at most 100 ms. Beyond 10 s it is the slowest request. |
| `max_ms` | Slowest request. |
| `peak_rps` | Most requests started within one second, which an average per minute hides. |
| `clients` | Distinct client addresses in the minute. |
| `clients_today` | Distinct client addresses since the start of the UTC day (or since runner started). The last row of a day is the day's count. Capped at 1,000,000. |
| `rss_max_kb` | Highest resident memory of the running process and its child processes in the minute, read from `/proc`. Empty on other systems than Linux. |

For example, requests per day with DuckDB: `select date_trunc('day', time) as day, sum(requests) from 'data/you_myapp-8000/metrics-*.csv' group by day order by day`.

`access-<day>.csv` has one row per request: `time,method,path,status,ms,bytes,client`. The query string is never written, since it may hold tokens. IDs in the path are replaced so endpoints can be counted: `/items/123` becomes `/items/:id`, and a UUID, 16 or more hex digits, or a segment longer than 64 characters become `:uuid`, `:hex` and `:long`. Client addresses are never written: `client` is a hash of the address with a random key that changes every UTC day and lives only in memory, so it tells requests of one client apart within a day, but cannot be traced back to the address or across days (or across a runner restart).

The client address is the connection's, except for connections from a loopback address, such as a Cloudflare Tunnel (`cloudflared`) or Caddy on the same host: then it is `CF-Connecting-IP`, or else the last `X-Forwarded-For` entry, which the local proxy added. Earlier entries come from the client and are ignored.

`memory-<day>.csv` has one row per minute on Linux: `time,runner_rss_max_kb,total_rss_max_kb`, the highest memory of runner alone and of runner with every process it started (the apps, their children and versions still draining). Processes runner did not start, such as cloudflared, are not counted.

When a process exits, runner also logs its peak memory, e.g. `Process 1234 stopped: signal: terminated, peak memory 48.2 MB`.

Metrics are counted in memory and written once a minute, so a crash of runner loses at most the current minute.

## Testing

```sh
go vet ./...
go test -race ./...
bash e2e/run.sh
```

Unit tests cover config validation, the config reload diff and invalid edits, `configDir` change detection (ignored files, `touch`, files still being written, size limits, the redeploy loop guard) and the redeploy it triggers, adding and removing apps at runtime, the app environment, artifact selection, download status checks and timeouts, extraction (including Zip Slip and cleanup), health check timeouts, process stop and reaping, restart backoff, the proxy, and the monitoring UI's basic auth, login throttling, status JSON, log buffers and log stream.

`e2e/run.sh` runs a full deploy cycle locally in about 60 seconds. It needs Go, curl and either `zip` or `python3`, and runs from any directory. It builds runner and three helpers:

- `e2e/fakegithub`: imitates the artifacts list and download endpoints, with endpoints to publish artifacts and count downloads;
- `e2e/stubapp`: the deployed app, built as several versions, with `/crash`, `/env?key=` to print a variable, `/config` to print `config.txt` of its working directory as read on start, and build flags to hang `/health` or crash on start;
- `e2e/portblocker`: holds ports, to prove versions still deploy when ports are taken.

Then, in a temporary directory, with short timings, it checks that runner:

1. answers `503` with `Retry-After` for an app with no deployed version;
2. deploys v1 to `bin/stubapp-<id>/`, on a port not held by another program, with only the app's `env` (a variable of runner's environment and `PATH` are not set), downloading it exactly once over several polls, and the monitoring UI answers `401` without credentials, `429` after 6 wrong logins, reports v1 in `/status` and replays its output in `/logs`;
3. logs and skips an app whose `externalPort` is taken, without affecting the others;
4. ignores newer artifacts that are expired, from another branch or with another name, without downloading them;
5. gives up on a version whose `/health` hangs, keeps serving v1, never retries it and leaves no zombie process;
6. under load, skips a corrupt zip without starting it, leaving nothing in `tmp/` and only v1 in `bin/`;
7. switches to v2 under two request loops (one fast, one holding each request open for 1s) with **zero failed requests**, then stops v1 with `SIGTERM`, reaps it, frees its port and deletes its directory, and `/status` reports v2 and both failed artifacts;
8. restarts v2 after a crash within 3 seconds, backs off 300ms, 600ms, 1.2s while restarts crash on start, and moves to a new port when the old one is taken;
9. deploys v3 while a v2 restart is pending, and cancels that restart;
10. reloads `config.json` while running: serves an app added at runtime, applies a token and branch change without restarting the process, redeploys on an env change under load with zero failed requests and without logging the values, logs invalid JSON once while both apps keep serving, and closes the port of a removed app and stops its process;
11. runs the app in a `configDir` set at runtime, and redeploys it under load with zero failed requests when `config.txt` there changes;
12. on `SIGTERM`, stops v3 gracefully and exits 0.

Each check prints `PASS` or `FAIL`. On failure the script prints the tail of runner's log and exits non-zero. `E2E_PORT`, `E2E_GITHUB_PORT` and `E2E_DRAIN_SECONDS` override the ports and drain delay; the monitoring UI listens on `E2E_PORT` + 80.

CI (`.github/workflows/ci.yml`) runs on every push to `main` and every pull request, as two parallel jobs: `go vet` plus `go test -race`, and the e2e test.

## Demo

Trimmed output of `bash e2e/run.sh` on macOS (45 checks):

```text
[e2e] portblocker holding ports 18010,18011
[e2e] published v1 (artifact 101)
[e2e] runner started (pid 50628) on :18000
PASS  app with no deployed version answered 503 with Retry-After on :18050
PASS  v1 served through proxy: stubapp v1 pid=50643 port=65239
PASS  v1 binary extracted to bin/stubapp-101/stubapp
PASS  v1 downloaded exactly once over several polls
...
[e2e] published version with hanging /health (105)
PASS  runner timed out the hanging /health of 105 and kept serving v1 (pid 50643)
PASS  no zombie children of runner after giving up on 105
[e2e] load loops started
[e2e] published corrupt zip (106)
PASS  runner failed to extract corrupt zip 106 and kept serving v1 (pid 50643)
[e2e] published v2 (artifact 107)
PASS  v2 served through proxy: stubapp v2 pid=51349 port=65522
PASS  zero failed requests during the switch (600 requests)
PASS  v1 internal port 65239 no longer accepts connections
PASS  v1 process 50643 is gone and was reaped
PASS  v1 shut down gracefully on SIGTERM
...
[e2e] crashed v2 (pid 51349)
PASS  v2 served again by a new process within 0s of the crash: stubapp v2 pid=52325 port=65522
PASS  22 of 161 requests failed while v2 was down, none after it was back
[e2e] crashed v2 (pid 52325) with every restart crashing on start, holding its port 65522
PASS  restarts crashing on start backed off 300ms, 600ms, 1.2s (2 restarts)
PASS  v2 restarted on port 49764 since its port 65522 was taken
[e2e] crashed v2 (pid 52733) and published v3 (artifact 108) while its restart is pending
PASS  v3 deployed while v2 was waiting to restart: stubapp v3 pid=52759 port=49777
PASS  the pending restart of v2 was cancelled; v3 (pid 52759) keeps serving
PASS  runner exited 0 on SIGTERM
PASS  v3 shut down gracefully on SIGTERM to runner

E2E PASSED
```

## Limitations

- **No timeouts between proxy and app, or on the external listener.** A hung app holds client connections open, and slow clients are not cut off. Put a proxy with timeouts in front if that matters.
- **No TLS.** `externalPort` and `adminPort` serve plain HTTP. Terminate TLS in front of runner (Caddy, a load balancer, Cloudflare).
- **Drain is a fixed delay.** Requests still running after `drainDelay` plus `stopTimeout` are cut when the old version is killed.
- **Token in plain text.** The GitHub token sits in `config.json`. Use a fine-grained, read-only token for one repository and restrict the file's permissions.
- **No fallback on a fresh start.** After runner restarts it deploys the matching artifact of the latest run. If that one fails, it does not fall back to an older good version; the app answers `503` until a newer artifact passes.
- **Failed artifact IDs are kept in memory only.** Each failed version is retried once after runner restarts.
- **Only the latest 30 artifacts are checked.** If 30 newer artifacts do not match `branch` or `artifactName`, runner finds nothing to deploy.
- **One process per app.** While the running version is down after a crash, requests fail until the restart passes its health check.
- **Port race.** Runner picks a free port and releases it before the app binds it. If another program takes it first, the health check fails and that version is skipped.
- **Admin and data settings need a restart.** A reload does not apply `adminPort`, `adminAddress`, `adminUser`, `adminPassword`, `dataDir`, `retentionDays`, `accessLog` or `accessLogRetentionDays`.
- **Memory is read on Linux only.** Elsewhere the monitoring UI shows none and `rss_max_kb` is empty.
- **A reload waits for deploys.** Removing or replacing an app waits for its deploy in progress, bounded by `downloadTimeout` and the health checks.
- **An app whose port was taken stays down.** If an added app cannot bind `externalPort`, it is only retried after it is removed and added again, or runner restarts.
- **configDir is polled, not watched.** A change is picked up on the next `configReloadInterval` tick, not right away. Symlinks in it are not followed, so a change behind a symlink is not seen.
- **configDir size.** At most 1,000 files and 10 MB, not counting `configIgnore`. Beyond that runner logs the error, shows it in the monitoring UI and stops detecting changes until it is back under the limits.
- **Shared configDir during a switch.** The old and new process run in the same `configDir` while traffic is switched and the old one drains, so they must not both write the same files there.
- **Single host.** No coordination between machines.
- **Unix only.** Stopping apps relies on `SIGTERM`.

## License

MIT, see [LICENSE](LICENSE).
