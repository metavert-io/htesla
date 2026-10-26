# htesla: review guidance

htesla is a Hearthloom ember that receives a Tesla's Fleet Telemetry stream
and records drives, charging and software updates in the owner's Hearthloom.
Go (`cmd/`, `internal/`), with the setup and drive pages in `ui/`.
`CLAUDE.md` is the maintainer guide; its hard rules protect the user's car,
money and privacy.

## Toward the car and Tesla's billing
- Read-only: OAuth scopes stay `openid offline_access vehicle_device_data
  vehicle_location`. No `vehicle_cmds` or `vehicle_charging_cmds`, no
  `wake_up`, no signed commands; the telemetry configuration is the one
  signed thing.
- Never poll billed endpoints (`vehicle_data`, `nearby_charging_sites`):
  Tesla bills every response below 500, including a 408. Every Fleet API
  call site goes through the cost meter.

## Privacy and secrets
- The VIN never leaves htesla. Posts use `builder.VehicleID(vin)` (the first
  12 hex characters of sha256(VIN)); logs and UI show the last 4 only. Keep
  the tests that assert no post contains a VIN.
- Secrets (`tesla_private_key`, `client_secret`, `tesla_refresh_token`,
  `tls_ca_key`, `tls_server_key`) live only in the daemon's sealed store:
  never in KV, files, logs, status, fixtures or error messages. The UI page
  can set a secret but never reads one, and never calls Tesla.
- Never write under `~/.hearthloom`: persistence goes through the ember API
  (KV, files, secrets).

## Correctness traps
- Never regenerate the app key after an error: a daemon or storage error is
  not "no key". Only the owner's explicit "make a new key" replaces it.
- Tesla refresh tokens are single-use. `tesla.Tokens.adoptLocked` seals the
  new pair before exposing the access token; don't reorder it.
- The receiver trusts only the vendored Tesla CA bundle, and only VINs on the
  signed-in account. The development trust (`HTESLA_DEV_CLIENT_CA`,
  `HTESLA_DEV_VINS`) stays replay-only, loud in the log, and unreachable from
  settings or the UI.
- No double posts, no lost records: the builder's cursor, open drive or
  charge and outbox are one KV document, saved before anything is posted;
  record keys are deterministic.
- Drives are posted as the car's movement (`POST /v1/ingest/movement`);
  htesla never mints drive episodes. Charges and software updates carry an
  RFC3339 UTC time, `tz_offset_minutes` and uncertainty `minute`.

## Wire, pages and dependencies
- Hearthloom only ever adds fields: ignore unknown ones, and treat a 404/405
  as an older host (degrade, don't crash).
- The page and the sidecar talk only through KV (`cmd` in, `status` out). No
  `alert`/`confirm`/`prompt` in the pages.
- `fleet-telemetry` stays at v0.9.4 (v0.9.5 needs Go 1.27). Use only the
  public `pkg/` packages of `vehicle-command`, and keep the Apache-2.0
  LICENSE and NOTICE files.
