[![Build](https://img.shields.io/github/actions/workflow/status/prezi/openai-exporter/build.yml?branch=master&style=flat-square)](https://github.com/prezi/openai-exporter/actions)
[![GitHub Release](https://img.shields.io/github/v/release/prezi/openai-exporter?style=flat-square)](https://github.com/prezi/openai-exporter/releases)

# OpenAI Exporter

Prometheus exporter for OpenAI **token usage** and **daily cost** data, scraped from OpenAI's official `/v1/organization/usage/*` and `/v1/organization/costs` admin APIs.

This is the Prezi-maintained fork of [`foxdalas/openai-exporter`](https://github.com/foxdalas/openai-exporter), with the following changes:

- Cost scrape decoupled from usage scrape; cost is fetched on its own slower interval and the gauge is reset each cycle so stale series drop out.
- Cost lookback window is configurable (default 35 days) so backfill/restatement is captured.
- `OPENAI_ORG_ID` is now optional; sent as `OpenAI-Organization` header only if set.
- Project names are pre-listed at startup and resolved with `singleflight` to avoid request stampedes.
- Optional cardinality controls for `user_id` and `api_key_id` labels.
- `/healthz` and `/readyz` endpoints for Kubernetes probes.
- Operational metrics: `openai_exporter_scrape_errors_total`, `openai_exporter_last_success_timestamp_seconds`, `openai_exporter_scrape_duration_seconds`.
- `vector_stores`, `audio_speeches` and `audio_transcriptions` are no longer polled for *token* usage — they don't return token fields, only bytes/seconds. Cost for those services still appears in `openai_api_daily_cost`.
- Multi-stage Dockerfile builds standalone with `docker build .`.
- Released to **GitHub Container Registry** (`ghcr.io/prezi/openai-exporter`) — not Docker Hub.

## Prerequisites

- Go 1.23 or higher (build only)
- An **OpenAI admin API key** with read access to `/v1/organization/*`. A regular project key is *not* sufficient.

## Configuration

Environment variables:

| Variable | Required | Description |
|---|---|---|
| `OPENAI_SECRET_KEY` | yes | Admin API key (`sk-admin-...`). |
| `OPENAI_ORG_ID` | no | Sent as `OpenAI-Organization` request header. Only needed if your admin key spans multiple orgs. |

Flags:

| Flag | Default | Description |
|---|---|---|
| `--web.listen-address` | `:9185` | Address to listen on. |
| `--web.telemetry-path` | `/metrics` | Path to expose metrics. |
| `--scrape.interval` | `1m` | Usage API scrape interval (also the bucket window). |
| `--cost.interval` | `1h` | Cost API scrape interval. |
| `--cost.lookback` | `840h` (35d) | How far back to fetch daily costs each scrape. |
| `--usage.backfill` | `1h` | On startup, how far back to fetch usage data. |
| `--label.user_id` | `true` | Include `user_id` label on token metrics. Set to `false` to reduce cardinality. |
| `--label.api_key_id` | `true` | Include `api_key_id` and `api_key_name` labels on token metrics. |
| `--openai.org-header` | `""` | Optional `OpenAI-Organization` header (also reads `$OPENAI_ORG_ID`). |
| `--log.level` | `info` | Log verbosity (`debug`, `info`, `warn`, `error`). |
| `--log.format` | `text` | Log format (`text` or `json`). |

## Build & Run

```bash
go build -o openai-exporter .
OPENAI_SECRET_KEY=sk-admin-... ./openai-exporter
```

### Docker

```bash
docker build -t openai-exporter .
docker run -d -p 9185:9185 -e OPENAI_SECRET_KEY=sk-admin-... openai-exporter

# Or pull a release:
docker run -d -p 9185:9185 \
  -e OPENAI_SECRET_KEY=sk-admin-... \
  ghcr.io/prezi/openai-exporter:latest
```

## How it works

### Token usage (`openai_api_tokens_total`)
- Fetches `/v1/organization/usage/{completions,embeddings,moderations,images}` on `--scrape.interval`.
- 1-minute bucket width; counters are emitted only for **completed** buckets and deduplicated by bucket id.
- The first complete bucket appears ~1–2 minutes after startup.
- On startup, usage is backfilled `--usage.backfill` into the past (default 1h).

### Cost (`openai_api_daily_cost`)
- Fetches `/v1/organization/costs` on `--cost.interval` (default 1h) over the last `--cost.lookback` window (default 35 days).
- Daily-bucketed by OpenAI; restated as days finish.
- The gauge is reset before each cost cycle so series for projects that stop spending drop out.

### Project / API key name enrichment
- All projects are pre-listed at startup via `/v1/organization/projects?limit=100`.
- During scrape, unknown ids are resolved on demand and cached. Concurrent lookups for the same id are coalesced via `singleflight`.
- Failures are *not* cached, so transient errors don't permanently mark a project as `unknown`.

## Metrics

### `openai_api_tokens_total` (Counter)

| Label | Description |
|---|---|
| `model` | Model id (e.g. `gpt-4-turbo-2024-04-09`). |
| `operation` | One of `completions`, `embeddings`, `moderations`, `images`. |
| `project_id` | OpenAI project id. |
| `project_name` | Resolved project name, or `unknown`. |
| `batch` | `true` / `false` / `unknown`. |
| `token_type` | `input`, `output`, `input_cached`, `input_audio`, `output_audio`. |
| `user_id` | (optional, see `--label.user_id`) |
| `api_key_id` | (optional, see `--label.api_key_id`) |
| `api_key_name` | (optional, see `--label.api_key_id`) |

### `openai_api_daily_cost` (Gauge)

| Label | Description |
|---|---|
| `date` | `YYYY-MM-DD` (UTC). |
| `project_id` | OpenAI project id. |
| `project_name` | Resolved project name. |
| `line_item` | Cost line item, e.g. `GPT-4 Turbo`. |
| `organization_id` | OpenAI org id. |
| `currency` | e.g. `usd`. |

### Operational metrics

- `openai_exporter_scrape_errors_total{endpoint="..."}` — counter of scrape errors by endpoint.
- `openai_exporter_last_success_timestamp_seconds{kind="usage|cost"}` — gauge with the unix timestamp of the last successful scrape.
- `openai_exporter_scrape_duration_seconds{kind="usage|cost"}` — histogram of scrape durations.

### Example PromQL

```promql
# Total cost in the last 30 days, by project
sum by (project_name) (max_over_time(openai_api_daily_cost[30d]))

# Cost for "today" (UTC)
sum(openai_api_daily_cost{date=~"^.*$"}
    and on() (vector(time()) - 0))   # or use a recording rule

# Token throughput (tokens/sec) by model
sum by (model) (rate(openai_api_tokens_total[5m]))
```

## Contributing

PRs welcome. Run `go test -race ./...`, `gofmt -l .`, and `golangci-lint run` before pushing.

## License

MIT — see [LICENSE](./LICENSE). Originally authored by Maxim Pogozhiy / `foxdalas`.
