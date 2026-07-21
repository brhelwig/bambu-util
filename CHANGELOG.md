# Changelog

All notable changes to this project are documented in this file. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

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

[0.1.0]: https://github.com/brhelwig/bambu-util/releases/tag/v0.1.0
[0.0.1]: https://github.com/brhelwig/bambu-util/releases/tag/v0.0.1
