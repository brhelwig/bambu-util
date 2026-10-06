//! The Settings and Events screens.

use axum::body::Bytes;
use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::response::Response;
use serde::Deserialize;
use serde_json::json;

use super::{App, Query, json_reply, read_json, text};
use crate::p1s::Config;
use crate::settings::{self as keys, BYTES_PER_MB};

/// How many entries the Events screen is sent.
const SHOWN_EVENTS: usize = 500;

/// The current values in their stored units: seconds, megabytes or a count,
/// plus the dashboard layout as text. The printer's details are not here.
pub async fn get(State(app): State<App>) -> Response {
    let v = app.settings.values();
    json_reply(&json!({
        keys::RETENTION: v.retention,
        keys::KEPT_JOBS: v.kept_jobs,
        keys::BED_OFF_AFTER: v.bed_off_after,
        keys::NOZZLE_OFF_AFTER: v.nozzle_off_after,
        keys::LAMP_OFF_AFTER: v.lamp_off_after,
        keys::ACTIVITY_LIMIT: v.activity_limit / BYTES_PER_MB,
        keys::DATABASE_LIMIT: v.database_limit / BYTES_PER_MB,
        keys::SESSION_LENGTH: v.session_length,
        keys::DASHBOARD: v.dashboard,
    }))
}

/// Stores one value. Nothing has to be told: everything that consults a
/// setting does so at the point of use.
pub async fn set(State(app): State<App>, Path(name): Path<String>, query: Query) -> Response {
    // The printer's details go through their own endpoint, which also
    // reconnects; setting them here would store a printer the app is not
    // talking to.
    if keys::is_text(&name) {
        if name != keys::DASHBOARD {
            return text(StatusCode::BAD_REQUEST, "not writable here");
        }
        let value = query.get("text").map(String::as_str).unwrap_or("");
        return match app.settings.set_text(&name, value) {
            Ok(()) => StatusCode::NO_CONTENT.into_response(),
            Err(err) => text(StatusCode::BAD_REQUEST, err),
        };
    }
    let Some(value) = query.get("value").and_then(|v| v.parse::<i64>().ok()) else {
        return text(StatusCode::BAD_REQUEST, "invalid value");
    };
    match app.settings.set(&name, value) {
        Ok(()) => StatusCode::NO_CONTENT.into_response(),
        Err(err) => text(StatusCode::BAD_REQUEST, err),
    }
}

/// The configured printer. The access code is never sent back, only whether
/// one is set.
pub async fn printer(State(app): State<App>) -> Response {
    let conf = app.link.config();
    json_reply(&json!({
        "ip": conf.ip,
        "serial": conf.serial,
        "accessCodeSet": !conf.access_code.is_empty(),
        "configured": conf.complete(),
    }))
}

/// What the setup screen sends. An access code left out keeps the stored one,
/// so the form can be re-saved without retyping a secret it was never shown.
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct PrinterRequest {
    #[serde(default)]
    ip: String,
    #[serde(default)]
    serial: String,
    #[serde(default)]
    access_code: String,
}

pub async fn set_printer(State(app): State<App>, body: Bytes) -> Response {
    let Some(req) = read_json::<PrinterRequest>(&body) else {
        return text(StatusCode::BAD_REQUEST, "invalid request");
    };
    let mut access_code = req.access_code.trim().to_string();
    if access_code.is_empty() {
        access_code = app.link.config().access_code;
    }
    let conf = Config {
        ip: req.ip.trim().into(),
        serial: req.serial.trim().into(),
        access_code,
    };
    if !conf.complete() {
        return text(
            StatusCode::BAD_REQUEST,
            "the printer's address, serial and access code are all needed",
        );
    }
    for (name, value) in [
        (keys::PRINTER_IP, &conf.ip),
        (keys::PRINTER_SERIAL, &conf.serial),
        (keys::PRINTER_ACCESS_CODE, &conf.access_code),
    ] {
        if let Err(err) = app.settings.set_text(name, value) {
            return text(StatusCode::BAD_REQUEST, err);
        }
    }
    // Saving is not the same as reaching it: connecting happens in the
    // background and the status reports how it went, so a printer that is
    // merely switched off can still be set up.
    app.link.configure(conf);
    StatusCode::NO_CONTENT.into_response()
}

/// The most recent event-log entries, newest first.
pub async fn events(State(app): State<App>) -> Response {
    json_reply(&json!({"events": app.activity.entries(SHOWN_EVENTS)}))
}

use axum::response::IntoResponse;
