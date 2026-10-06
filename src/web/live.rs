//! GET /api/live: one websocket per open page, carrying everything the page
//! used to poll for.
//!
//! Server to page, as JSON text, each message a full replacement:
//! - `{"type":"status", ...}` — the `/api/status` shape, on connect, on every
//!   change to the printer's state (at most four a second), and every second
//!   so countdowns stay current;
//! - `{"type":"range","oldest","newest"}` — the scrub bar's span, on connect,
//!   per recorded frame and every second;
//! - `{"type":"jobs","jobs":[...]}` — recent prints, on connect and whenever
//!   one starts, ends or is pruned.
//!
//! Server to page, binary: while the page has said `{"type":"follow","on":true}`,
//! each recorded frame as an 8-byte big-endian unix second then the JPEG.

use std::sync::Arc;
use std::time::Duration;

use axum::extract::State;
use axum::extract::ws::{Message, WebSocket, WebSocketUpgrade};
use axum::http::{HeaderMap, StatusCode, header};
use axum::response::Response;
use serde_json::{Value, json};
use tokio::sync::{broadcast, watch};
use tokio::time::Instant;

use super::{App, text};

/// The fewest milliseconds between two status messages.
const STATUS_EVERY: Duration = Duration::from_millis(250);
const PING_EVERY: Duration = Duration::from_secs(30);

/// The latest of each message, shared by every open page.
#[derive(Clone)]
pub struct Live {
    status: watch::Sender<Arc<str>>,
    range: watch::Sender<Arc<str>>,
    jobs: watch::Sender<Arc<str>>,
}

impl Default for Live {
    fn default() -> Self {
        let empty = || watch::Sender::new(Arc::from(""));
        Live {
            status: empty(),
            range: empty(),
            jobs: empty(),
        }
    }
}

fn publish(tx: &watch::Sender<Arc<str>>, msg: Value) {
    let msg: Arc<str> = msg.to_string().into();
    tx.send_if_modified(|current| {
        let changed = *current != msg;
        if changed {
            *current = msg;
        }
        changed
    });
}

fn tagged(kind: &str, mut v: Value) -> Value {
    if let Value::Object(map) = &mut v {
        map.insert("type".into(), kind.into());
    }
    v
}

impl Live {
    fn status(&self, app: &App) {
        publish(&self.status, tagged("status", super::status_of(app)));
    }

    fn range(&self, app: &App) {
        match super::camera::range_of(app) {
            Ok(range) => publish(&self.range, tagged("range", range)),
            Err(err) => tracing::warn!("live: camera range: {err}"),
        }
    }

    fn jobs(&self, app: &App) {
        match app.store.recent_jobs() {
            Ok(jobs) => publish(&self.jobs, json!({"type": "jobs", "jobs": jobs})),
            Err(err) => tracing::warn!("live: jobs: {err}"),
        }
    }

    /// Keeps the messages current, forever. Spawn once.
    pub async fn run(self, app: App) {
        let mut state = app.cache.subscribe();
        let mut frames = app.hub.frames();
        let mut tick = tokio::time::interval(Duration::from_secs(1));
        let mut next_status = Instant::now();
        self.jobs(&app);
        loop {
            let throttled = Instant::now() < next_status;
            tokio::select! {
                changed = state.changed(), if !throttled => {
                    if changed.is_err() {
                        return;
                    }
                    self.status(&app);
                    next_status = Instant::now() + STATUS_EVERY;
                }
                _ = tokio::time::sleep_until(next_status), if throttled => {}
                _ = tick.tick() => {
                    self.status(&app);
                    self.range(&app);
                }
                frame = frames.recv() => match frame {
                    Ok(_) | Err(broadcast::error::RecvError::Lagged(_)) => self.range(&app),
                    Err(broadcast::error::RecvError::Closed) => return,
                },
                _ = app.jobs_changed.notified() => {
                    self.jobs(&app);
                    self.range(&app);
                }
            }
        }
    }
}

pub async fn socket(State(app): State<App>, headers: HeaderMap, ws: WebSocketUpgrade) -> Response {
    // A websocket is opened with GET, which the cross-origin check lets
    // through, and browsers send the session cookie with it from any page. So
    // the origin is checked here: only this app's own page may connect.
    if !same_origin(&app, &headers) {
        return text(StatusCode::FORBIDDEN, "cross-origin websocket refused");
    }
    ws.on_upgrade(move |socket| client(app, socket))
}

fn same_origin(app: &App, headers: &HeaderMap) -> bool {
    let get = |name| {
        headers
            .get(name)
            .and_then(|v: &header::HeaderValue| v.to_str().ok())
    };
    let (Some(origin), Some(host)) = (get(header::ORIGIN), get(header::HOST)) else {
        return false;
    };
    if app.cross_origin.trusted.iter().any(|t| t == origin) {
        return true;
    }
    url::Url::parse(origin).is_ok_and(|u| {
        let origin_host = match u.port() {
            Some(port) => format!("{}:{port}", u.host_str().unwrap_or("")),
            None => u.host_str().unwrap_or("").to_string(),
        };
        origin_host.eq_ignore_ascii_case(host)
    })
}

async fn client(app: App, mut socket: WebSocket) {
    let live = &app.live;
    let (mut status, mut range, mut jobs) = (
        live.status.subscribe(),
        live.range.subscribe(),
        live.jobs.subscribe(),
    );
    // Fill in anything not computed yet, so the first messages are complete.
    live.status(&app);
    live.range(&app);
    if jobs.borrow().is_empty() {
        live.jobs(&app);
    }
    let mut frames = app.hub.frames();
    let mut following = false;
    let mut ping = tokio::time::interval(PING_EVERY);
    for rx in [&mut status, &mut range, &mut jobs] {
        let msg = rx.borrow_and_update().clone();
        if !msg.is_empty()
            && socket
                .send(Message::Text(msg.as_ref().into()))
                .await
                .is_err()
        {
            return;
        }
    }
    loop {
        let out = tokio::select! {
            Ok(()) = status.changed() => Message::Text(status.borrow_and_update().as_ref().into()),
            Ok(()) = range.changed() => Message::Text(range.borrow_and_update().as_ref().into()),
            Ok(()) = jobs.changed() => Message::Text(jobs.borrow_and_update().as_ref().into()),
            frame = frames.recv(), if following => match frame {
                Ok(frame) => binary_frame(frame.0, &frame.1),
                // A slow page skips to the newest frame rather than queueing.
                Err(broadcast::error::RecvError::Lagged(_)) => continue,
                Err(broadcast::error::RecvError::Closed) => return,
            },
            incoming = socket.recv() => match incoming {
                Some(Ok(Message::Text(msg))) => {
                    let Some(on) = follow_request(&msg) else { continue };
                    following = on;
                    if !on {
                        continue;
                    }
                    // Show the newest frame at once rather than at the next one.
                    match newest_frame(&app) {
                        Some(msg) => msg,
                        None => continue,
                    }
                }
                Some(Ok(Message::Close(_))) | Some(Err(_)) | None => return,
                Some(Ok(_)) => continue,
            },
            _ = ping.tick() => Message::Ping(Vec::new().into()),
        };
        if socket.send(out).await.is_err() {
            return;
        }
    }
}

/// `{"type":"follow","on":true}` → Some(true).
fn follow_request(msg: &str) -> Option<bool> {
    let v: Value = serde_json::from_str(msg).ok()?;
    if v.get("type")?.as_str()? != "follow" {
        return None;
    }
    v.get("on")?.as_bool()
}

fn binary_frame(ts: i64, jpeg: &[u8]) -> Message {
    let mut out = Vec::with_capacity(8 + jpeg.len());
    out.extend_from_slice(&ts.to_be_bytes());
    out.extend_from_slice(jpeg);
    Message::Binary(out.into())
}

fn newest_frame(app: &App) -> Option<Message> {
    let (_, newest) = app.store.range().ok()?;
    let (jpeg, ts) = app.store.frame_at_or_after(newest?).ok()??;
    Some(binary_frame(ts, &jpeg))
}
