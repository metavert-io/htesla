# htesla — maintainer guide

This guide is for anyone changing htesla, people and coding agents alike.
`README.md` describes the ember to its users. This file covers what that
one doesn't: the rules the code keeps, the Hearthloom contract it
depends on, how to test without a car, and how to release.

## Build checks (run all before calling anything done)

```bash
go build ./...
go vet ./...
go test -count=1 ./...        # Go 1.26; prefix GOTOOLCHAIN=local if a newer toolchain gets fetched
./build.sh                    # → dist/ (linux amd64/arm64, darwin arm64/amd64; CGO off, static)
hearthloom ember validate dist   # the daemon's own manifest parser
```

GitHub Actions runs the Go checks, `./build.sh` and govulncheck for every
push to `main` and every pull request: check `gh run list` after pushing.
Build release packages with the newest Go 1.26 patch release (`go
version`, then `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`):
the binaries carry the standard library they were built with, so an old
patch release ships its known vulnerabilities, and CI, which uses the
newest patch, can't see them.

UI changes need a real browser check. Use Playwright driving installed
Chrome (`chromium.launch({ channel: "chrome", headless: true })`):
- load the pages through the dashboard (`#/plugin/htesla/<page>`)
- at widths 360 and 1280, in light and dark
- with no `pageerror`, no horizontal scroll, and no native
  `alert`/`confirm`/`prompt` (they are forbidden)

## Hard rules (each one protects the user's car, money or privacy)

1. **Read-only toward the car.**
   - Request only `openid offline_access vehicle_device_data
     vehicle_location`.
   - Never request `vehicle_cmds` or `vehicle_charging_cmds`, never call
     `wake_up`, never send a signed command.
   - The one signed thing is the telemetry *configuration*, which
     controls nothing in the car.
2. **Never poll billed endpoints.**
   - `vehicle_data` and `nearby_charging_sites` cost money, and Tesla
     bills every response below 500, including a 408 "asleep".
   - Use the free ones: `GET /api/1/vehicles[/{vin}]`, `fleet_status`,
     `fleet_telemetry_config*`, `release_notes`, `service_data`,
     `users/*`.
   - Every Fleet API call is metered into the cost guard
     (`api_calls/<month>`). A new call site must be metered too.
3. **The VIN never leaves htesla.**
   - Records posted to Hearthloom use `builder.VehicleID(vin)` = first 12 hex of
     sha256(VIN). Tests assert that no post contains a VIN; keep them.
   - In logs and UI, show `last4` only.
4. **Never regenerate the app key on its own.**
   - Tesla pairs the car to the public half, so a new key silently
     breaks pairing.
   - A daemon or storage error must never be read as "no key". Only an
     explicit owner click ("make a new key") replaces a lost one.
5. **Seal a refresh token before using its access token.**
   - Tesla refresh tokens are single-use and rotate. The last-used one
     stays valid about 24h; a new one invalidates the old.
   - `tesla.Tokens.adoptLocked` saves first, keeps the pair pending if
     the save fails, and only then exposes the access token. Never
     reorder that.
6. **Secrets live only in the daemon's sealed store.** Today those are
   `tesla_private_key`, `client_secret`, `tesla_refresh_token`,
   `tls_ca_key` and `tls_server_key`.
   - Never put a secret in KV, files, logs, status, test fixtures or
     commit messages.
   - The UI page can set a secret but cannot read one back (the daemon
     enforces that). Keep the page that way: it never reads a secret and
     never calls Tesla.
7. **Embers never write under `~/.hearthloom`.** All persistence goes
   through the daemon's ember API (KV ≤256KB, files ≤32MB, secrets ≤64KB).
8. **The receiver trusts only Tesla's certificate authorities** (the
   vendored bundle).
   - It only accepts VINs on the signed-in account.
   - The development trust (`HTESLA_DEV_CLIENT_CA`, `HTESLA_DEV_VINS`)
     exists only for `htesla-replay`. It must stay loud in the log and
     never be reachable from settings or the UI.
9. **No double posts, no lost records.**
   - The builder's cursor, open drive/charge and outbox are ONE KV
     document (`builder`), saved before anything is posted.
   - Record keys are deterministic (`drive:<vid>:<start unix>`,
     `charge:<vid>:<start unix>`, `version:<vid>:<version>`), so a
     re-post is a no-op.
   - Chunk files for the same minute get `_2`, `_3`… suffixes, so a
     name-ordered cursor can't skip one.
10. **Never mint drive episodes.**
    - Drives go to `POST /v1/ingest/movement` as the CAR's movement;
      Hearthloom's trips pass decides whose drive it was and mints the
      episode.
    - Charges and software updates are htesla's own episodes
      (`POST /v1/ingest`, source `htesla`, kinds `charge` /
      `software_update`). Each needs an RFC3339 UTC time,
      `tz_offset_minutes` and uncertainty `minute`.

## The Hearthloom contract it depends on

htesla needs [Hearthloom](https://github.com/metavert-io/hearthloom)
**0.5.72 or later** (`requires.host` in the
manifest). The public 0.5.71 release (schema 59) predates all of these
APIs; installing on it would pass the version gate and then fail, so
never lower the requirement below the first release that ships them.
From 0.5.72 it uses:

| feature | endpoint / manifest field |
|---|---|
| runs on hosted Linux nodes (the daemon supervises it) | `launch` keyed by Rust target triple; `placement: "cloud"` |
| publishes the Tesla public key without auth | manifest `publish` → `/.well-known/appspecific/com.tesla.3p.public-key.pem` from file `tesla/public-key.pem` |
| sealed secrets | `/v1/plugins/self/secrets[/{key}]` (GET of a value: the sidecar's key only) |
| OAuth sign-in hand-off | `POST /v1/plugins/self/oauth/pending`; the page posts `{type:"hearthloom-oauth"}` to the dashboard; Tesla redirects to `/plugins/htesla/oauth/callback`; `POST /v1/plugins/self/oauth/take` |
| event hints | `GET /v1/bus/stream`: embers hear `plugin.oauth` and `operational.changed`. Keep the short poll fallback; hints are best-effort. |
| movement layer | `POST /v1/ingest/movement` (`ingest` scope; the daemon stamps subject `vehicle:htesla:<id>`) |
| episodes, notifications | `POST /v1/ingest` (`ingest`), `POST /v1/notifications` (`notify`, dedupe key `htesla-cost:<YYYY-MM>:<80\|100>`) |
| storage | `/v1/plugins/self/kv/*`, `/v1/plugins/self/files/*` |

Wire discipline: Hearthloom only ever adds fields. Ignore unknown ones,
and treat a 404/405 as an older host: degrade, don't crash.

### The page ↔ sidecar protocol

The setup page and the program talk only through the ember's KV:
- the page writes button presses to `cmd`
- the sidecar acts on them and rewrites `status`, which the page renders

Add a new action as a new `cmd` value plus a status field. Never give
the page a path to Tesla or to a secret.

## Testing without a car

You need a throwaway Hearthloom daemon. Never use a user's live one.

```bash
HEARTHLOOM_HOME=<scratch dir> HEARTHLOOM_PORT=<free port> \
HEARTHLOOM_HOST=0.0.0.0 HEARTHLOOM_ALLOW_UNREVIEWED_EMBERS=1 \
HEARTHLOOM_SIDECAR_BACKEND=supervisor \
HTESLA_DEV_CLIENT_CA=<dev/ca.pem> HTESLA_DEV_VINS=<vin> \
  hearthloom daemon run
```

- **`HEARTHLOOM_HOST=0.0.0.0`** makes the daemon a "cloud" node, which
  htesla's placement needs.
- **`HEARTHLOOM_ALLOW_UNREVIEWED_EMBERS=1`** lets a cloud node install a
  package that didn't come from the registry.
- **`HEARTHLOOM_SIDECAR_BACKEND=supervisor`** is REQUIRED on a Mac.
  Without it the daemon registers htesla with the user's real launchd
  (`~/Library/LaunchAgents`).
- **Set up the daemon:**
  - bootstrap an admin (`POST /v1/auth/bootstrap {email,password}`) and
    use the session cookie
  - install with `POST /v1/plugins/install {"path": "<dist>"}`
  - for an unreleased host, use a copy of `dist/` with `requires.host`
    lowered
- **Replay:** `htesla-replay certs` → `gen -day … -tz …` → `send`
  (README "Development"). Drives post after they close, and Hearthloom
  mints them when it reconciles a CLOSED day. Run that by hand with
  `POST /v1/settings/automations/trips_finalize/run`.
- **Clean up:** SIGINT the daemon, then check that `pgrep -fl htesla`
  is empty and `~/Library/LaunchAgents` is unchanged. Delete the scratch
  home: a daemon can copy local history into it.

The live test tag (`-tags live`, `HTESLA_LIVE_ENV=<env file>`) makes ONE
real call, a client_credentials token request. Never add live tests that
change anything at Tesla. Never print tokens. Never commit an env file
(`.gitignore` covers `*.env`).

## Tesla facts worth not re-learning

- **Endpoints:**
  - token requests go to `https://fleet-auth.prd.vn.cloud.tesla.com/oauth2/v3/token`
    (not auth.tesla.com)
  - authorize at `https://auth.tesla.com/oauth2/v3/authorize`
  - Fleet API base URLs:
    - NA/APAC: `https://fleet-api.prd.na.vn.cloud.tesla.com`
    - EMEA: `https://fleet-api.prd.eu.vn.cloud.tesla.com`
    - China (`…tesla.cn`) needs a separate developer account and is
      unsupported
- **Partner registration:** `POST /api/1/partner_accounts {"domain"}`
  with a client_credentials token. The domain must match the root domain
  of the app's Allowed Origin, and the public key must already be served
  at the domain.
- **Telemetry config:**
  - signed with vehicle-command's `pkg/sign.SignMessageForFleet(key,
    "TelemetryClient", claims)` and sent to `fleet_telemetry_config_jws`
  - `hostname` must be under the app's root domain
  - `ca` is the chain the car uses to verify OUR server
  - `delivery_policy: "latest"`, no `exp`
  - `minimum_delta` applies to location fields only
  - delete with `DELETE /api/1/vehicles/{vin}/fleet_telemetry_config`
    (free); that's Pause
- **Car behaviour:**
  - sends only when awake, only changed fields, batched every 500 ms
  - buffers ~5000 messages (≈42 min) while it can't connect
  - retries with backoff of 30 s or less
  - connectivity isn't a field: derive it from the socket
- **Billing:**
  - streaming signals cost 150,000 per $1, data calls $0.002, wakes
    $0.02, against a $10 monthly credit
  - a $0 limit or no payment method disables the app
  - exceeding the limit deletes telemetry configs **permanently**
- **Driver identity:** there is none. No active-profile or phone-key
  signal exists, which is why attribution lives in Hearthloom (phone
  evidence + per-car settings).

## Dependencies

- **`github.com/teslamotors/fleet-telemetry` is pinned to v0.9.4.**
  v0.9.5 needs Go 1.27; the difference is proto fields 260–271, none of
  which we stream. Its `config` package pulls in Kafka/ZeroMQ (cgo), so
  the CA bundle is VENDORED in `third_party/fleettelemetry/`
  (`prod_ca.crt`, with Tesla's LICENSE and NOTICE). Refresh it from
  upstream `config/files/prod_ca.crt` when Tesla rotates CAs.
- **`github.com/teslamotors/vehicle-command` v0.4.1**: public `pkg/sign`
  and `pkg/protocol` only. Don't copy from its `internal/`.
- Both are Apache-2.0, and htesla is MIT. Keep the NOTICE file in the repo
  and in `dist/licenses/` (build.sh copies it).

## Releasing

1. Bump `version` in `hearthember.json` (the binary's `-X main.Version`
   comes from it), and `requires.host` if you rely on a newer Hearthloom
   API.
2. Run the build checks, then `./build.sh` and `hearthloom ember validate dist`.
3. `hearthloom ember pack dist` → `htesla-<version>.zip`, then sign it:
   `ember-sign sign -key <publisher key> -package htesla-<version>.zip
   -publisher <publisher>`.
   - The publisher key never goes in this repo; `.gitignore` covers
     `.ember-signing.key`.
4. Publish to the hearthlaunch registry. Hosted nodes install only
   versions the registry has counter-signed AND marked cloud-eligible,
   which is an operator step.
5. Versions are immutable once published. Fix forward with a new
   version.
