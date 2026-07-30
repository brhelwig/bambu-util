# Changelog

All notable changes to this project are documented in this file. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Notifications to the phone, so the page does not have to be open. Turn
  them on from the Notifications card; a test button proves the path before
  anything is riding on it. Subscriptions are stored under `DATA_DIR`
  alongside the server identity browsers bind to — losing that directory
  silently unsubscribes every phone, so it belongs on a volume.
  Requires HTTPS, and on iPhone and iPad iOS 16.4 or later with the app
  added to the Home Screen. Only the test message exists so far; printer
  events follow.
- Always-on camera recording into a rolling history buffer (default 24h,
  configurable via `RECORDING_RETENTION`), stored in SQLite under `DATA_DIR`.
- One camera view: follows the live tail of the recording buffer by
  default, with a scrub bar to drag back through recent footage, a
  **Live** button to jump back to the tail, and a jobs list to fast-forward
  through a specific print's footage as a timelapse.
- Chamber lamp automation: turns on the moment a job starts or the
  bed/nozzle is commanded hot, off automatically 8h after going idle.
  Fires only on those transitions, so a manual toggle in between is never
  overridden. Shown as a countdown in the status card, same as the
  bed/nozzle auto-off timers.
- Released container images are tagged `vX.Y.Z`, so a deployment can pin a
  version or roll back to one. The version tag names the same manifest the
  `main` build produced rather than a rebuild of the tagged commit.

### Changed

- Notification subscriptions moved into the existing database, having briefly
  had a `push.db` of their own. An installation tracking `main` that already
  turned notifications on will mint a fresh identity on the next start, and
  those phones stop receiving until notifications are turned on again — the
  page notices and resets itself to Off. Delete the orphaned
  `DATA_DIR/push.db`; nothing reads it any more. No released version is
  affected.
- The camera connection is now held continuously so it can record, instead
  of only while a viewer is on the page. Bambu Studio's own camera view will
  not work while bambu-util is running.
- Removed the raw MJPEG live-stream endpoint and the manual camera on/off
  toggle — every view now sources frames from the recording buffer, so
  there's no separate "live" connection to toggle.
- The scrub bar is bounded rather than spanning the whole stored buffer: it
  reaches back one `RECORDING_RETENTION` window (24h by default) while the
  printer is idle, and starts 5 minutes before the print began while one is
  running, so a job is one drag of the bar. Older footage — the kept prints'
  thinned timelapses — is still reachable by picking a job from the list.
- Timelapse speeds are 30x, 60x, 300x, and 600x, and play back at 4 frames per
  second instead of 1 — the old 1x-20x speeds were slower than the recording
  rate they were replaying.
- The five most recently finished prints keep their footage regardless of
  `RECORDING_RETENTION`, thinned to one frame every 10 seconds once past the
  cutoff. Whole prints at the full recording rate would add gigabytes. The
  in-progress print is protected too, so a print longer than the retention
  window still yields a whole timelapse — but only back 48h, so a job row left
  open by a printer that vanished mid-print cannot exempt footage from
  retention indefinitely.
- Recent jobs show each print's start time, so two runs of the same file can
  be told apart.
- **Play** is an icon button, and live is a `● LIVE` badge on the camera image
  itself — lit red while the view follows the tail, dimmed once it has been
  scrubbed back, and tapping it returns to live. It used to be a skip-to-end
  glyph sitting between play and the speed selector, which read as "next track"
  and grouped a mode with the controls that only ever act on recorded footage.
- The page no longer carries a "P1S bed control" heading; the name lives in the
  document title, matching the web manifest.
- **Eject** is now called **Unload**, matching what it does.
- Time remaining reads as hours and minutes (`2h 15m`) once over an hour.
- AMS desiccant dryness is shown as Bambu Studio's letter grade (A driest)
  instead of a bare `level 5`.

### Fixed

- Pause, resume, stop and unload are now sent so the printer's broker has to
  acknowledge them, and retries until it does. Every command was previously
  sent unacknowledged, so a dropped **Stop** was silent — the page reported it
  sent and the print carried on. Commands that are unsafe to repeat, such as
  extrude, deliberately stay unacknowledged.
- The scrub bar's caption reports when the displayed frame was actually taken,
  not the time it was dragged to. The frame endpoint returns the first frame at
  or after the requested time, so with retention leaving gaps between kept
  prints the two could be hours apart and the picture was misdated.
- A print no longer appears in the recent-jobs list more than once. Pausing
  closed the job and resuming opened a second one, and a restart mid-print
  opened another while leaving the first open forever; a paused print now
  counts as the same job, and a restart adopts the row already open. Printer
  states that mean neither running nor finished — `PREPARE`, or `unknown`
  before the first status report — no longer end a job that is still going.
  A print running under a different name than the adopted row starts its own
  row, so a job boundary crossed while the service was down no longer files
  one print's footage under the previous print's name.

### Removed

- The filament load path — `POST /api/actions/load` and the command behind it.
  Filament handling became unload-only two releases ago and the page has not
  called it since, but it stayed reachable to anyone who knew the URL and it
  commanded the printer to feed filament.
- Chamber temperature. The P1S reports a value that does not track the
  chamber (5°C mid-print, with the bed at 55°C), and there is no way to make
  it meaningful. The chamber *fan* speed is unaffected.
- Wi-Fi signal strength. Nothing is decided by it, and the printer being
  reachable is already reported by the connection row.

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

- Camera stream auto-starts on page load; the show/hide toggle is gone.
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

[0.3.0]: https://github.com/brhelwig/bambu-util/releases/tag/v0.3.0
[0.2.0]: https://github.com/brhelwig/bambu-util/releases/tag/v0.2.0
[0.1.0]: https://github.com/brhelwig/bambu-util/releases/tag/v0.1.0
[0.0.1]: https://github.com/brhelwig/bambu-util/releases/tag/v0.0.1
