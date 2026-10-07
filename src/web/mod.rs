//! The phone-facing HTTP interface.

mod actions;
mod camera;
mod csrf;
mod live;
mod notify;
mod settings;
mod statics;

use std::collections::HashMap;
use std::sync::Arc;

use axum::Router;
use axum::http::{HeaderValue, StatusCode, header};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use serde::Serialize;
use serde_json::{Value, json};
use tokio::sync::Notify;

use crate::activity::Log;
use crate::camera::Hub;
use crate::clock::Clock;
use crate::history::Store;
use crate::p1s::{self, Link, StateCache};
use crate::push::Sender;
use crate::settings::Settings;
use crate::timers::{self, Timers};

pub use csrf::CrossOrigin;
pub use live::Live;

/// Everything the handlers reach.
#[derive(Clone)]
pub struct App {
    pub cache: StateCache,
    pub link: Link,
    pub store: Store,
    pub sender: Sender,
    pub settings: Settings,
    pub timers: Timers,
    pub activity: Log,
    pub hub: Hub,
    pub live: Live,
    /// Fires when a job row is opened, closed or pruned.
    pub jobs_changed: Arc<Notify>,
    pub cross_origin: CrossOrigin,
    pub clock: Clock,
}

pub fn router(app: App) -> Router {
    Router::new()
        .route("/api/status", get(status))
        .route("/api/actions/{name}", post(actions::action))
        .route("/api/live", get(live::socket))
        .route("/camera/history/range", get(camera::range))
        .route("/camera/history/frame", get(camera::frame))
        .route("/camera/history/jobs", get(camera::jobs))
        .route("/api/events", get(settings::events))
        .route(
            "/api/printer",
            get(settings::printer).post(settings::set_printer),
        )
        .route("/api/settings", get(settings::get))
        .route("/api/settings/{name}", post(settings::set))
        .route("/api/push/key", get(notify::key))
        .route("/api/push/subscribe", post(notify::subscribe))
        .route("/api/push/unsubscribe", post(notify::unsubscribe))
        .route("/api/push/test", post(notify::test))
        .route(
            "/api/push/preferences",
            get(notify::preferences).post(notify::set_preferences),
        )
        .route("/healthz", get(|| async { StatusCode::OK }))
        .fallback(statics::serve)
        .method_not_allowed_fallback(|| async {
            text(StatusCode::METHOD_NOT_ALLOWED, "Method Not Allowed")
        })
        .with_state(app)
}

type Query = axum::extract::Query<HashMap<String, String>>;

/// A plain-text reply, the way every error and action result is sent.
pub fn text(status: StatusCode, msg: impl Into<String>) -> Response {
    let mut body = msg.into();
    if !status.is_success() {
        body.push('\n');
    }
    let mut resp = (status, body).into_response();
    let headers = resp.headers_mut();
    headers.insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("text/plain; charset=utf-8"),
    );
    headers.insert(
        header::X_CONTENT_TYPE_OPTIONS,
        HeaderValue::from_static("nosniff"),
    );
    resp
}

/// A JSON reply.
pub fn json_reply(v: &impl Serialize) -> Response {
    let mut body = serde_json::to_vec(v).unwrap_or_default();
    body.push(b'\n');
    ([(header::CONTENT_TYPE, "application/json")], body).into_response()
}

/// Decodes a small JSON request body.
fn read_json<T: serde::de::DeserializeOwned>(body: &[u8]) -> Option<T> {
    if body.len() > 4096 {
        return None;
    }
    serde_json::from_slice(body).ok()
}

async fn status(axum::extract::State(app): axum::extract::State<App>) -> Response {
    json_reply(&status_of(&app))
}

/// What the page shows about the printer, from its merged state and the
/// app's own timers.
pub fn status_of(app: &App) -> Value {
    let snap = app.cache.snapshot();
    let (f, connected) = (&snap.fields, snap.connected);
    let state = p1s::gcode_state(f);
    let get = |name: &str| f.get(name).cloned().unwrap_or(Value::Null);
    let now = (app.clock)();
    let allowed = |action| p1s::print_action_allowed(connected, state, action).is_ok();
    json!({
        "connected": connected,
        "configured": app.link.config().complete(),
        "problem": snap.problem.as_deref(),
        "gcodeState": state,
        "actionsAllowed": p1s::action_allowed(connected, state).is_ok(),
        "bedTemp": get("bed_temper"),
        "bedTarget": get("bed_target_temper"),
        "nozzleTemp": get("nozzle_temper"),
        "nozzleTarget": get("nozzle_target_temper"),
        "progress": get("mc_percent"),
        "jobName": p1s::job_name(f),
        "layerNum": get("layer_num"),
        "totalLayerNum": get("total_layer_num"),
        "remainingMinutes": get("mc_remaining_time"),
        "chamberLight": p1s::chamber_light(f),
        "fans": {
            "cooling": get("cooling_fan_speed"),
            "aux": get("big_fan1_speed"),
            "chamber": get("big_fan2_speed"),
        },
        "ams": get("ams"),
        "hms": p1s::hms_errors(f),
        "bedOffIn": app.timers.remaining(timers::BED_OFF, now),
        "nozzleOffIn": app.timers.remaining(timers::NOZZLE_OFF, now),
        "lampOffIn": app.timers.remaining(timers::LAMP_OFF, now),
        "printActions": {"pause": allowed("pause"), "resume": allowed("resume"), "stop": allowed("stop")},
    })
}

#[cfg(test)]
pub mod tests;
