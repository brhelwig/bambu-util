//! The recorded camera footage.

use axum::extract::State;
use axum::http::{StatusCode, header};
use axum::response::{IntoResponse, Response};
use serde_json::{Value, json};

use super::{App, Query, json_reply, text};
use crate::clock;
use crate::p1s;

/// How far before a running print's start the scrub bar begins, so the whole
/// job is one drag of the bar with nothing earlier in the way.
pub const JOB_LEAD_IN: i64 = 5 * 60;

/// The unix second a served frame was actually captured: the first at or after
/// the one requested, which retention gaps can make hours later.
pub const FRAME_TIMESTAMP: &str = "X-Frame-Timestamp";

pub async fn range(State(app): State<App>) -> Response {
    match range_of(&app) {
        Ok(v) => json_reply(&v),
        Err(_) => text(StatusCode::INTERNAL_SERVER_ERROR, "range query failed"),
    }
}

/// The span the scrub bar covers. Footage held beyond it — kept prints'
/// thinned timelapses — is reached by picking a job.
pub fn range_of(app: &App) -> rusqlite::Result<Value> {
    let (mut oldest, newest) = app.store.range()?;
    if let (Some(old), Some(new)) = (oldest, newest) {
        let start = seek_start(app);
        if start > old {
            // A print that started after the last recorded frame (camera down
            // when it began) would otherwise put the start past the end.
            oldest = Some(start.min(new));
        }
    }
    Ok(json!({"oldest": oldest, "newest": newest}))
}

/// Just before the running print, or one retention window back when idle.
fn seek_start(app: &App) -> i64 {
    let snap = app.cache.snapshot();
    if p1s::job_active(p1s::gcode_state(&snap.fields))
        && let Ok(Some(job)) = app.store.active_job()
    {
        return job.start - JOB_LEAD_IN;
    }
    clock::secs((app.clock)()) - app.settings.values().retention
}

pub async fn frame(State(app): State<App>, query: Query) -> Response {
    let Some(ts) = query.get("ts").and_then(|t| t.parse::<i64>().ok()) else {
        return text(StatusCode::BAD_REQUEST, "invalid ts");
    };
    match app.store.frame_at_or_after(ts) {
        Ok(Some((jpeg, taken))) => {
            let mut resp = jpeg.into_response();
            let headers = resp.headers_mut();
            headers.insert(
                header::CONTENT_TYPE,
                header::HeaderValue::from_static("image/jpeg"),
            );
            headers.insert(FRAME_TIMESTAMP, taken.into());
            resp
        }
        Ok(None) => text(StatusCode::NOT_FOUND, "no frame at or after ts"),
        Err(_) => text(StatusCode::INTERNAL_SERVER_ERROR, "frame query failed"),
    }
}

pub async fn jobs(State(app): State<App>) -> Response {
    match app.store.recent_jobs() {
        Ok(jobs) => json_reply(&jobs),
        Err(_) => text(StatusCode::INTERNAL_SERVER_ERROR, "jobs query failed"),
    }
}
