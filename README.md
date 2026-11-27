# metrics_module

CPU, memory, network traffic and block I/O history for the
[modularnost](https://github.com/modularnost/panel) panel: sampled from the
core, kept in SQLite, sent as charts the panel draws. Permission: `stats:read`.

It adds a **Metrics** entry to the menu and a **Metrics** tab to every
service's page. Both are pages of blocks (a table, a chart per number, a range
to pick) that the panel draws in its own look, theme and phone layout; the
module sends numbers and units, not HTML. Open pages refresh after each sample.
When the panel reports which pages have viewers, Metrics sends change
notifications only for those pages. Older panels still receive both
notifications after each sample.

Needs a panel that reads a `contract` 3 manifest, the one without frames. A panel
of contract 2 shows the module as failed, and `0.3.0` of the module is the last
one for it.

## Install

`ghcr.io/modularnost/metrics-module` — `latest` on a release tag, `edge` on
main, `sha-<commit>` pinned.

```sh
docker volume create metrics_data

docker service create --name metrics_module   --network panelnet   --label panel.module=true   --label panel.module.name=metrics   --label panel.module.version=0.4.0   --mount type=volume,source=metrics_data,target=/data   --constraint node.role==manager   --env PANEL_CORE_URL=http://panel:8080   ghcr.io/modularnost/metrics-module:latest
```

The volume holds the history, so it must stay on one node — pin a hostname if
the cluster has several managers.

The Metrics process runs as UID and GID `10001`. The entrypoint updates the
ownership of `/data` and the default SQLite files before dropping privileges,
so named volumes from earlier images can be reused. For a custom `DB_PATH`
or a service with an explicit user, give that user write access to the
database, its sidecars, and its parent directory before upgrading.

## Environment

| | Default | |
| --- | --- | --- |
| `PANEL_CORE_URL` | `http://panel:8080` | |
| `PANEL_MODULE_NAME` | `metrics` | must match the `panel.module.name` label |
| `DB_PATH` | `/data/metrics.db` | |
| `SAMPLE_INTERVAL` | `30s` | one `/internal/stats` call costs ~1s |
| `RETENTION` | `168h` | older rows are deleted on every write |
| `ADDR` | `:8080` | |

## Token

The core sends the token with every `/manifest` fetch. The module checks a new
token with the core before using it. This also refreshes the token when the
core restarts with a new `PANEL_MODULE_SECRET`.

## What it keeps

The module accepts stats responses up to 16 MiB. A larger response is logged
and the sample is skipped.

One row per service per tick: CPU, memory and cumulative traffic/I/O summed
across replicas, memory limit per replica, replica count. Traffic and I/O
graphs calculate bytes/s from adjacent samples. At 30s that is ~2900 rows per
service per day. Charts bucket the range into <=180 points in SQL.

No per-replica or per-node history, no alerting, no downsampling into hourly
rows — add that when a retention window longer than a week is wanted.

## Build

`go test ./... && docker build -t metrics-module:latest .`
