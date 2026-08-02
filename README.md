# bambu-util

Utilities for Bambu Lab printers on the local network.

## P1S web bridge

A single-binary web app for controlling a Bambu P1S from a phone browser.
Browsers can't speak the printer's protocols (MQTT over TLS on :8883, a
proprietary camera stream on :6000), so this bridge runs on a machine on the
same network, holds those connections, and serves a plain mobile web page.

Features:

- **Bed down** (absolute move to Z200), **Home** (`G28`), **Extrude**, and
  **Unload** — the manual bed and filament actions
- Bed-drying and nozzle-cleaning temperature sliders, each with material
  presets and a safety auto-off
- Live status: connection, printer state, bed/nozzle temperatures
  (actual/target), print progress, job name, layer, and time remaining
- Filament (AMS): per-tray colour, material, and nozzle temperature range,
  plus the unit's desiccant dryness as Bambu Studio's A-E grade
- Chamber camera (~1 fps), recorded continuously into a rolling buffer
  (24h by default, set in Settings) — the bridge holds the camera
  connection the whole time it runs, not just while someone is watching,
  so Bambu Studio's own camera view will not work while bambu-util is
  running (the printer only serves one camera client at a time). One view
  shows it all: it follows the live tail of the buffer by default, and a
  scrub bar drags back through earlier footage. While the printer is idle
  the bar reaches back exactly as far as the history window keeps frames,
  so nothing is recorded that can't be scrubbed to; during a print it starts
  five minutes before the print did, so the whole job is one drag and nothing
  earlier is in the way. A `● LIVE` badge on the image shows whether the view
  is following the tail, and tapping it returns there
- Recent print jobs are listed under the camera, each with its start time so
  two runs of the same file can be told apart. Pick one to play its
  footage as a timelapse at 30x, 60x, 300x, or 600x. The five most recently
  finished prints keep their footage regardless of the retention window,
  thinned to one frame every 10 seconds once it ages out — enough for a
  timelapse without holding whole prints at the full recording rate
- Bed actions are refused server-side unless the printer is idle
  (IDLE/FINISH/FAILED) — nothing can move the bed or change temperatures
  mid-print
- Print controls: **Pause** (while printing), **Resume** (while paused), and
  **Stop** (either; needs a second confirming tap). Guarded server-side to the
  matching printer states
- Chamber lamp automation: turns on automatically the moment a job starts
  running or the bed/nozzle is commanded hot, and off automatically 8h
  after it goes idle. The manual toggle always works and is never
  overridden — automation only ever acts on the active/idle transitions
  themselves
- A Settings screen — the gear in the top corner — holds notifications, the
  camera history window, and the automatic-off delays for the bed, nozzle and
  chamber lamp. It is a separate screen, not more cards below the status, so
  the printer controls stay at the top of the page. The values are stored in
  the database and take effect as soon as they are saved
- An Events screen shows what was sent to the printer and whether the printer
  acknowledged it, what the printer reported back, and what notifications went
  out — each with its raw message, for working out why something did not happen.
  It is kept in the database, so it is still there after a restart, which is
  when it is most wanted. How much it may hold is a size in megabytes set on the
  Settings screen; the oldest entries go once it is reached
- An optional cap on the database file itself, off by default. With one set, the
  oldest data goes — camera frames and event-log entries alike, whichever is
  older — until the file is back under it, and the space is returned to the disk
  a little at a time rather than in one long pause. It overrules the camera
  window and the prints-kept setting, because a full disk stops everything
- Light, dark, or follow-the-system theme, and the printer screen's sections
  can be hidden and reordered from Settings
- iOS "Add to Home Screen" gives an app-like full-screen page
- Notifications to the phone, so the page does not have to be open: a print
  starting, finishing or ending without finishing; any error the printer
  raises, which is how filament runout arrives; and a repeating reminder of how
  long the bed has been on with no print running. Each subscribed device picks
  which of those it wants and how often to be reminded, so a phone and a tablet
  can differ. Turn them on from
  the Notifications card; a test button confirms the whole path before waiting
  on the printer. Requires the app to be served over HTTPS.
  On iPhone and iPad it additionally requires iOS 16.4 or later **and the app
  added to the Home Screen** — a Safari tab cannot receive notifications, and
  the page says so rather than failing quietly

### Configuration

The app cannot read from a database it has not been told how to find, serve a
page before it knows where to listen, or put a login in front of itself using
settings that are behind that login. Everything else — including the printer —
is set on the Settings screen and kept in the database.

**The app will not start until it is told what to do about authentication.**
Either configure a provider or say expressly that you want none. There is no
default, so an app that is running is one whose exposure was chosen rather than
overlooked.

| Variable | Required | Description |
|---|---|---|
| `OIDC_ISSUER` | to require a login | The provider's base URL, e.g. `https://id.example.com`. Its configuration is read from `/.well-known/openid-configuration` under this, at startup, so a wrong URL stops the app rather than surfacing at the first login |
| `OIDC_CLIENT_ID` | to require a login | From the provider |
| `OIDC_CLIENT_SECRET` | to require a login | From the provider. This is a server-side app, so it is a confidential client |
| `PUBLIC_URL` | to require a login | Where the app is reached from a browser, e.g. `https://printer.example.com`. The redirect URI is this plus `/auth/callback`, and that exact URL is what the provider must have registered. It is given rather than worked out from the request, because a redirect built from a header the browser controls is how a login ends up being sent somewhere else |
| `AUTH_DISABLED` | instead of the four above | Set to `true` to run with no login at all. Anything that can reach the port can then drive the printer and watch the camera |
| `LISTEN_ADDR` | no | Listen address, default `:8081` |
| `DATA_DIR` | no | Directory for the database, default `./data`. It also holds the pending heater and lamp countdowns, so they survive a restart. Mount a volume here so the history buffer survives restarts — and so notification subscriptions do, since losing the server's identity silently unsubscribes every phone. Write-ahead logging means the directory also holds `-wal` and `-shm` files; a backup taken while the app runs needs all three, not just the `.db`. |

Any OpenID Connect provider works — nothing here is written against a
particular one. It was built against [Pocket ID](https://pocket-id.org), where
the client needs the redirect URI above and a client secret, and its "Restrict
User Groups" decides who may log in. The app trusts the provider on that: a
valid login for its client gets in, so access is managed in one place rather
than two that can disagree.

How long a login lasts is on the Settings screen, counted from the last time the
page was used, and is 14 days by default.

### Run

```sh
AUTH_DISABLED=true go run ./cmd/bambu-util
```

Then open the page, go to Settings, and enter the printer's address, serial and
access code — from the printer screen, Settings → WLAN for the address and
access code, Settings → Device for the serial.

Or the container image: `ghcr.io/brhelwig/bambu-util` (linux/arm64), tagged
three ways:

| Tag | Points at |
|---|---|
| `latest` | The newest `main` build. Deployments that follow it pick up new code by restarting. |
| `vX.Y.Z` | A released version — the same manifest as the `main` build it was cut from, not a rebuild. Use it to pin or roll back. |
| `<commit sha>` | One specific `main` build. |

### Printer prerequisites

Recent P1 firmware rejects third-party G-code unless **LAN Only Mode** and
**Developer Mode** are enabled on the printer screen. Status and camera work
either way; the four actions need Developer Mode.

### Protocol notes

- MQTT: TLS :8883, username `bblp`, password = LAN access code, self-signed
  certificate. Status arrives on `device/<serial>/report`; after the initial
  `pushall` dump the printer only sends changed fields, so reports are merged
  into a cached state.
- Camera: TLS :6000. An 80-byte auth packet (magic words `0x40`, `0x3000`,
  then username and access code zero-padded to 32 bytes each), then framed
  JPEGs: a 16-byte header whose first four bytes are the little-endian image
  size. Layout learned from
  [ha-bambulab](https://github.com/greghesp/ha-bambulab)'s chamber-image
  client.

### Security

The page has no authentication — run it only on a trusted network (LAN or
tailnet). The printer's access code is stored in the database under `DATA_DIR`,
so that directory is as sensitive as the credential itself and belongs on a
volume you would not share. The page is never sent the code back, only whether
one is set, so it cannot be read off a screen — but anyone who can reach the
page can replace it.
