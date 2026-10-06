//! Caps the database file size by deleting the oldest data across all sources
//! (camera frames, event log), overriding their own retention. Freed pages are
//! returned to the disk with incremental vacuum, a chunk at a time so the
//! database isn't locked for long. The database is created in incremental
//! auto-vacuum mode, so there is nothing to convert first.

use crate::db::Db;

/// One deletable row, with its age and size.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Item {
    pub id: i64,
    /// Unix milliseconds, so sources of different precision compare.
    pub when: i64,
    pub bytes: i64,
}

/// One kind of data the cap may delete from.
pub trait Source: Send + Sync {
    fn name(&self) -> &'static str;
    /// At most `n` items, oldest first.
    fn oldest(&self, n: usize) -> rusqlite::Result<Vec<Item>>;
    /// Removes every item up to and including `id`.
    fn delete_through(&self, id: i64) -> rusqlite::Result<()>;
}

/// How far under the limit a pass cuts.
const LOW_WATER: f64 = 0.9;
/// How many pages one incremental vacuum returns, kept small so the write lock
/// is held briefly.
const RECLAIM_CHUNK: i64 = 256;
/// How many items are read from each source in one round.
const BATCH: usize = 2000;
/// Bounds one pass; a database far over its limit comes down over several.
const MAX_ROUNDS: usize = 8;

/// Holds one database to a limit.
pub struct Enforcer {
    db: Db,
    sources: Vec<Box<dyn Source>>,
    /// Bytes; zero means no limit. Read on each pass.
    limit: Box<dyn Fn() -> i64 + Send + Sync>,
}

impl Enforcer {
    pub fn new(
        db: Db,
        limit: impl Fn() -> i64 + Send + Sync + 'static,
        sources: Vec<Box<dyn Source>>,
    ) -> Enforcer {
        Enforcer {
            db,
            sources,
            limit: Box::new(limit),
        }
    }

    /// Runs a single pass.
    pub fn once(&self) -> Result<(), String> {
        let limit = (self.limit)();
        if limit <= 0 {
            return Ok(()); // switched off: nothing is deleted and nothing is compacted
        }
        if self.size()? <= limit {
            return Ok(());
        }
        let target = (limit as f64 * LOW_WATER) as i64;
        for _ in 0..MAX_ROUNDS {
            let size = self.size()?;
            if size <= target {
                return Ok(());
            }
            let deleted = self.delete_oldest(size - target)?;
            self.reclaim()?;
            if deleted == 0 {
                let after = self.size()?;
                if after > limit {
                    tracing::warn!(
                        "capacity: database is {} MB against a {} MB limit and there is nothing left to delete",
                        after >> 20,
                        limit >> 20
                    );
                }
                return Ok(());
            }
        }
        Ok(())
    }

    /// The main database file's size (not counting the WAL).
    fn size(&self) -> Result<i64, String> {
        let conn = self.db.lock();
        let pages: i64 = conn
            .query_row("PRAGMA page_count", [], |r| r.get(0))
            .map_err(|e| format!("page count: {e}"))?;
        let size: i64 = conn
            .query_row("PRAGMA page_size", [], |r| r.get(0))
            .map_err(|e| format!("page size: {e}"))?;
        Ok(pages * size)
    }

    /// Removes the oldest items across all sources until `want` bytes are
    /// freed, and reports how many it deleted.
    fn delete_oldest(&self, want: i64) -> Result<usize, String> {
        let mut all = Vec::new();
        for (i, source) in self.sources.iter().enumerate() {
            let items = source
                .oldest(BATCH)
                .map_err(|e| format!("{}: {e}", source.name()))?;
            all.extend(items.into_iter().map(|item| (item, i)));
        }
        all.sort_by_key(|(item, _)| item.when);

        // One cut-off per source: the last of its items the walk reached.
        let mut through = vec![None; self.sources.len()];
        let (mut freed, mut count) = (0, 0);
        for (item, source) in all {
            if freed >= want {
                break;
            }
            through[source] = Some(item.id);
            freed += item.bytes;
            count += 1;
        }
        for (source, id) in self.sources.iter().zip(through) {
            if let Some(id) = id {
                source
                    .delete_through(id)
                    .map_err(|e| format!("{}: {e}", source.name()))?;
            }
        }
        Ok(count)
    }

    /// Returns freed pages to the disk a chunk at a time, stopping when the
    /// free list is empty.
    fn reclaim(&self) -> Result<(), String> {
        loop {
            let conn = self.db.lock();
            let free: i64 = conn
                .query_row("PRAGMA freelist_count", [], |r| r.get(0))
                .map_err(|e| format!("free list: {e}"))?;
            if free == 0 {
                return Ok(());
            }
            conn.execute_batch(&format!("PRAGMA incremental_vacuum({RECLAIM_CHUNK})"))
                .map_err(|e| format!("return pages: {e}"))?;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::activity::{self, Log};
    use crate::clock;
    use crate::history::Store;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicI64, Ordering};

    struct Fixture {
        db: Db,
        frames: Store,
        events: Log,
        limit: Arc<AtomicI64>,
        enforcer: Enforcer,
    }

    fn fixture() -> Fixture {
        let path = crate::testing::tempdir().join("cap.sqlite");
        let db = Db::open(path).unwrap();
        let frames = Store::new(db.clone());
        let events = Log::new(db.clone(), || 1 << 30, clock::system()).unwrap();
        let limit = Arc::new(AtomicI64::new(0));
        let l = limit.clone();
        let enforcer = Enforcer::new(
            db.clone(),
            move || l.load(Ordering::SeqCst),
            vec![Box::new(frames.clone()), Box::new(events.clone())],
        );
        Fixture {
            db,
            frames,
            events,
            limit,
            enforcer,
        }
    }

    impl Fixture {
        /// 400 frames of 8 KB a second apart, with a log entry every ten.
        fn fill(&self) {
            for i in 0..400 {
                self.frames.insert_frame(i, &[0u8; 8000]).unwrap();
            }
            for i in 0..40 {
                self.db
                    .lock()
                    .execute(
                        "INSERT INTO activity (at, kind, summary, payload) VALUES (?, 'report', 'report', ?)",
                        rusqlite::params![i * 10_000 + 5, "x".repeat(2000)],
                    )
                    .unwrap();
            }
        }
        fn size(&self) -> i64 {
            self.enforcer.size().unwrap()
        }
    }

    #[test]
    fn deleting_alone_does_not_shrink_the_file_and_reclaiming_does() {
        let f = fixture();
        f.fill();
        let before = f.size();
        f.frames.delete_through(300).unwrap();
        assert_eq!(f.size(), before);
        f.enforcer.reclaim().unwrap();
        assert!(f.size() < before);
    }

    #[test]
    fn holds_the_real_stores_to_a_size_oldest_first() {
        let f = fixture();
        f.fill();
        f.frames.open_job("benchy.gcode", 0).unwrap();
        let limit = f.size() / 2;
        f.limit.store(limit, Ordering::SeqCst);
        f.enforcer.once().unwrap();
        assert!(f.size() <= limit);
        let (oldest, newest) = f.frames.range().unwrap();
        assert!(
            oldest.unwrap() > 0,
            "the cap is absolute: a print's footage is not protected"
        );
        assert_eq!(newest, Some(399));
        let entries = f.events.entries(100);
        assert!(
            !entries.is_empty() && entries.len() < 40,
            "both sources gave something up"
        );
        // Everything left in either source is newer than everything deleted.
        let oldest_entry = entries.last().unwrap().at;
        assert!(oldest_entry / 1000 >= oldest.unwrap() - 10);
    }

    #[test]
    fn under_its_limit_or_off_nothing_is_touched() {
        let f = fixture();
        f.fill();
        let size = f.size();
        f.limit.store(0, Ordering::SeqCst);
        f.enforcer.once().unwrap();
        f.limit.store(size * 2, Ordering::SeqCst);
        f.enforcer.once().unwrap();
        assert_eq!(f.frames.range().unwrap(), (Some(0), Some(399)));
        assert_eq!(f.events.entries(100).len(), 40);
    }

    #[test]
    fn stops_when_there_is_nothing_left_to_delete() {
        let f = fixture();
        f.fill();
        f.limit.store(1, Ordering::SeqCst);
        f.enforcer.once().unwrap();
        assert_eq!(f.frames.range().unwrap(), (None, None));
        assert!(f.events.entries(100).is_empty());
    }

    #[test]
    fn the_measurement_matches_the_file_on_disk() {
        let path = crate::testing::tempdir().join("m.sqlite");
        let db = Db::open(&path).unwrap();
        Store::new(db.clone())
            .insert_frame(1, &[0u8; 100_000])
            .unwrap();
        db.lock()
            .execute_batch("PRAGMA wal_checkpoint(TRUNCATE)")
            .unwrap();
        let e = Enforcer::new(db, || 0, vec![]);
        assert_eq!(
            e.size().unwrap(),
            std::fs::metadata(&path).unwrap().len() as i64
        );
    }

    #[test]
    fn sources_are_named_in_errors() {
        struct Broken;
        impl Source for Broken {
            fn name(&self) -> &'static str {
                "broken"
            }
            fn oldest(&self, _: usize) -> rusqlite::Result<Vec<Item>> {
                Err(rusqlite::Error::InvalidQuery)
            }
            fn delete_through(&self, _: i64) -> rusqlite::Result<()> {
                Ok(())
            }
        }
        let f = fixture();
        f.fill();
        let e = Enforcer::new(f.db.clone(), || 1, vec![Box::new(Broken)]);
        assert!(e.once().unwrap_err().starts_with("broken: "));
        let _ = activity::REPORT;
    }
}
