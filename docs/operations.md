# open-artifact — Operations Manual

How to run and configure open-artifact in practice. This is a **living
document** — grow it as operational surface lands. For the design and the
invariants behind these knobs, see [`architecture.md`](architecture.md).

Items marked _(planned)_ have a flag or endpoint reserved today but their full
behavior is delivered by a later issue.

## Binary and commands

A single binary, `open-artifact`, with two long-running commands:

| Command              | Plane          | Default port |
| -------------------- | -------------- | ------------ |
| `open-artifact serve`        | data plane     | `8080`       |
| `open-artifact admin serve`  | control plane  | `8081`       |

Plus:

- `open-artifact --version` — prints version, commit, and `GOOS/GOARCH`. Commit
  comes from `-ldflags` at release time and falls back to the embedded VCS
  revision for dev builds.

Both servers run until they receive `SIGINT` or `SIGTERM`, then shut down
gracefully: they stop accepting new connections and drain in-flight requests
within a bounded window (15s) before exiting.

## Configuration model

- **Flags and environment variables only — no config files.**
- Every flag has a matching env var: prefix `OPEN_ARTIFACT`, the flag name
  uppercased with dashes turned into underscores. For example `--bucket-url` →
  `OPEN_ARTIFACT_BUCKET_URL`.
- **Precedence:** command-line flag > environment variable > built-in default.
- The platform `PORT` variable is also bound to `--port`. When both are set,
  `OPEN_ARTIFACT_PORT` wins over `PORT`.
- Configuration is validated at startup; all problems are reported together as
  one joined error, and the process exits non-zero without starting.

## Shared flags (both planes)

| Flag                | Env var                        | Default                    | Notes |
| ------------------- | ------------------------------ | -------------------------- | ----- |
| `--port`            | `OPEN_ARTIFACT_PORT` / `PORT`  | `8080` serve, `8081` admin | 1–65535. |
| `--bucket-url`      | `OPEN_ARTIFACT_BUCKET_URL`     | — (required)               | gocloud.dev/blob URL; see backends below. |
| `--bucket-prefix`   | `OPEN_ARTIFACT_BUCKET_PREFIX`  | _(empty)_                  | Scopes one deployment within a shared bucket; see rules below. |
| `--enable-metrics`  | `OPEN_ARTIFACT_ENABLE_METRICS` | `true`                     | _(endpoint behavior planned, #16)_ |
| `--metrics-path`    | `OPEN_ARTIFACT_METRICS_PATH`   | `/metrics`                 | Must start with `/`. _(planned, #16)_ |
| `--log-level`       | `OPEN_ARTIFACT_LOG_LEVEL`      | `info`                     | `debug`, `info`, `warn`, `error`. |
| `--log-format`      | `OPEN_ARTIFACT_LOG_FORMAT`     | `text`                     | `text` or `json`. |
| `--log-debug`       | `OPEN_ARTIFACT_LOG_DEBUG`      | `false`                    | Adds caller/source location to records. |

## Data-plane-only flags (`serve`)

These exist on `serve` only. They are **stubbed** today and fully consumed by
later issues (auth: #7; format serving: #19–#25).

| Flag                    | Env var                             | Default | Notes |
| ----------------------- | ----------------------------------- | ------- | ----- |
| `--repo-type`           | `OPEN_ARTIFACT_REPO_TYPE`           | _(empty)_ | Allowed: `pypi`, `npm`, `maven` (internal `echo` reserved for OIDC CI). |
| `--disable-authn`       | `OPEN_ARTIFACT_DISABLE_AUTHN`       | `false` | Disables authentication; intended to log a warning. _(planned, #7)_ |
| `--authn-kind`          | `OPEN_ARTIFACT_AUTHN_KIND`          | `oidc`  | Only `oidc` is supported. |
| `--authn-oidc-issuers`  | `OPEN_ARTIFACT_AUTHN_OIDC_ISSUERS`  | _(empty)_ | Comma-separated issuer URLs. |
| `--authn-oidc-audience` | `OPEN_ARTIFACT_AUTHN_OIDC_AUDIENCE` | _(empty)_ | Expected token audience. |

## Storage backends

`--bucket-url` is passed to `gocloud.dev/blob`. The following drivers are
registered:

| Scheme      | Backend            | Example URL |
| ----------- | ------------------ | ----------- |
| `mem://`    | in-memory          | `mem://` |
| `file://`   | local filesystem   | `file:///var/lib/open-artifact` |
| `s3://`     | AWS S3             | `s3://my-bucket?region=us-east-1` |
| `gs://`     | Google Cloud Storage | `gs://my-bucket` |
| `azblob://` | Azure Blob Storage | `azblob://my-container` |

Driver-specific options (credentials, region, endpoints) follow each driver's
URL parameters and standard cloud credential discovery. `mem://` and `file://`
do not support signed URLs, so downloads stream through the server rather than
redirecting.

### `--bucket-prefix` rules

Optional. Scopes one deployment under a shared bucket; on-bucket objects then
live under `open-artifact/v1/<prefix>/...`. It must be a clean, relative,
slash-separated path. Rejected: absolute paths, `..`, empty segments (leading,
trailing, or doubled slashes), and any segment beginning with `.`.

## Logging

Structured logs via `log/slog`, written to stderr.

- Format and verbosity are controlled by `--log-format` / `--log-level`;
  `--log-debug` adds source locations.
- The logger is created once at startup and carried on the request context; an
  HTTP middleware places it on every request's context.
- Stable field keys are used across components: `component`, `namespace`,
  `format`, `op`, `path`, `method`, `status`, `duration`, `error`.
- Credentials are never logged (the `Authorization` header is never emitted).

## Health and metrics _(planned, #16)_

Liveness (`/healthz`), readiness (`/readyz`), and the metrics endpoint
(`--metrics-path`, default `/metrics`) are reserved here via flags; their
handlers and probe behavior are delivered by the observability work.

## Deployment notes

- The admin plane has **no built-in authentication**. Deploy `admin serve`
  behind network/platform access controls. _(behavior planned, #6)_
- Run one format per `serve` process; run separate processes for separate
  endpoints.
