# Security policy

## Reporting a vulnerability

Please report security problems privately, through GitHub: open this
repository's **Security** tab and choose **Report a vulnerability**. That
creates a private advisory that only the maintainers can see. Please do not
open a public issue, pull request or discussion about a vulnerability.

A useful report says what you found, how to reproduce it, which version or
commit you tested, and what an attacker could do with it. Fixes land on
`main` and ship in the next release; older releases are not patched
separately.

## What htesla protects, and how

- **It can't control the car.** It asks Tesla only for `openid
  offline_access vehicle_device_data vehicle_location`, never for command
  scopes, never wakes the car and never sends a signed command. The one
  thing it signs is the telemetry configuration, which controls nothing in
  the car.
- **The VIN stays in htesla.** Records sent to Hearthloom carry a vehicle id
  derived from a hash of the VIN, and logs and pages show only its last four
  characters.
- **Secrets live in Hearthloom's sealed store.** The app's private key, the
  client secret, the refresh token and the receiver's TLS keys are never
  written to the ember's KV or files, logs or status. The setup page can set
  a secret but can't read one back.
- **The receiver trusts only Tesla.** The telemetry server accepts client
  certificates only from Tesla's certificate authorities (the vendored
  bundle), and only for VINs on the signed-in account.
- **It can't run up your Tesla bill unseen.** It uses only Tesla's free
  endpoints, meters every Fleet API call, and notifies you at 80% and 100%
  of the monthly credit.
