# sync2connect

Syncs health data between providers — currently **Withings → Garmin Connect**
body composition (weight, body fat %, hydration, bone mass, muscle mass, BMI).

The provider layer is modular: adding another source or destination (Fitbit,
Polar, ...) means implementing one small Go interface and registering it —
see [Adding a provider](#adding-a-provider).

## How it works

- **Withings** is read through the official
  [public health data API](https://developer.withings.com) with OAuth2. You
  need a (free) developer application for the `client_id`/`client_secret`.
- **Garmin Connect** has no public write API — the official
  [Health API](https://developer.garmin.com/gc-developer-program/health-api/)
  is read-only and partner-gated. Like the well-known `withings-sync` and
  `garth` projects, sync2connect signs in with your Garmin account the same
  way the Garmin Connect mobile app does, and uploads measurements as FIT
  weight files. The resulting OAuth token is valid for about a year, so the
  login happens once.
- Sync state (OAuth tokens, last-sync watermark) lives in a single state
  directory, so repeated runs only transfer new or updated measurements.

## Quick start (binary)

Download a binary for Linux, macOS or Windows from the
[releases page](https://github.com/matthieudolci/sync2connect/releases)
(every CI run also publishes artifacts), then:

```sh
cp config.example.yaml config.yaml   # edit it: credentials, interval, routes

# one-time authentication for each provider
./sync2connect auth withings   # opens an OAuth flow, local callback on :8484
./sync2connect auth garmin     # asks for email/password (+ MFA code if enabled)

# sync
./sync2connect sync --once     # single pass
./sync2connect sync            # keeps running when sync.interval is set
```

On a headless machine use `./sync2connect auth withings --manual`: it prints
the authorization URL and lets you paste the redirect URL back instead of
starting a local callback server.

## Quick start (Docker)

Images are published to GitHub Container Registry for `linux/amd64` and
`linux/arm64`:

```sh
mkdir -p data
cp config.example.yaml data/config.yaml   # edit it

# one-time authentication (interactive)
docker run --rm -it -v ./data:/data --env-file .env \
  ghcr.io/matthieudolci/sync2connect:latest auth withings --manual
docker run --rm -it -v ./data:/data --env-file .env \
  ghcr.io/matthieudolci/sync2connect:latest auth garmin

# run the sync loop
docker compose up -d
```

See [docker-compose.yml](docker-compose.yml). The container expects the
config at `/data/config.yaml` and writes tokens/state to `/data`. It runs as
a non-root user (uid 65532), so make sure the mounted directory is writable:
`chown -R 65532:65532 data` (or run with `--user "$(id -u)"`).

`latest` follows tagged releases; the `main` tag follows the main branch.

## Configuration

```yaml
state_dir: /data          # tokens + sync state (env: SYNC2CONNECT_STATE_DIR)

sync:
  interval: 1h            # omit for a single pass (e.g. when using cron)
  initial_lookback: 720h  # how far back the very first sync reaches
  routes:
    - source: withings
      destination: garmin

providers:
  withings:
    client_id: ${WITHINGS_CLIENT_ID}        # ${VAR} is expanded from the
    client_secret: ${WITHINGS_CLIENT_SECRET} # environment in any value
    # redirect_uri: http://localhost:8484/callback
  garmin:
    email: ${GARMIN_EMAIL}        # only needed for `auth garmin`
    password: ${GARMIN_PASSWORD}
```

The config file is found via `-config`, `$SYNC2CONNECT_CONFIG`, or
`./config.yaml`, in that order. See
[config.example.yaml](config.example.yaml) for all options.

### Withings application

1. Create an app at <https://developer.withings.com> (a normal Withings
   account works).
2. Set the callback URL to `http://localhost:8484/callback` (or whatever you
   configure as `redirect_uri`).
3. Put the client id/secret in the config and run
   `sync2connect auth withings`.

## CLI reference

```
sync2connect [-config path] auth <provider> [--manual]
sync2connect [-config path] sync [--once]
sync2connect version
```

## Adding a provider

Providers live in `internal/provider/<name>` and register themselves:

```go
func init() {
    provider.Register("fitbit", New)
}

func New(cfg provider.Config) (provider.Provider, error) { ... }
```

Implement `provider.Source` (fetch measurements changed since a timestamp),
`provider.Destination` (receive measurements), or both, plus optionally
`provider.Authenticator` for an interactive `auth` command. Add a blank
import in `cmd/sync2connect/main.go`, and the provider can be referenced
from `sync.routes` in the config. The shared measurement type is
`model.BodyMeasurement`.

## Development

```sh
make test     # go test -race
make lint     # go vet + gofmt
make build    # local binary
make cross    # dist/ binaries for linux, macOS, windows
make docker   # local image
```

CI builds and uploads per-platform artifacts for every push and pull
request. Pushing a `v*` tag creates a GitHub release with archives and
checksums and publishes the Docker image (`docker.yml` workflow) to
`ghcr.io/matthieudolci/sync2connect`.

## Disclaimer

This project is not affiliated with Withings or Garmin. The Garmin upload
uses the same private API as the official mobile app; it can break if Garmin
changes that API, and you use it at your own risk.
