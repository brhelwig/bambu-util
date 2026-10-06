# Changelog

All notable changes to this project are documented in this file. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Every commit
that passes CI on `main` is released, versioned by date as `YY.DOY.M` (year,
day of year, minute of day, UTC; e.g. `26.279.907`).

## [Unreleased]

### Added

- Login over OpenID Connect (authorization code flow with PKCE). Built against
  the standard discovery document and tested with Pocket ID; who may log in is
  decided by the provider. Sessions are stored in the database and last 14 days
  from last use by default. The health check and the files a phone needs before
  logging in are served without a session.
- Settings screen (gear icon) for the printer connection, notifications,
  camera history window, number of prints whose footage is kept, bed/nozzle/lamp
  auto-off delays, event log size, database size cap, login length, theme and
  status-screen layout. Values are stored in the database, applied with a Save
  button, and take effect without a restart.
- Events screen (pulse icon) listing commands sent and whether the printer
  acknowledged them, printer reports, and notifications sent, each with its raw
  message. Stored in the database with a size limit (64 MB by default).
- Optional cap on the database file size, off by default. When exceeded, the
  oldest camera frames and events are deleted, regardless of other retention
  settings, and the space is returned to the disk incrementally.
- Web push notifications: print started, finished or failed; printer errors
  (including filament runout); heaters turned off automatically; and a
  repeating reminder while the bed is hot with no print running. Each device
  chooses which it wants. Requires HTTPS, and iOS 16.4+ with the page added to
  the Home Screen on iPhone/iPad.
- Continuous camera recording into a rolling buffer (24h by default). One view
  follows the live image with a scrub bar into the buffer and a `● LIVE` badge
  to return to it. During a print the scrub bar starts 5 minutes before the
  print did.
- Recent prints list with start times; each can be played back as a timelapse.
  The most recent finished prints (5 by default) keep their footage past the
  buffer window, thinned to one frame every 10 seconds. The print in progress is
  also protected, up to 48h.
- Chamber lamp automation: on when a print starts or a heater is set, off after
  a delay (8h by default) once idle. Manual toggles are never overridden.
- Readable messages for printer errors: about 4,900 HMS codes and 940
  `print_error` codes, shown in the error banner and notifications. The wording
  comes from the community and has not been checked against a real printer.
- Light, dark or system theme (system by default).
- Status-screen sections can be hidden and reordered.

### Changed

- **Breaking:** the app will not start unless authentication is configured. Set
  `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` and `PUBLIC_URL`, or set
  `AUTH_DISABLED=true` to run without a login.
- **Breaking:** the printer is configured on the Settings screen. `PRINTER_IP`,
  `PRINTER_SERIAL`, `PRINTER_ACCESS_CODE` and `RECORDING_RETENTION` are no
  longer read. The app starts without a printer, and changing it reconnects
  without a restart. The access code is never sent back to the page.
- The camera connection is held continuously, so Bambu Studio's camera view
  will not work while bambu-util is running.
- The MJPEG live-stream endpoint and the camera on/off toggle are removed; every
  view reads from the recording buffer.
- Timelapse speeds are 30x, 60x, 300x and 600x, played at 4 frames per second.
- Pending auto-off and reminder timers are stored in the database and survive a
  restart; one that fell due while the app was down fires on startup.
- The heater auto-off waits until the printer is idle instead of turning
  heaters off mid-print.
- Pause, resume, stop and unload are sent with MQTT QoS 1, so the printer has to
  acknowledge them. Extrude is still sent without, since it is unsafe to repeat.
- Unload only requires the printer to be idle; it no longer requires the AMS to
  report a loaded tray, which external spools never do.
- The app is named **Bambu Util** in the page, browser tab, notifications and
  manifest. Home Screen icons added before this keep their old label until
  re-added.
- **Eject** is renamed **Unload**.
- Time remaining is shown as hours and minutes (`2h 15m`) once over an hour.
- AMS desiccant dryness is shown as Bambu Studio's A–E grade.

### Fixed

- With `AUTH_DISABLED=true`, requests a browser marks as coming from another
  site are refused, so other web pages can't send commands to the printer.
- A print no longer appears more than once in the recent-prints list after a
  pause or a restart, and `PREPARE` or an unknown state no longer ends a job
  that is still running.

### Removed

- `POST /api/actions/load`. The page stopped using it in 0.5.0.
- Chamber temperature, which the P1S does not report meaningfully. Chamber fan
  speed is unaffected.
- Wi-Fi signal strength.

## [0.5.0] - 2026-07-22

### Added

- Per-tray filament editor in the AMS card: an Edit button opens an inline form
  to set a tray's colour, material type, and nozzle temperature range
  (`ams_filament_setting`). It prefills from the tray's reported values and
  resends the whole profile — including the printer's `tray_info_idx` unchanged
  — so editing one field doesn't blank the others. Idle-only.

### Fixed

- State cache now deep-merges partial MQTT reports. Nested fields like the AMS
  `tray_now` (the tray currently fed to the nozzle) previously got wiped
  whenever a later partial report re-sent the `ams` object without them, so the
  UI could never tell which bay was loaded. They now survive partial updates.

### Changed

- Bed/print controls are reworked into a compact icon grid with accessible
  labels: camera, lamp, pause/resume, stop on the first row; lower bed, home,
  extrude, eject on the second.
- Camera is now a manual toggle that remembers its last state per browser and
  only requests the feed if it was left on, instead of auto-starting.
- The bed-drying and nozzle cards are hidden while a print is running: the
  printer drives those temperatures itself, so the sliders' nearest-preset
  value would disagree with the live machine status (e.g. slider 60 vs bed 55).
- Filament handling is unload-only. A single Eject button — disabled when
  nothing is loaded — replaces the per-slot load/unload buttons, since the
  printer loads filament on its own. The AMS card marks the loaded tray.

## [0.4.0] - 2026-07-22

### Added

- Camera stream auto-starts on page load.
- Status is split into "Job status" and "Machine status" cards; the job card
  shows a "No active print" placeholder when idle.
- New status fields: job name, layer / total layers, time remaining, chamber
  temperature, wifi signal, and per-fan speeds (cooling / aux / chamber).
- AMS filament slots with colour swatch, material, and reported humidity.
- HMS error banner, shown only when the printer reports errors, translated via
  a small code lookup table.
- Bed drying slider using Bambu's official P1S bed-drying presets (60–100 °C).
- Nozzle cold-pull / cleaning slider (presets slightly above print temp) with
  an Extrude button, blocked unless the nozzle is hot.
- Filament unload, and per-slot load that heats to the nozzle temperature set
  on the slider.
- Chamber lamp toggle.
- Heater safety auto-off (bed after 24 h, nozzle after 15 min) enforced
  server-side, with a live countdown; adjusting a heater resets its timer.
- Demo mode for previewing without a printer: `?demo` (idle, interactive) and
  `?demo=print` (running job, controls locked).

### Changed

- The camera show/hide toggle is removed.
- Bed heating moved from a fixed 100 °C toggle to the drying slider, and the
  nozzle from a fixed toggle to the cleaning slider.

## [0.3.0] - 2026-07-21

### Changed

- Pause and Resume are one toggle button: it reads "Pause print" while
  printing and "Resume print" while paused.
- "Bed 100°C" and "Heater off" are one toggle button, switching on whether
  the bed currently has a target temperature.

## [0.2.0] - 2026-07-21

### Changed

- Container image renamed from `ghcr.io/brhelwig/p1s-bridge` to
  `ghcr.io/brhelwig/bambu-util` to match the repository name.
- Binary, command path (`cmd/bambu-util`), and release archive names renamed
  from `p1s-bridge` to `bambu-util`.
- Print-control buttons are now always visible and merely disabled when not
  applicable, instead of hidden outside RUNNING/PAUSE.
- The web page is served with `Cache-Control: no-cache` so UI updates reach
  browsers (and iOS home-screen apps) immediately.

## [0.1.0] - 2026-07-21

### Added

- Print controls: pause (while RUNNING), resume (while PAUSE), stop (RUNNING
  or PAUSE, with a two-tap confirm in the UI). Guards enforced server-side;
  `/api/status` gains a `printActions` map and the page shows only
  currently-valid controls.

## [0.0.1] - 2026-07-21

### Added

- `p1s-bridge`: single-binary web app for controlling a Bambu P1S over the
  local network from a phone browser
  - Bed actions (lower bed, home, bed 100°C, heater off), refused server-side
    unless the printer is idle (IDLE/FINISH/FAILED)
  - Live status over MQTT: connection, printer state, bed/nozzle temperatures,
    print progress
  - Chamber camera relayed as MJPEG; the printer camera connection is held
    only while someone is watching
  - Embedded dark mobile web page with iOS "Add to Home Screen" support
- Container image `ghcr.io/brhelwig/p1s-bridge` (linux/arm64), pushed on every
  merge to main
- Release binaries for Linux, macOS, and Windows (amd64 and arm64)
- Monthly Dependabot updates for Go modules, GitHub Actions, and Docker base
  images

[Unreleased]: https://github.com/brhelwig/bambu-util/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/brhelwig/bambu-util/releases/tag/v0.5.0
[0.4.0]: https://github.com/brhelwig/bambu-util/releases/tag/v0.4.0
[0.3.0]: https://github.com/brhelwig/bambu-util/releases/tag/v0.3.0
[0.2.0]: https://github.com/brhelwig/bambu-util/releases/tag/v0.2.0
[0.1.0]: https://github.com/brhelwig/bambu-util/releases/tag/v0.1.0
[0.0.1]: https://github.com/brhelwig/bambu-util/releases/tag/v0.0.1
