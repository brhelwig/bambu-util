//! Recorded camera frames and print-job boundaries, so the app can serve a
//! scrollback buffer and per-print timelapses.

use rusqlite::{OptionalExtension, params};

use crate::capacity::{Item, Source};
use crate::db::Db;
use crate::p1s;

/// The spacing, in seconds, that kept footage is reduced to once it ages past
/// the cutoff. Timelapses play at 30x or faster, so a frame every 10s is enough.
pub const THIN_INTERVAL: i64 = 10;

/// Bounds, in seconds, how far back from the cutoff an in-progress print's
/// footage is protected. A printer that vanishes mid-print can leave its job
/// row open forever; this stops that row exempting all later footage from
/// retention.
pub const MAX_OPEN_JOB_SPAN: i64 = 48 * 60 * 60;

/// One print's recorded time range, in unix seconds. `end` is None while it is
/// still in progress.
#[derive(Debug, Clone, PartialEq, serde::Serialize)]
pub struct Job {
    pub id: i64,
    pub name: String,
    pub start: i64,
    pub end: Option<i64>,
}

/// Camera frames and job boundaries.
#[derive(Clone)]
pub struct Store {
    db: Db,
}

/// A span whose frames survive the cutoff. `end` is None for a print still in
/// progress, meaning the span has no upper bound yet.
struct Window {
    start: i64,
    end: Option<i64>,
}

impl Store {
    pub fn new(db: Db) -> Store {
        Store { db }
    }

    /// Records one camera frame at a unix-second timestamp.
    pub fn insert_frame(&self, ts: i64, jpeg: &[u8]) -> rusqlite::Result<()> {
        self.db.lock().execute(
            "INSERT INTO frames (ts, jpeg) VALUES (?, ?)",
            params![ts, jpeg],
        )?;
        Ok(())
    }

    /// The frame with the smallest timestamp at or after `ts`, with that
    /// timestamp.
    pub fn frame_at_or_after(&self, ts: i64) -> rusqlite::Result<Option<(Vec<u8>, i64)>> {
        self.db
            .lock()
            .query_row(
                "SELECT jpeg, ts FROM frames WHERE ts >= ? ORDER BY ts ASC LIMIT 1",
                [ts],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .optional()
    }

    /// The oldest and newest frame timestamps stored.
    pub fn range(&self) -> rusqlite::Result<(Option<i64>, Option<i64>)> {
        self.db
            .lock()
            .query_row("SELECT MIN(ts), MAX(ts) FROM frames", [], |r| {
                Ok((r.get(0)?, r.get(1)?))
            })
    }

    /// Enforces retention. Frames older than `cutoff` are deleted unless they
    /// belong to a kept print — the one still in progress, or one of the
    /// `kept_jobs` most recently finished — whose footage is instead thinned to
    /// one frame per THIN_INTERVAL. Job rows go only once they are both older
    /// than the cutoff and no longer among the kept ones.
    pub fn prune(&self, cutoff: i64, kept_jobs: i64) -> rusqlite::Result<()> {
        let kept = self.kept_windows(cutoff, kept_jobs)?;
        let conn = self.db.lock();

        // Pre-cutoff frames inside none of the kept windows.
        let mut sql = String::from("DELETE FROM frames WHERE ts < ?");
        let mut args = vec![cutoff];
        for w in &kept {
            match w.end {
                None => {
                    sql.push_str(" AND ts < ?");
                    args.push(w.start);
                }
                Some(end) => {
                    sql.push_str(" AND NOT (ts >= ? AND ts <= ?)");
                    args.extend([w.start, end]);
                }
            }
        }
        conn.execute(&sql, rusqlite::params_from_iter(args))?;

        // The pre-cutoff part of each kept window, down to its earliest frame
        // per interval. Newer frames are left alone.
        for w in &kept {
            let end = w.end.map_or(cutoff - 1, |end| end.min(cutoff - 1));
            if end < w.start {
                continue;
            }
            conn.execute(
                "DELETE FROM frames WHERE ts >= ?1 AND ts <= ?2 AND id NOT IN (
                   SELECT MIN(id) FROM frames WHERE ts >= ?1 AND ts <= ?2 GROUP BY ts / ?3)",
                params![w.start, end, THIN_INTERVAL],
            )?;
        }

        conn.execute(
            "DELETE FROM jobs WHERE end_ts IS NOT NULL AND end_ts < ? AND id NOT IN (
               SELECT id FROM jobs WHERE end_ts IS NOT NULL ORDER BY start_ts DESC, id DESC LIMIT ?)",
            params![cutoff, kept_jobs],
        )?;
        Ok(())
    }

    fn kept_windows(&self, cutoff: i64, kept_jobs: i64) -> rusqlite::Result<Vec<Window>> {
        let conn = self.db.lock();
        let mut stmt = conn.prepare(
            "SELECT start_ts, end_ts FROM (
               SELECT start_ts, end_ts FROM jobs WHERE end_ts IS NULL ORDER BY start_ts DESC, id DESC LIMIT 1)
             UNION ALL
             SELECT start_ts, end_ts FROM (
               SELECT start_ts, end_ts FROM jobs WHERE end_ts IS NOT NULL ORDER BY start_ts DESC, id DESC LIMIT ?)",
        )?;
        stmt.query_map([kept_jobs], |r| {
            let (start, end): (i64, Option<i64>) = (r.get(0)?, r.get(1)?);
            // See MAX_OPEN_JOB_SPAN.
            let start = if end.is_none() {
                start.max(cutoff - MAX_OPEN_JOB_SPAN)
            } else {
                start
            };
            Ok(Window { start, end })
        })?
        .collect()
    }

    /// Records the start of a print and returns its id.
    pub fn open_job(&self, name: &str, start: i64) -> rusqlite::Result<i64> {
        let conn = self.db.lock();
        conn.execute(
            "INSERT INTO jobs (name, start_ts, end_ts) VALUES (?, ?, NULL)",
            params![name, start],
        )?;
        Ok(conn.last_insert_rowid())
    }

    pub fn close_job(&self, id: i64, end: i64) -> rusqlite::Result<()> {
        self.db
            .lock()
            .execute("UPDATE jobs SET end_ts = ? WHERE id = ?", params![end, id])?;
        Ok(())
    }

    /// The print still in progress — the newest row with no end — if any.
    pub fn active_job(&self) -> rusqlite::Result<Option<Job>> {
        self.db
            .lock()
            .query_row(
                "SELECT id, name, start_ts FROM jobs WHERE end_ts IS NULL ORDER BY start_ts DESC, id DESC LIMIT 1",
                [],
                |r| Ok(Job { id: r.get(0)?, name: r.get(1)?, start: r.get(2)?, end: None }),
            )
            .optional()
    }

    /// Closes job `id` at the last frame recorded inside it, or at `fallback`
    /// when it has none. For a row left open by a crash, that is when the print
    /// actually stopped being recorded.
    pub fn close_job_at_last_frame(&self, id: i64, fallback: i64) -> rusqlite::Result<()> {
        let end = {
            let conn = self.db.lock();
            let start: i64 =
                conn.query_row("SELECT start_ts FROM jobs WHERE id = ?", [id], |r| r.get(0))?;
            // Bounded at the next print's start so one row can't claim the
            // footage of everything recorded after it.
            let last: Option<i64> = conn.query_row(
                "SELECT MAX(ts) FROM frames WHERE ts >= ?1
                   AND ts < COALESCE((SELECT MIN(start_ts) FROM jobs WHERE start_ts > ?1), ?2)",
                params![start, i64::MAX],
                |r| r.get(0),
            )?;
            last.filter(|&last| last >= start).unwrap_or(fallback)
        };
        self.close_job(id, end)
    }

    /// Closes every open job row except the newest, reporting how many it
    /// closed. Only one print runs at a time, so any others are stale.
    pub fn close_orphan_jobs(&self) -> rusqlite::Result<usize> {
        let open: Vec<(i64, i64)> = {
            let conn = self.db.lock();
            let mut stmt =
                conn.prepare("SELECT id, start_ts FROM jobs WHERE end_ts IS NULL ORDER BY start_ts DESC, id DESC")?;
            stmt.query_map([], |r| Ok((r.get(0)?, r.get(1)?)))?
                .collect::<rusqlite::Result<_>>()?
        };
        // [0] is the newest: leave it open.
        for &(id, start) in open.iter().skip(1) {
            self.close_job_at_last_frame(id, start)?;
        }
        Ok(open.len().saturating_sub(1))
    }

    /// Every stored job, newest-started first (ties by insertion order).
    /// Pruning keeps this bounded.
    pub fn recent_jobs(&self) -> rusqlite::Result<Vec<Job>> {
        let conn = self.db.lock();
        let mut stmt = conn.prepare(
            "SELECT id, name, start_ts, end_ts FROM jobs ORDER BY start_ts DESC, id DESC",
        )?;
        stmt.query_map([], |r| {
            Ok(Job {
                id: r.get(0)?,
                name: r.get(1)?,
                start: r.get(2)?,
                end: r.get(3)?,
            })
        })?
        .collect()
    }
}

impl Source for Store {
    fn name(&self) -> &'static str {
        "camera frames"
    }

    /// Ordered by id to match `delete_through`. `when` is in milliseconds,
    /// like the event log's.
    fn oldest(&self, n: usize) -> rusqlite::Result<Vec<Item>> {
        let conn = self.db.lock();
        let mut stmt =
            conn.prepare("SELECT id, ts, octet_length(jpeg) FROM frames ORDER BY id ASC LIMIT ?")?;
        stmt.query_map([n as i64], |r| {
            Ok(Item {
                id: r.get(0)?,
                when: r.get::<_, i64>(1)? * 1000,
                bytes: r.get(2)?,
            })
        })?
        .collect()
    }

    /// Removes every frame up to and including `id`, including footage
    /// retention would keep.
    fn delete_through(&self, id: i64) -> rusqlite::Result<()> {
        self.db
            .lock()
            .execute("DELETE FROM frames WHERE id <= ?", [id])?;
        Ok(())
    }
}

/// Opens and closes job rows as the printer's state moves, so frames can be
/// grouped and played back per print.
pub struct JobTracker {
    store: Store,
    open: Option<(i64, String)>,
}

impl JobTracker {
    /// Adopts a row left open by an earlier process, so a restart mid-print
    /// doesn't record the print twice, and closes any older open rows.
    pub fn new(store: Store) -> JobTracker {
        match store.close_orphan_jobs() {
            Ok(0) => {}
            Ok(n) => {
                tracing::info!("history: closed {n} job row(s) left open by an earlier process")
            }
            Err(err) => tracing::warn!("history: closing orphan jobs: {err}"),
        }
        let open = match store.active_job() {
            Ok(job) => job.map(|j| (j.id, j.name)),
            Err(err) => {
                tracing::warn!("history: adopting the open job: {err}");
                None
            }
        };
        JobTracker { store, open }
    }

    /// Opens a row when a print starts and closes it when it ends, returning
    /// whether anything changed. Repeated calls in one state change nothing.
    /// States that are neither running nor ended ("unknown", PREPARE) leave the
    /// row alone.
    pub fn observe(&mut self, gcode_state: &str, job_name: &str, now: i64) -> bool {
        let mut changed = false;
        if p1s::job_active(gcode_state) {
            // A different name means a job boundary was missed (e.g. while the
            // app was down), so start a new row. An empty name is just a
            // partial report.
            if self
                .open
                .as_ref()
                .is_some_and(|(_, open)| !job_name.is_empty() && job_name != open)
            {
                changed |= self.close(now);
            }
            if self.open.is_none() {
                match self.store.open_job(job_name, now) {
                    Ok(id) => {
                        self.open = Some((id, job_name.to_string()));
                        changed = true;
                    }
                    Err(err) => tracing::warn!("history: opening a job: {err}"),
                }
            }
        } else if p1s::job_ended(gcode_state) && self.open.is_some() {
            changed |= self.close(now);
        }
        changed
    }

    fn close(&mut self, now: i64) -> bool {
        let Some((id, _)) = self.open else {
            return false;
        };
        match self.store.close_job_at_last_frame(id, now) {
            Ok(()) => {
                self.open = None;
                true
            }
            // Left open on purpose: the next report in the same state retries.
            Err(err) => {
                tracing::warn!("history: closing a job: {err}");
                false
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const KEPT: i64 = 5;

    fn store() -> Store {
        Store::new(Db::memory())
    }

    fn timestamps(s: &Store) -> Vec<i64> {
        let conn = s.db.lock();
        let mut stmt = conn
            .prepare("SELECT ts FROM frames ORDER BY ts ASC")
            .unwrap();
        stmt.query_map([], |r| r.get(0))
            .unwrap()
            .collect::<rusqlite::Result<_>>()
            .unwrap()
    }

    #[test]
    fn insert_and_frame_at_or_after() {
        let s = store();
        s.insert_frame(100, &[1]).unwrap();
        s.insert_frame(200, &[2]).unwrap();
        assert_eq!(s.frame_at_or_after(150).unwrap(), Some((vec![2], 200)));
        assert_eq!(s.frame_at_or_after(100).unwrap(), Some((vec![1], 100)));
        assert_eq!(s.frame_at_or_after(201).unwrap(), None);
    }

    #[test]
    fn range() {
        let s = store();
        assert_eq!(s.range().unwrap(), (None, None));
        s.insert_frame(300, &[1]).unwrap();
        s.insert_frame(100, &[1]).unwrap();
        assert_eq!(s.range().unwrap(), (Some(100), Some(300)));
    }

    #[test]
    fn prune_deletes_old_frames() {
        let s = store();
        s.insert_frame(100, &[1]).unwrap();
        s.insert_frame(500, &[2]).unwrap();
        s.prune(300, KEPT).unwrap();
        assert_eq!(s.range().unwrap(), (Some(500), Some(500)));
    }

    #[test]
    fn job_lifecycle() {
        let s = store();
        let id = s.open_job("benchy.3mf", 100).unwrap();
        assert_eq!(
            s.recent_jobs().unwrap(),
            vec![Job {
                id,
                name: "benchy.3mf".into(),
                start: 100,
                end: None
            }]
        );
        s.close_job(id, 200).unwrap();
        assert_eq!(s.recent_jobs().unwrap()[0].end, Some(200));
    }

    #[test]
    fn prune_preserves_an_ongoing_job_however_old() {
        let s = store();
        s.open_job("old-but-running.3mf", 0).unwrap();
        s.prune(1000, KEPT).unwrap();
        assert_eq!(s.recent_jobs().unwrap().len(), 1);
    }

    #[test]
    fn prune_keeps_the_newest_finished_jobs_and_drops_the_rest() {
        let s = store();
        for i in 0..=KEPT {
            let start = 100 + i * 10;
            let id = s.open_job(&format!("job{i}.3mf"), start).unwrap();
            s.close_job(id, start + 5).unwrap();
        }
        s.prune(100_000, KEPT).unwrap();
        let jobs = s.recent_jobs().unwrap();
        assert_eq!(jobs.len() as i64, KEPT);
        assert!(jobs.iter().all(|j| j.name != "job0.3mf"));
    }

    #[test]
    fn prune_keeps_and_thins_footage_of_kept_jobs() {
        let s = store();
        let id = s.open_job("kept.3mf", 1000).unwrap();
        s.close_job(id, 1099).unwrap();
        for ts in 1000..=1099 {
            s.insert_frame(ts, &[1]).unwrap();
        }
        s.prune(5000, KEPT).unwrap();
        let want: Vec<i64> = (0..100 / THIN_INTERVAL)
            .map(|i| 1000 + i * THIN_INTERVAL)
            .collect();
        assert_eq!(timestamps(&s), want);
    }

    #[test]
    fn prune_deletes_frames_outside_every_kept_job() {
        let s = store();
        let id = s.open_job("kept.3mf", 1000).unwrap();
        s.close_job(id, 1099).unwrap();
        for ts in [1000, 500, 2000] {
            s.insert_frame(ts, &[1]).unwrap();
        }
        s.prune(5000, KEPT).unwrap();
        assert_eq!(timestamps(&s), vec![1000]);
    }

    #[test]
    fn prune_leaves_frames_newer_than_cutoff_at_full_rate() {
        let s = store();
        s.open_job("running.3mf", 1000).unwrap();
        for ts in 1000..=1099 {
            s.insert_frame(ts, &[1]).unwrap();
        }
        s.prune(900, KEPT).unwrap();
        assert_eq!(timestamps(&s).len(), 100);
    }

    #[test]
    fn prune_thins_only_the_expired_part_of_a_running_job() {
        let s = store();
        s.open_job("long.3mf", 1000).unwrap();
        for ts in 1000..=1099 {
            s.insert_frame(ts, &[1]).unwrap();
        }
        s.prune(1050, KEPT).unwrap();
        let got = timestamps(&s);
        assert_eq!(got.len() as i64, 50 / THIN_INTERVAL + 50);
        assert_eq!(*got.last().unwrap(), 1099);
    }

    #[test]
    fn active_job() {
        let s = store();
        assert_eq!(s.active_job().unwrap(), None);
        let done = s.open_job("done.3mf", 100).unwrap();
        s.close_job(done, 200).unwrap();
        assert_eq!(s.active_job().unwrap(), None);
        let open = s.open_job("running.3mf", 300).unwrap();
        assert_eq!(
            s.active_job().unwrap(),
            Some(Job {
                id: open,
                name: "running.3mf".into(),
                start: 300,
                end: None
            })
        );
    }

    #[test]
    fn prune_bounds_how_far_back_an_open_job_protects_footage() {
        let s = store();
        const DAY: i64 = 86400;
        const NOW: i64 = 100 * DAY;
        s.open_job("stuck-open.3mf", NOW - 30 * DAY).unwrap();
        for d in (0..=30).rev() {
            s.insert_frame(NOW - d * DAY, &[1]).unwrap();
        }
        s.prune(NOW - DAY, KEPT).unwrap();
        let got = timestamps(&s);
        assert!(!got.is_empty());
        assert!(
            got[0] > NOW - 29 * DAY,
            "stuck-open row still exempts old footage: {got:?}"
        );
    }

    #[test]
    fn prune_keeps_a_whole_long_running_print() {
        let s = store();
        const HOUR: i64 = 3600;
        const NOW: i64 = 1_000_000;
        s.open_job("36h-print.3mf", NOW - 36 * HOUR).unwrap();
        for h in (0..=36).rev() {
            s.insert_frame(NOW - h * HOUR, &[1]).unwrap();
        }
        s.prune(NOW - 24 * HOUR, KEPT).unwrap();
        assert_eq!(timestamps(&s)[0], NOW - 36 * HOUR);
    }

    #[test]
    fn close_orphan_jobs_leaves_a_single_open_row_alone() {
        let s = store();
        s.open_job("running.3mf", 1000).unwrap();
        assert_eq!(s.close_orphan_jobs().unwrap(), 0);
        assert_eq!(s.recent_jobs().unwrap()[0].end, None);
    }

    #[test]
    fn close_orphan_jobs_closes_stranded_rows_at_their_last_frame() {
        let s = store();
        s.open_job("pencil.3mf", 1000).unwrap();
        s.open_job("pencil.3mf", 2000).unwrap();
        s.open_job("running.3mf", 3000).unwrap();
        for ts in [1000, 1500, 1900, 2000, 2400, 3000, 3500] {
            s.insert_frame(ts, &[1]).unwrap();
        }
        assert_eq!(s.close_orphan_jobs().unwrap(), 2);
        let jobs = s.recent_jobs().unwrap();
        assert_eq!((jobs[0].name.as_str(), jobs[0].end), ("running.3mf", None));
        assert_eq!(jobs[1].end, Some(2400));
        assert_eq!(jobs[2].end, Some(1900));
    }

    #[test]
    fn close_orphan_jobs_falls_back_to_start_without_footage() {
        let s = store();
        s.open_job("footage-already-pruned.3mf", 1000).unwrap();
        s.open_job("running.3mf", 5000).unwrap();
        s.close_orphan_jobs().unwrap();
        let jobs = s.recent_jobs().unwrap();
        assert_eq!(jobs[1].end, Some(1000));
    }

    #[test]
    fn prune_ignores_all_but_the_newest_open_row() {
        let s = store();
        s.open_job("stranded.3mf", 100).unwrap();
        s.open_job("running.3mf", 9000).unwrap();
        s.insert_frame(500, &[1]).unwrap();
        s.insert_frame(9500, &[2]).unwrap();
        s.prune(5000, KEPT).unwrap();
        assert_eq!(timestamps(&s), vec![9500]);
    }

    // The tracker, which replaces the old polling job watcher.

    fn tracked() -> (Store, JobTracker) {
        let s = store();
        let t = JobTracker::new(s.clone());
        (s, t)
    }

    #[test]
    fn tracker_opens_and_closes_on_transition() {
        let (s, mut t) = tracked();
        assert!(!t.observe("IDLE", "", 100));
        assert!(t.observe("RUNNING", "benchy.3mf", 200));
        assert!(!t.observe("RUNNING", "benchy.3mf", 300));
        assert!(t.observe("FINISH", "benchy.3mf", 400));
        assert!(!t.observe("FINISH", "benchy.3mf", 500));
        let jobs = s.recent_jobs().unwrap();
        assert_eq!(jobs.len(), 1);
        assert_eq!((jobs[0].start, jobs[0].end), (200, Some(400)));
    }

    #[test]
    fn tracker_keeps_one_row_across_pause_and_resume() {
        let (s, mut t) = tracked();
        t.observe("RUNNING", "a", 100);
        t.observe("PAUSE", "a", 200);
        t.observe("RUNNING", "a", 300);
        assert_eq!(s.recent_jobs().unwrap().len(), 1);
        assert_eq!(s.recent_jobs().unwrap()[0].end, None);
    }

    #[test]
    fn tracker_adopts_a_row_left_open_by_an_earlier_process() {
        let s = store();
        s.open_job("a", 100).unwrap();
        let mut t = JobTracker::new(s.clone());
        t.observe("RUNNING", "a", 200);
        assert_eq!(s.recent_jobs().unwrap().len(), 1);
        t.observe("FINISH", "a", 300);
        assert_eq!(s.recent_jobs().unwrap()[0].end, Some(300));
    }

    #[test]
    fn tracker_closes_rows_stranded_by_an_earlier_process() {
        let s = store();
        s.open_job("a", 100).unwrap();
        s.open_job("b", 200).unwrap();
        JobTracker::new(s.clone());
        let jobs = s.recent_jobs().unwrap();
        assert_eq!((jobs[0].end, jobs[1].end), (None, Some(100)));
    }

    #[test]
    fn tracker_starts_a_fresh_row_when_a_different_print_is_running() {
        let (s, mut t) = tracked();
        t.observe("RUNNING", "a", 100);
        t.observe("RUNNING", "b", 200);
        let jobs = s.recent_jobs().unwrap();
        assert_eq!(jobs.len(), 2);
        assert_eq!((jobs[0].name.as_str(), jobs[0].end), ("b", None));
        assert_eq!(jobs[1].end, Some(200));
    }

    #[test]
    fn tracker_keeps_the_open_row_when_the_name_is_missing() {
        let (s, mut t) = tracked();
        t.observe("RUNNING", "a", 100);
        t.observe("RUNNING", "", 200);
        assert_eq!(s.recent_jobs().unwrap().len(), 1);
    }

    #[test]
    fn tracker_ignores_unknown_and_prepare() {
        let (s, mut t) = tracked();
        t.observe("unknown", "", 100);
        assert!(s.recent_jobs().unwrap().is_empty());
        t.observe("RUNNING", "a", 200);
        t.observe("PREPARE", "a", 300);
        t.observe("unknown", "a", 400);
        assert_eq!(s.recent_jobs().unwrap()[0].end, None);
    }

    #[test]
    fn tracker_closes_at_the_last_recorded_frame_or_now() {
        let (s, mut t) = tracked();
        t.observe("RUNNING", "a", 100);
        s.insert_frame(150, &[1]).unwrap();
        t.observe("FINISH", "a", 500);
        t.observe("RUNNING", "b", 600);
        t.observe("FAILED", "b", 700);
        let jobs = s.recent_jobs().unwrap();
        assert_eq!(jobs[1].end, Some(150));
        assert_eq!(jobs[0].end, Some(700));
    }
}
