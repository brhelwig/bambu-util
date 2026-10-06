//! The app's configuration, stored in the database so it can be changed from
//! the page. Everything that consults a setting reads it at the point of use,
//! so an edit takes effect without a restart.

use std::collections::HashMap;
use std::sync::{Arc, RwLock};

use rusqlite::params;

use crate::db::Db;

// Names of the settings, as the page sends them.
pub const RETENTION: &str = "retention";
pub const KEPT_JOBS: &str = "kept-jobs";
pub const BED_OFF_AFTER: &str = "bed-off-after";
pub const NOZZLE_OFF_AFTER: &str = "nozzle-off-after";
pub const LAMP_OFF_AFTER: &str = "lamp-off-after";
pub const ACTIVITY_LIMIT: &str = "activity-limit";
pub const DATABASE_LIMIT: &str = "database-limit";
pub const SESSION_LENGTH: &str = "session-length";

pub const PRINTER_IP: &str = "printer-ip";
pub const PRINTER_SERIAL: &str = "printer-serial";
pub const PRINTER_ACCESS_CODE: &str = "printer-access-code";
/// The printer screen's sections, in order. Anything absent is hidden.
pub const DASHBOARD: &str = "dashboard";

const TEXTS: &[&str] = &[PRINTER_IP, PRINTER_SERIAL, PRINTER_ACCESS_CODE, DASHBOARD];

/// Whether a setting holds words rather than a number.
pub fn is_text(name: &str) -> bool {
    TEXTS.contains(&name)
}

/// Converts the stored megabytes to bytes.
pub const BYTES_PER_MB: i64 = 1 << 20;

/// One complete set of settings. Durations are in seconds, limits in bytes.
/// `access_code` is a credential and must never be sent to the browser.
#[derive(Clone, Debug, PartialEq)]
pub struct Values {
    pub printer_ip: String,
    pub printer_serial: String,
    pub access_code: String,
    pub dashboard: String,

    pub retention: i64,
    pub kept_jobs: i64,
    pub bed_off_after: i64,
    pub nozzle_off_after: i64,
    pub lamp_off_after: i64,
    pub activity_limit: i64,
    /// Zero when the cap is off.
    pub database_limit: i64,
    pub session_length: i64,
}

impl Default for Values {
    /// What an unconfigured app runs with, and what a value that cannot be
    /// read falls back to.
    fn default() -> Self {
        Values {
            printer_ip: String::new(),
            printer_serial: String::new(),
            access_code: String::new(),
            dashboard: String::new(),
            retention: 24 * 3600,
            kept_jobs: 5,
            bed_off_after: 24 * 3600,
            nozzle_off_after: 15 * 60,
            lamp_off_after: 8 * 3600,
            activity_limit: 64 * BYTES_PER_MB,
            database_limit: 0,
            session_length: 14 * 24 * 3600,
        }
    }
}

#[derive(Clone, Copy)]
enum Unit {
    Count,
    Seconds,
    Megabytes,
}

/// A numeric setting's unit and allowed range. `off_at_zero` settings also
/// accept 0, meaning off.
#[derive(Clone, Copy)]
struct Spec {
    unit: Unit,
    min: i64,
    max: i64,
    off_at_zero: bool,
}

impl Spec {
    const fn new(unit: Unit, min: i64, max: i64) -> Spec {
        Spec {
            unit,
            min,
            max,
            off_at_zero: false,
        }
    }

    fn allows(&self, value: i64) -> bool {
        (self.off_at_zero && value == 0) || (self.min..=self.max).contains(&value)
    }

    /// Says what the setting will take, in the units it is written in.
    fn refuse(&self, name: &str) -> String {
        let (min, max) = (self.show(self.min), self.show(self.max));
        if self.off_at_zero {
            format!("{name} must be 0 to switch it off, or between {min} and {max}")
        } else {
            format!("{name} must be between {min} and {max}")
        }
    }

    fn show(&self, value: i64) -> String {
        match self.unit {
            Unit::Count => value.to_string(),
            Unit::Seconds => show_duration(value),
            Unit::Megabytes => format!("{value} MB"),
        }
    }
}

/// Renders seconds as hours, minutes and seconds, e.g. `1h0m0s`.
fn show_duration(secs: i64) -> String {
    let (h, m, s) = (secs / 3600, secs % 3600 / 60, secs % 60);
    if h > 0 {
        format!("{h}h{m}m{s}s")
    } else if m > 0 {
        format!("{m}m{s}s")
    } else {
        format!("{s}s")
    }
}

const DAY: i64 = 24 * 3600;

fn spec(name: &str) -> Option<Spec> {
    Some(match name {
        RETENTION => Spec::new(Unit::Seconds, 3600, 30 * DAY),
        KEPT_JOBS => Spec::new(Unit::Count, 0, 50),
        // The bed and lamp are set in whole hours on the Settings screen.
        BED_OFF_AFTER => Spec::new(Unit::Seconds, 3600, 7 * DAY),
        NOZZLE_OFF_AFTER => Spec::new(Unit::Seconds, 60, 7 * DAY),
        LAMP_OFF_AFTER => Spec::new(Unit::Seconds, 3600, 7 * DAY),
        ACTIVITY_LIMIT => Spec::new(Unit::Megabytes, 1, 512),
        SESSION_LENGTH => Spec::new(Unit::Seconds, DAY, 365 * DAY),
        DATABASE_LIMIT => Spec {
            off_at_zero: true,
            ..Spec::new(Unit::Megabytes, 256, 64 * 1024)
        },
        _ => return None,
    })
}

/// Reads and writes the settings, caching the current values in memory.
#[derive(Clone)]
pub struct Settings {
    db: Db,
    values: Arc<RwLock<Values>>,
}

impl Settings {
    pub fn new(db: Db) -> rusqlite::Result<Settings> {
        let s = Settings {
            db,
            values: Arc::default(),
        };
        s.reload()?;
        Ok(s)
    }

    /// The current settings.
    pub fn values(&self) -> Values {
        self.values
            .read()
            .unwrap_or_else(|p| p.into_inner())
            .clone()
    }

    /// Stores one numeric setting, in seconds, megabytes or a count depending
    /// on the setting.
    pub fn set(&self, name: &str, value: i64) -> Result<(), String> {
        let spec = spec(name).ok_or_else(|| format!("settings: unknown setting {name:?}"))?;
        if !spec.allows(value) {
            return Err(spec.refuse(name));
        }
        self.put(name, &value.to_string())
    }

    /// Stores one setting that holds words. An empty value clears it, which is
    /// how a printer is forgotten.
    pub fn set_text(&self, name: &str, value: &str) -> Result<(), String> {
        if !is_text(name) {
            return Err(format!("settings: {name:?} does not hold text"));
        }
        if value.len() > 512 {
            return Err(format!("{name} is too long"));
        }
        if value.is_empty() {
            self.db
                .lock()
                .execute("DELETE FROM settings WHERE name = ?", [name])
                .map_err(|e| e.to_string())?;
            return self.reload().map_err(|e| e.to_string());
        }
        self.put(name, value)
    }

    fn put(&self, name: &str, value: &str) -> Result<(), String> {
        self.db
            .lock()
            .execute(
                "INSERT INTO settings (name, value) VALUES (?, ?)
                 ON CONFLICT(name) DO UPDATE SET value = excluded.value",
                params![name, value],
            )
            .map_err(|e| e.to_string())?;
        self.reload().map_err(|e| e.to_string())
    }

    /// Reads every setting back into memory. An invalid stored value falls
    /// back to its default.
    fn reload(&self) -> rusqlite::Result<()> {
        let stored: HashMap<String, String> = {
            let conn = self.db.lock();
            let mut stmt = conn.prepare("SELECT name, value FROM settings")?;
            stmt.query_map([], |row| Ok((row.get(0)?, row.get(1)?)))?
                .collect::<rusqlite::Result<_>>()?
        };
        let d = Values::default();
        let num = |name: &str, fallback: i64| {
            stored
                .get(name)
                .and_then(|raw| raw.parse::<i64>().ok())
                .filter(|n| spec(name).is_some_and(|s| s.allows(*n)))
                .unwrap_or(fallback)
        };
        let text = |name: &str| stored.get(name).cloned().unwrap_or_default();
        let v = Values {
            printer_ip: text(PRINTER_IP),
            printer_serial: text(PRINTER_SERIAL),
            access_code: text(PRINTER_ACCESS_CODE),
            dashboard: text(DASHBOARD),
            retention: num(RETENTION, d.retention),
            kept_jobs: num(KEPT_JOBS, d.kept_jobs),
            bed_off_after: num(BED_OFF_AFTER, d.bed_off_after),
            nozzle_off_after: num(NOZZLE_OFF_AFTER, d.nozzle_off_after),
            lamp_off_after: num(LAMP_OFF_AFTER, d.lamp_off_after),
            activity_limit: num(ACTIVITY_LIMIT, d.activity_limit / BYTES_PER_MB) * BYTES_PER_MB,
            database_limit: num(DATABASE_LIMIT, d.database_limit / BYTES_PER_MB) * BYTES_PER_MB,
            session_length: num(SESSION_LENGTH, d.session_length),
        };
        *self.values.write().unwrap_or_else(|p| p.into_inner()) = v;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn store() -> Settings {
        Settings::new(Db::memory()).unwrap()
    }

    #[test]
    fn defaults_when_nothing_stored() {
        assert_eq!(store().values(), Values::default());
    }

    #[test]
    fn set_is_read_back_in_its_units() {
        let s = store();
        s.set(RETENTION, 7200).unwrap();
        s.set(KEPT_JOBS, 0).unwrap();
        s.set(ACTIVITY_LIMIT, 2).unwrap();
        s.set(DATABASE_LIMIT, 256).unwrap();
        let v = s.values();
        assert_eq!(v.retention, 7200);
        assert_eq!(v.kept_jobs, 0);
        assert_eq!(v.activity_limit, 2 * BYTES_PER_MB);
        assert_eq!(v.database_limit, 256 * BYTES_PER_MB);
    }

    #[test]
    fn values_survive_reopening() {
        let db = Db::memory();
        Settings::new(db.clone())
            .unwrap()
            .set(NOZZLE_OFF_AFTER, 60)
            .unwrap();
        assert_eq!(Settings::new(db).unwrap().values().nozzle_off_after, 60);
    }

    #[test]
    fn refusals_read_in_the_settings_units() {
        let s = store();
        assert_eq!(
            s.set(RETENTION, 10).unwrap_err(),
            "retention must be between 1h0m0s and 720h0m0s"
        );
        assert_eq!(
            s.set(NOZZLE_OFF_AFTER, 1).unwrap_err(),
            "nozzle-off-after must be between 1m0s and 168h0m0s"
        );
        assert_eq!(
            s.set(KEPT_JOBS, 51).unwrap_err(),
            "kept-jobs must be between 0 and 50"
        );
        assert_eq!(
            s.set(ACTIVITY_LIMIT, 0).unwrap_err(),
            "activity-limit must be between 1 MB and 512 MB"
        );
        assert_eq!(
            s.set(DATABASE_LIMIT, 10).unwrap_err(),
            "database-limit must be 0 to switch it off, or between 256 MB and 65536 MB"
        );
        assert_eq!(
            s.set("nope", 1).unwrap_err(),
            "settings: unknown setting \"nope\""
        );
        assert_eq!(s.values(), Values::default());
    }

    #[test]
    fn database_limit_zero_is_off() {
        let s = store();
        s.set(DATABASE_LIMIT, 1024).unwrap();
        s.set(DATABASE_LIMIT, 0).unwrap();
        assert_eq!(s.values().database_limit, 0);
    }

    #[test]
    fn invalid_stored_values_fall_back() {
        let db = Db::memory();
        db.lock()
            .execute_batch("INSERT INTO settings VALUES ('retention', 'x'), ('kept-jobs', '999')")
            .unwrap();
        let v = Settings::new(db).unwrap().values();
        assert_eq!(v.retention, Values::default().retention);
        assert_eq!(v.kept_jobs, Values::default().kept_jobs);
    }

    #[test]
    fn text_settings() {
        let s = store();
        s.set_text(PRINTER_IP, "10.0.0.5").unwrap();
        s.set_text(DASHBOARD, "camCard,jobCard").unwrap();
        assert_eq!(s.values().printer_ip, "10.0.0.5");
        assert_eq!(s.values().dashboard, "camCard,jobCard");
        s.set_text(PRINTER_IP, "").unwrap();
        assert_eq!(s.values().printer_ip, "");
        assert_eq!(
            s.set_text(DASHBOARD, &"x".repeat(513)).unwrap_err(),
            "dashboard is too long"
        );
        assert!(s.set_text(RETENTION, "1").is_err());
        assert!(is_text(PRINTER_ACCESS_CODE) && !is_text(RETENTION));
    }
}
