# metrics_module

CPU and memory history for the
[modularnost](https://github.com/modularnost/panel) panel: sampled from the
core, kept in SQLite, drawn as SVG. Permission: `stats:read`.

## Install

`ghcr.io/modularnost/metrics-module` — `latest` on a release tag, `edge` on
main, `sha-<commit>` pinned.

```sh
docker volume create metrics_data

docker service create --name metrics_module   --network panelnet   --label panel.module=true   --label panel.module.name=metrics   --label panel.module.version=0.1.0   --mount type=volume,source=metrics_data,target=/data   --constraint node.role==manager   --env PANEL_CORE_URL=http://panel:8080   ghcr.io/modularnost/metrics-module:latest
```

The volume holds the history, so it must stay on one node — pin a hostname if
the cluster has several managers.

## Environment

| | Default | |
| --- | --- | --- |
| `PANEL_CORE_URL` | `http://panel:8080` | |
| `DB_PATH` | `/data/metrics.db` | |
| `SAMPLE_INTERVAL` | `30s` | one `/internal/stats` call costs ~1s |
| `RETENTION` | `168h` | older rows are deleted on every write |
| `ADDR` | `:8080` | |

## Token

The core sends it with every `/manifest` fetch, so sampling starts within a
discovery TTL (15s) of either side booting, and survives the core restarting
with a new `PANEL_MODULE_SECRET`. Needs a core new enough to do that.

## What it keeps

One row per service per tick: CPU and memory summed across replicas, the memory
limit per replica, the replica count. At 30s that is ~2900 rows per service per
day. Charts bucket the range into <=180 points in SQL.

No per-replica or per-node history, no alerting, no downsampling into hourly
rows — add that when a retention window longer than a week is wanted.

## Build

`go test ./... && docker build -t metrics-module:latest .`
