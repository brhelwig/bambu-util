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
  (`RECORDING_RETENTION`, default 24h) — the bridge holds the camera
  connection the whole time it runs, not just while someone is watching,
  so Bambu Studio's own camera view will not work while bambu-util is
  running (the printer only serves one camera client at a time). One view
  shows it all: it follows the live tail of the buffer by default, and a
  scrub bar drags back through earlier footage. While the printer is idle
  the bar reaches back exactly as far as `RECORDING_RETENTION` keeps frames,
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
- iOS "Add to Home Screen" gives an app-like full-screen page
- Notifications to the phone, so the page does not have to be open. Turn them
  on from the Notifications card; a test button confirms the whole path
  before waiting on the printer. Requires the app to be served over HTTPS.
  On iPhone and iPad it additionally requires iOS 16.4 or later **and the app
  added to the Home Screen** — a Safari tab cannot receive notifications, and
  the page says so rather than failing quietly

### Configuration

Environment variables only — no config files:

| Variable | Required | Description |
|---|---|---|
| `PRINTER_IP` | yes | Printer LAN IP (printer screen → Settings → WLAN) |
| `PRINTER_SERIAL` | yes | Printer serial (Settings → Device) |
| `PRINTER_ACCESS_CODE` | yes | LAN access code (Settings → WLAN) |
| `LISTEN_ADDR` | no | Listen address, default `:8081` |
| `DATA_DIR` | no | Directory for the recording and notification databases, default `./data`. Mount a volume here so the history buffer survives restarts — and so notification subscriptions do, since losing the server's identity silently unsubscribes every phone. |
| `RECORDING_RETENTION` | no | How long to keep recorded frames, as a Go duration (`12h`, `48h`, ...), default `24h`. Also how far back the scrub bar reaches when no print is running |

### Run

```sh
PRINTER_IP=192.0.2.10 PRINTER_SERIAL=01P00XXXXXXXXXX PRINTER_ACCESS_CODE=xxxxxxxx \
  go run ./cmd/bambu-util
```

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
tailnet). Real printer credentials never live in this repo; they are injected
as environment variables at deploy time.
