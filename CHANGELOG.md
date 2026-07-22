# Changelog

All notable changes to this project are documented in this file. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [Semantic Versioning](https://semver.org/).

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
