# Print job fields — design

## Problem

`StateCache` merges the printer's entire raw MQTT `print` report, but
`Server.status` only reads 6 keys back out (`gcode_state`, bed/nozzle temps,
`mc_percent`). Job name, layer progress, time remaining, chamber temp, fan
speeds, AMS filament state, and HMS health errors are already landing in the
cache but are never surfaced.

## Backend — `internal/web/server.go` `status` handler

Add new keys to the `/api/status` JSON response, following the existing
pattern of raw passthrough from `StateCache` (no backend-side unit
conversion, same as `bedTemp`/`nozzleTemp` today):

- `jobName` — `fields["subtask_name"]`, falls back to `fields["gcode_file"]`
  if empty
- `layerNum` / `totalLayerNum` — `fields["layer_num"]` /
  `fields["total_layer_num"]`
- `remainingMinutes` — `fields["mc_remaining_time"]`
- `chamberTemp` — `fields["chamber_temper"]`
- `wifiSignal` — `fields["wifi_signal"]` (raw string, e.g. `"-45dBm"`)
- `fans` — `{cooling: fields["cooling_fan_speed"], aux:
  fields["big_fan1_speed"], chamber: fields["big_fan2_speed"]}`, passed
  through raw and unlabeled as a unit — it is not verified whether Bambu
  reports these as 0–15 gear values or already-scaled percentages
- `ams` — `fields["ams"]` passed through raw (whatever nested shape the
  printer sends); the frontend walks it defensively
- `hms` — `fields["hms"]` mapped through a lookup table (below); entries
  with no table match fall back to displaying the raw code

## `internal/p1s/hms_codes.go` (new file)

A small `map[string]string` of known HMS code → human message pairs (e.g.
AMS filament runout, nozzle clog), sourced from community docs
(`ha-bambulab`/`pybambu`), not verified against this printer's actual
payload. Exposed as a pure function:

```go
func HMSMessage(code string) (string, bool)
```

Unit-tested table-driven, same style as `guard_test.go`. The table starts
small and grows as real codes are observed in practice.

## Frontend — `internal/web/static/index.html`

- New rows in the existing status card: Job name, Layers (`n / total`), Time
  remaining, Chamber temp, Wifi signal, and three separate fan-speed rows
  (cooling / aux / chamber).
- New AMS section below the existing card: one row per slot with a color
  swatch (from the slot's reported color) and material label. Rendered
  defensively — a slot missing expected sub-keys is skipped rather than
  causing a rendering error.
- New warning banner above the log line, hidden by default (`display:
  none`), shown only when the `hms` array is non-empty. Lists translated
  messages, falling back to the raw code for anything not in the lookup
  table.

## Known risk

AMS and HMS field shapes are based on community documentation, not verified
against a real payload from this printer — this repo has never captured
one. Backend and frontend code must be defensive (skip/omit on unexpected
shape rather than crash). The AMS/HMS UI may need adjustment once a real
payload with those fields populated is observed in practice.

## Out of scope

- Any Go struct/schema for the full `print` report — the cache stays an
  untyped `map[string]any`, matching current code.
- Camera auto-start (tracked separately).
