# htesla — your Tesla's drives, charging and software history, in your own memory

`htesla` is an **ember** (a plugin) for
[**Hearthloom**](https://github.com/metavert-io/hearthloom), a local-first
personal memory system. It reads your car's data **straight from Tesla**
and adds it to your Hearthloom record:

- every **drive**: where you went, how far, how much energy, what was
  playing, the route
- every **charging session**: where, how much energy, battery % before
  and after
- every **software update**, with Tesla's release notes

It also adds a Desk widget (charge level, where the car is parked, the
last drive) and a **Drives** page (your routes, drives per day,
efficiency against temperature).

Owning a car doesn't mean every drive is yours. Hearthloom works out
**whose drive each one was** from your phone's own motion data and your
settings for the car, and asks you a yes/no question only when it can't
tell.

> Not affiliated with, endorsed by or sponsored by Tesla, Inc. "Tesla" is
> a trademark of Tesla, Inc., used here only to say which cars this
> works with.

## How it works

```
 your Tesla ──mutual-TLS WebSocket (Fleet Telemetry)──▶ htesla ──▶ Hearthloom
     ▲                                                    │        (your node)
     │ pairing (Tesla app)                                │
 Tesla's servers ◀──── Fleet API: sign-in, your cars, ────┘
                       pairing status, streaming setup
```

- **The car streams to you.** With Tesla's
  [Fleet Telemetry](https://github.com/teslamotors/fleet-telemetry) the car
  opens a connection straight to your always-on Hearthloom node and sends
  what changed while it is awake. No third-party service sits in between:
  no TeslaMate, no proxy process. Tesla's own Go libraries are compiled
  in.
- **Read-only, by design.** It asks Tesla only for
  `openid offline_access vehicle_device_data vehicle_location`. It never
  requests command permissions, never sends a command, never wakes the
  car, and never polls Tesla's billed `vehicle_data` endpoint. Pairing
  grants it no control over the car.
- **Your data stays on your node.** The only outside calls go to Tesla.
  Secrets (your app key, client secret and sign-in token) are sealed by
  Hearthloom, and are never exported or copied to another node.
- **The car's VIN never enters your record.** Each car gets an opaque id
  (the first 12 hex characters of a SHA-256 of the VIN).

## What you need

1. **A [Hearthloom](https://github.com/metavert-io/hearthloom) always-on
   node, version 0.5.72 or later** (the first release with the ember
   support htesla uses), with a
   public HTTPS address: for example a hosted node at
   `https://yourname.hearthlaunch.com`. The ember runs on that node (its
   placement is `cloud`).
2. **Inbound raw TCP on the telemetry port** (default `8443`) at a
   hostname under the same domain as your node. Raw means the connection
   must reach the node untouched: the car authenticates with a client
   certificate, and that TLS session ends inside htesla, not at a proxy.
   On a hosted node this is an operator setting (an ember port plus a
   dedicated IPv4 address).
3. **A Tesla account** with the car on it, and the Tesla app on your
   phone (for pairing). The car needs firmware 2024.26 or later. 2018 and
   older Model S/X can't stream. Intel Atom Model S/X need *Allow
   Third-Party App Data Streaming* switched on in the car's Safety
   settings.
4. **A Tesla developer app**: free to create, described below.

Not supported yet: Tesla's China region (it needs a separate tesla.cn
developer account).

## Setup guide

### 1. Create your Tesla developer app

At [developer.tesla.com](https://developer.tesla.com), sign in with your
Tesla account and choose **Create Fleet API Application**:

| step | what to enter |
|---|---|
| Registration, Application Details | your details, an app name, and a short description such as "personal data logger for my own car" |
| Client Details: OAuth Grant Type | **Authorization Code and Machine-to-Machine** |
| Allowed Origin URL | your node's address, e.g. `https://yourname.hearthlaunch.com` |
| Allowed Redirect URI | `https://yourname.hearthlaunch.com/plugins/htesla/oauth/callback` |
| Allowed Returned URL | leave empty |
| API & Scopes | **Vehicle Information** and **Vehicle Location** only. Leave Vehicle Commands and Vehicle Charging Management off. |
| Billing Details | add a payment method and a small billing limit (e.g. $10–15). See [Costs](#costs). |

When it's created, note the **Client ID** and **Client Secret**. Don't
paste the secret into chat, email or a file you might commit; you'll
enter it once on the ember's page, which seals it.

### 2. Let the car reach your node

Make sure the telemetry port (default `8443`) accepts raw TCP from the
internet at a hostname under your node's domain (see
[What you need](#what-you-need)). On a hosted node, ask your operator to
enable the ember port.

### 3. Install the ember

- **From the registry:** in Hearthloom open **☰ → Integrations →
  embers**, find `htesla` and choose *add to your hearth*, or run
  `hearthloom ember install htesla`. Review what it asks for and approve.
  The install screen shows that it publishes one file openly at your
  address: your app's **public** key, which Tesla checks.
- **From source:** `./build.sh`, then install the `dist/` folder from
  **☰ → Integrations → embers → install one you built yourself**. Hosted
  nodes accept only registry-reviewed embers unless their operator allows
  others.

### 4. Fill in its settings

On the ember's card in **Integrations**:

| setting | value |
|---|---|
| this node's public address | e.g. `https://yourname.hearthlaunch.com`; must match the Allowed Origin |
| Tesla app Client ID | from step 1 |
| Tesla region | `na` (North America and Asia-Pacific) or `eu` (Europe, Middle East, Africa) |
| telemetry port | `8443` unless you chose another |
| time zone for drives and charges | your IANA zone, e.g. `America/New_York`. The car doesn't report one, and drives and charges are dated in it. |

### 5. Follow the six steps on the Tesla page

Open **☰ → Tesla**. Do this from **your always-on node's own
dashboard**, because sign-in returns to that node. Each step shows its
live state:

1. **Your app's key.** htesla makes an EC P-256 key pair once. The
   private half is sealed, and the public half is published at
   `https://<your node>/.well-known/appspecific/com.tesla.3p.public-key.pem`.
   It is never replaced on its own, because a new key would break the
   car's pairing.
2. **Your Tesla developer app.** Check the Allowed Origin and Redirect
   URI it shows against your app, then save your **Client Secret**. The
   page can save it but never read it back.
3. **Register this address.** htesla checks that your public key is
   reachable, then registers your node's domain with Tesla.
4. **Sign in with Tesla.** A Tesla sign-in tab opens. Approve, and Tesla
   sends you back to your node, which hands htesla a one-time code.
5. **Pair your car.** Open the `tesla.com/_ak/…` link shown for each car
   on the phone with the Tesla app, and approve the key.
6. **Start streaming.** htesla signs a streaming configuration with your
   app key and sends it to Tesla. Once the car shows **synced**, it
   streams to your node whenever it's awake.

### 6. Check it's working

- The Tesla page shows the car online, records arriving, and this month's
  projected cost.
- The **Desk widget** shows charge level and where the car is parked.
- Drives appear in your record **the day after** they happen, because
  Hearthloom reconciles each day once it's over. Charging sessions and
  software updates appear as soon as they finish.

## Whose drive was it?

htesla records each drive as **the car's** movement. Hearthloom then
decides each night whose drive it was:

| your phone (Hearthloom's iOS app) during the drive | result |
|---|---|
| in a vehicle at the same time | **your trip**, with the car's distance, energy and route. Your phone's own record of that trip is folded in. |
| walking, still, or somewhere else | **someone else's drive**, filed under the car's setting for other people's drives (`family` by default), and kept off your map |
| nothing, though you normally have phone data | "not known yet", plus a yes/no on **Tend**: "Were you in the Model Y on Tue Oct 20, 08:10–08:32?" (at most three a week per car) |
| no phone data at all | the car's setting (yours, unless you say otherwise) |

Guest Mode drives are never yours. In **Integrations → your cars** you
can set a car to *always mine* or *never mine*, and choose who drives
when you're aboard and who else drives it. Tesla doesn't say which
driver profile is active, so this is the only honest way to tell.

## Costs

Tesla bills Fleet API use against a **$10 monthly credit** per developer
account:

- streamed data: 150,000 signals per $1
- data requests: $0.002 each (htesla uses only Tesla's free endpoints)

Fields are sent only when they change. A parked car costs almost
nothing, and driving costs roughly 1,100–1,300 signals an hour (about
$0.008). One car should stay well inside the credit.

Two cautions:
- Tesla disables apps that have no payment method or a $0 billing limit.
- Going over your billing limit makes Tesla **delete the streaming setup
  for good**.

htesla therefore projects the month on its page and rings your
notification bell at 80% and 100% of the credit. **Pause streaming**
removes the setup without cost, and **Resume** restores it.

## What it stores

Everything goes through Hearthloom's ember storage; htesla never writes
to disk directly.

| where | what |
|---|---|
| files `telemetry/<day>/<minute>.jsonl` | every record received, kept 14 days |
| files `drives/<id>.json` | each drive's route (500 points or fewer) |
| KV `latest`, `summary`, `drives`, `status`, `builder`, `cost`, `usage/<day>` | the latest values, the widget's summary, the drives index, setup state, the record builder's cursor and outbox, cost tracking |
| sealed secrets | your app's private key, client secret, sign-in token, and the TLS keys |
| files `tesla/public-key.pem`, `tls/*.pem` | the published public key and the receiver's certificates |

What reaches your record: drives (through Hearthloom's movement layer),
`charge` episodes and `software_update` episodes.

## Troubleshooting

| symptom | likely cause |
|---|---|
| "Register" says the key isn't reachable | `https://<your node>/.well-known/appspecific/com.tesla.3p.public-key.pem` doesn't load from the internet. Check that the ember is enabled and your node's address. |
| Sign-in says the link expired | You started sign-in on a different node than the one in your Redirect URI. Start it from that node's dashboard. |
| "Sign in again" | Tesla sign-in tokens expire after three months unused, and a password change ends them. Sign in again from step 4. |
| Pairing never shows as paired | Approve the key in the Tesla app on a phone signed in to the same Tesla account. |
| Streaming config never *synced*, or errors show | The car is asleep or offline (it syncs when it next wakes), its firmware is too old, or it can't reach your telemetry port. |
| Car connects but is refused | Check that raw TCP reaches the node untouched (no TLS-terminating proxy) and that the hostname is under your app's domain. |
| The page says secrets are sealed with a key on the same disk | Your node has no master key configured. Secrets are still encrypted, but the key sits beside them. |

## Status

htesla 0.2 is new. Its parts are unit-tested and it has been run end to
end against a Hearthloom node with replayed telemetry. It has not yet
been run against a real car. Two things that only a real car can confirm:
- whether every car accepts the private certificate authority htesla
  creates for its receiver (Tesla's docs recommend a commonly trusted
  one)
- the behaviour when a stopped hosted node is woken by the car's
  connection

Reports are welcome.

## Development

```bash
./build.sh        # → dist/ for linux amd64/arm64 and macOS arm64/amd64 (static, CGO off)
go test ./...     # unit tests (Go 1.26)
```

- **No car needed:** `cmd/htesla-replay` generates a day of telemetry and
  plays it into a running receiver over the same mutual-TLS WebSocket a
  car uses. It only works against a sidecar started with the development
  variables `HTESLA_DEV_CLIENT_CA` and `HTESLA_DEV_VINS`; a real node
  trusts only Tesla's certificate authorities.
- **One live check:** with your own credentials in an env file,
  `HTESLA_LIVE_ENV=<file> go test -tags live -run Live ./internal/tesla/`
  makes one harmless real call, an app-level token request.
- `CLAUDE.md` is the maintainer guide: the rules the code keeps, the
  Hearthloom contract it depends on, and how to release.

### Layout

| piece | where |
|---|---|
| manifest (scopes, placement, launch per platform, published file, settings, pages) | `hearthember.json` |
| Hearthloom client: KV, files, sealed secrets, sign-in hand-off, event stream | `internal/hearth` |
| app key pair | `internal/keys` |
| Tesla sign-in, rotating refresh tokens, Fleet API, streaming config and signing | `internal/tesla` |
| telemetry receiver (mutual-TLS WebSocket, decode and acknowledge) | `internal/receiver` |
| receiver certificates | `internal/certs` |
| record storage | `internal/store` |
| drive, charge and update builder | `internal/builder` |
| cost projection and warnings | `internal/cost`, `internal/app/costguard.go` |
| the setup state machine and record posting | `internal/app` |
| setup page, Desk widget, Drives page | `ui/` |
| Tesla's certificate authority bundle (Apache-2.0) | `third_party/fleettelemetry` |

## License

MIT, see [LICENSE](LICENSE). htesla uses
[`teslamotors/vehicle-command`](https://github.com/teslamotors/vehicle-command)
and [`teslamotors/fleet-telemetry`](https://github.com/teslamotors/fleet-telemetry)
(both Apache-2.0, Copyright Tesla, Inc.) and includes fleet-telemetry's
production certificate bundle; see `third_party/fleettelemetry/NOTICE`.
