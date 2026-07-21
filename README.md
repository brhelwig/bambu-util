# bambu-util

Utilities for Bambu Lab printers on the local network.

## p1s-bridge

A single-binary web app for controlling a Bambu P1S from a phone browser.
Browsers can't speak the printer's protocols (MQTT over TLS on :8883, a
proprietary camera stream on :6000), so this bridge runs on a machine on the
same network, holds those connections, and serves a plain mobile web page.

Features:

- **Lower bed** (absolute move to Z200), **Home** (`G28`), **Bed 100°C**,
  **Heater off** — the bed-drying / cleaning actions
- Live status: connection, printer state, bed/nozzle temperatures
  (actual/target), print progress
- Chamber camera view (~1 fps), relayed as MJPEG; the bridge only connects to
  the printer camera while someone is watching
- Actions are refused server-side unless the printer is idle
  (IDLE/FINISH/FAILED) — nothing can move the bed or change temperatures
  mid-print
- iOS "Add to Home Screen" gives an app-like full-screen page

### Configuration

Environment variables only — no config files:

| Variable | Required | Description |
|---|---|---|
| `PRINTER_IP` | yes | Printer LAN IP (printer screen → Settings → WLAN) |
| `PRINTER_SERIAL` | yes | Printer serial (Settings → Device) |
| `PRINTER_ACCESS_CODE` | yes | LAN access code (Settings → WLAN) |
| `LISTEN_ADDR` | no | Listen address, default `:8081` |

### Run

```sh
PRINTER_IP=192.0.2.10 PRINTER_SERIAL=01P00XXXXXXXXXX PRINTER_ACCESS_CODE=xxxxxxxx \
  go run ./cmd/p1s-bridge
```

Or the container image: `ghcr.io/brhelwig/p1s-bridge` (linux/arm64).

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
