# Camera history recording

## Problem

The chamber camera is currently only viewable live, and only while the page
is open (the bridge connects to the printer's camera port on demand, per
viewer, and drops the connection when the last viewer leaves — so Bambu
Studio can use the camera the rest of the time). There's no way to look back
at what happened while nobody was watching, and no per-print record of a
job.

## Goal

Keep the camera connected continuously and record it into a rolling buffer,
so the app can:

- Scrub back through a continuous window of recent footage (a DVR-style
  buffer), and
- Jump straight to a given print job and fast-forward through just that
  job's footage (a "timelapse" view).

**Accepted tradeoff:** holding the camera connection continuously means
Bambu Studio's live camera view will not work while bambu-util is running,
since the printer's camera port only serves one client. This is a deliberate
change from the current on-demand behavior.

## Architecture

### Recorder replaces the on-demand Hub

Today `Hub` opens the camera connection when the first viewer attaches and
closes it when the last one leaves. That lifecycle goes away: the recorder
connects at process startup and keeps retrying/reconnecting for the life of
the process, independent of viewers. Live viewers still attach/detach to
receive the real-time frame broadcast exactly as today — they just no longer
control the connection.

Every frame from the camera (~1fps, unchanged) is:

1. broadcast to any attached live viewers (existing behavior), and
2. written to a SQLite-backed store (new).

### Storage — new `internal/history` package

```sql
CREATE TABLE frames (
  id  INTEGER PRIMARY KEY,
  ts  INTEGER NOT NULL,   -- unix seconds
  jpeg BLOB NOT NULL
);
CREATE INDEX frames_ts ON frames(ts);

CREATE TABLE jobs (
  id       INTEGER PRIMARY KEY,
  name     TEXT NOT NULL,
  start_ts INTEGER NOT NULL,
  end_ts   INTEGER          -- NULL while the job is still running
);
```

Uses `modernc.org/sqlite`, a pure-Go SQLite driver — required because the
Dockerfile builds with `CGO_ENABLED=0` against a distroless base image with
no C toolchain.

A small poller watches `StateCache.Snapshot()` for `gcode_state`
transitions into/out of `RUNNING` (reusing `p1s.GcodeState` and
`p1s.JobName`) and opens/closes `jobs` rows accordingly.

A pruner goroutine runs on a periodic tick (every few minutes) and deletes
frames — and fully-expired job rows — older than `now - RECORDING_RETENTION`.

### Config

Two new environment variables, consistent with the project's env-var-only
config style:

| Variable | Required | Default | Description |
|---|---|---|---|
| `DATA_DIR` | no | `./data` | Directory for the SQLite file. Needs a mounted volume to survive container restarts. |
| `RECORDING_RETENTION` | no | `24h` | How long to retain recorded frames, parsed as a Go duration (e.g. `12h`, `48h`). |

## API

Three new endpoints on the existing server:

- `GET /camera/history/range` → `{"oldest": <unix|null>, "newest": <unix|null>}`,
  the span of timestamps currently in the buffer. Both null if empty.
- `GET /camera/history/frame?ts=<unix>` → the JPEG with the closest
  timestamp at or after `ts`. 404 if none exists at or after that point.
- `GET /camera/history/jobs` → jobs still within the retention window:
  `[{"id": ..., "name": ..., "start": ..., "end": <unix|null>}]`. `end` is
  null for a job still in progress.

No new streaming protocol — playback is client-driven (see below).

## Playback UI

Live view and History become two sections/tabs on the existing page.

History, on load, fetches `/camera/history/range` and `/camera/history/jobs`
and shows:

- A scrub bar spanning `oldest`–`newest`.
- Play/pause and a speed selector (1x / 5x / 20x / 60x).
- A list of recent jobs.

Playing advances a virtual timestamp on a timer at the selected speed and
re-fetches `/camera/history/frame?ts=...` each tick to update the displayed
image — the same "one JPEG in, display it" pattern the page already uses for
live view.

Clicking a job seeks the scrubber to that job's `start`, and auto-plays at a
faster default speed (e.g. 20x) up to its `end` (or up to `now` if it's the
in-progress job). That is the per-job "timelapse" — same playback primitive
as the DVR scrubber, just pre-seeked and bounded to one job's range.

## Edge cases

- **Empty buffer** (just started, no frames yet): range endpoint returns
  nulls; UI shows "no recordings yet" instead of a scrub bar.
- **Scrub position with no frame at or after it** (e.g. dragged past
  `newest`, or into a gap left by a camera reconnect with nothing after it):
  frame endpoint 404s; UI holds the last frame it successfully displayed.
- **Recorder loses the camera connection**: same retry/backoff loop `Hub`
  already has today. Gaps in the buffer are just gaps — no special handling.
- **DB write failure** (disk full, etc.): log and drop the frame; must never
  take down live view or the server.

## Testing

- `internal/history`: table-driven unit tests for insert / prune /
  nearest-frame-at-or-after / job open-close, matching the existing style in
  `camera_test.go` / `hub_test.go`.
- `internal/web`: server tests for the three new endpoints against a seeded
  store, matching the existing style in `server_test.go`.
