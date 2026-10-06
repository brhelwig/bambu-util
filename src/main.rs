//! bambu-util serves a phone-friendly control page for a Bambu P1S on the
//! local network: bed actions and live status over the printer's MQTT
//! interface, and its chamber camera, recorded continuously into a rolling
//! history buffer.

mod activity;
mod auth;
mod camera;
mod capacity;
mod clock;
mod core;
mod db;
mod history;
mod p1s;
mod push;
mod settings;
#[cfg(test)]
mod testing;
mod timers;
mod web;

use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use axum::Router;
use tokio::sync::Notify;

use crate::clock::Clock;
use crate::p1s::{Config, Link, Ports, StateCache};

/// 32 random bytes as unpadded base64url: long enough that guessing one is not
/// worth attempting.
pub fn random_token() -> String {
    use base64::Engine;
    let mut raw = [0u8; 32];
    getrandom::getrandom(&mut raw).expect("random bytes");
    base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(raw)
}

/// The database file inside DATA_DIR.
const DB_FILE: &str = "bambu-util.sqlite";
/// What the Go version kept its data in. It is not read: the Rust version
/// starts afresh.
const OLD_DB_FILE: &str = "bambu-util.db";

/// How often retention and the size cap run.
const HOUSEKEEPING_EVERY: Duration = Duration::from_secs(5 * 60);

/// The assembled program — everything below `main` — so it can be started
/// without a process around it.
pub struct Started {
    pub router: Router,
    pub link: Link,
    pub cache: StateCache,
}

/// Opens the database under `data_dir`, wires everything to it and starts the
/// background tasks.
pub async fn start(
    data_dir: &Path,
    decision: auth::Decision,
    ports: Ports,
    clock: Clock,
) -> Result<Started, String> {
    std::fs::create_dir_all(data_dir)
        .map_err(|e| format!("create data dir {}: {e}", data_dir.display()))?;
    if data_dir.join(OLD_DB_FILE).exists() {
        tracing::info!(
            "{} is from the Go version and is no longer used; it can be deleted",
            data_dir.join(OLD_DB_FILE).display()
        );
    }
    let db = db::Db::open(data_dir.join(DB_FILE)).map_err(|e| format!("open database: {e}"))?;
    let settings =
        settings::Settings::new(db.clone()).map_err(|e| format!("open settings: {e}"))?;
    // The log reads its budget on every entry, so a change on the settings
    // page takes hold without a restart.
    let limits = settings.clone();
    let events = activity::Log::new(
        db.clone(),
        move || limits.values().activity_limit,
        clock.clone(),
    )
    .map_err(|e| format!("open event log: {e}"))?;
    let cache = StateCache::new();
    let link = Link::new(cache.clone(), events.clone(), clock.clone(), ports);
    let store = history::Store::new(db.clone());
    let hub = camera::Hub::new(link.clone(), store.clone(), clock.clone());
    let sender = push::Sender::new(db.clone(), events.clone(), clock.clone())
        .map_err(|e| format!("load notification identity: {e}"))?;
    let timers =
        timers::Timers::new(db.clone()).map_err(|e| format!("open pending timers: {e}"))?;
    let logins = auth::Store::new(db.clone());

    let authenticator = match &decision {
        auth::Decision::Disabled => None,
        auth::Decision::Required(cfg) => {
            let s = settings.clone();
            Some(
                auth::Authenticator::new(
                    cfg,
                    logins.clone(),
                    move || s.values().session_length,
                    clock.clone(),
                )
                .await?,
            )
        }
    };

    // Whatever printer was set up last time. With none, the app still serves
    // the page — that is where one is entered.
    let v = settings.values();
    link.configure(Config {
        ip: v.printer_ip,
        serial: v.printer_serial,
        access_code: v.access_code,
    });

    let jobs_changed = Arc::new(Notify::new());
    let cross_origin = auth::cross_origin(authenticator.as_ref());
    let app = web::App {
        cache: cache.clone(),
        link: link.clone(),
        store: store.clone(),
        sender: sender.clone(),
        settings: settings.clone(),
        timers: timers.clone(),
        activity: events.clone(),
        hub: hub.clone(),
        live: web::Live::default(),
        jobs_changed: jobs_changed.clone(),
        cross_origin: cross_origin.clone(),
        clock: clock.clone(),
    };

    let s = settings.clone();
    let reactor = core::Reactor::new(
        timers.clone(),
        history::JobTracker::new(store.clone()),
        move || s.values(),
    );
    let runner = core::Runner {
        link: link.clone(),
        sender,
        jobs_changed: jobs_changed.clone(),
        clock: clock.clone(),
    };
    tokio::spawn(reactor.run(cache.subscribe(), runner));
    tokio::spawn(hub.run());
    tokio::spawn(app.live.clone().run(app.clone()));
    tokio::spawn(logins.run_sweeper(clock.clone()));
    tokio::spawn(housekeeping(
        db,
        store,
        events,
        settings,
        jobs_changed,
        clock,
    ));

    let mut router = web::router(app);
    if let Some(auth) = &authenticator {
        router = auth.wrap(router);
    }
    let router = router.layer(axum::middleware::from_fn_with_state(
        cross_origin,
        web::CrossOrigin::layer,
    ));
    Ok(Started {
        router,
        link,
        cache,
    })
}

/// Retention, then the size cap — which only bites when what retention keeps
/// is still too much — every few minutes, forever.
async fn housekeeping(
    db: db::Db,
    store: history::Store,
    events: activity::Log,
    settings: settings::Settings,
    jobs_changed: Arc<Notify>,
    clock: Clock,
) {
    let limits = settings.clone();
    let enforcer = Arc::new(capacity::Enforcer::new(
        db,
        move || limits.values().database_limit,
        vec![Box::new(store.clone()), Box::new(events)],
    ));
    let mut tick = tokio::time::interval(HOUSEKEEPING_EVERY);
    tick.tick().await;
    loop {
        tick.tick().await;
        let (store, settings, enforcer, clock) = (
            store.clone(),
            settings.clone(),
            enforcer.clone(),
            clock.clone(),
        );
        let done = tokio::task::spawn_blocking(move || {
            let v = settings.values();
            if let Err(err) = store.prune(clock::secs(clock()) - v.retention, v.kept_jobs) {
                tracing::warn!("history: prune: {err}");
            }
            if let Err(err) = enforcer.once() {
                tracing::warn!("capacity: {err}");
            }
        })
        .await;
        if let Err(err) = done {
            tracing::warn!("housekeeping: {err}");
        }
        jobs_changed.notify_one();
    }
}

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info,rumqttc=warn")),
        )
        // Colour only for a person at a terminal, not in a container's log.
        .with_ansi(std::io::IsTerminal::is_terminal(&std::io::stdout()))
        .init();
    let env = |name: &str| std::env::var(name).ok().filter(|v| !v.is_empty());
    let addr = env("LISTEN_ADDR").unwrap_or_else(|| ":8081".into());
    let data_dir = env("DATA_DIR").unwrap_or_else(|| "./data".into());

    // Refuse to start unless authentication was explicitly configured.
    let decision = auth::decide(env).unwrap_or_else(|err| fatal(&err));
    if decision == auth::Decision::Disabled {
        tracing::warn!(
            "{} is set, so anything that can reach this port can drive the printer and watch the camera",
            auth::ENV_DISABLED
        );
    }
    let started = start(
        Path::new(&data_dir),
        decision,
        Ports::default(),
        clock::system(),
    )
    .await
    .unwrap_or_else(|e| fatal(&e));

    // ":8081" means every interface, as it does to Go.
    let bind = if addr.starts_with(':') {
        format!("0.0.0.0{addr}")
    } else {
        addr.clone()
    };
    let listener = tokio::net::TcpListener::bind(&bind)
        .await
        .unwrap_or_else(|e| fatal(&format!("listen on {addr}: {e}")));
    tracing::info!(
        "bambu-util listening on {addr} ({})",
        started.link.describe()
    );
    // Open pages hold websockets, which would keep a graceful shutdown waiting
    // forever, so it gets a few seconds.
    let (stopping, stop) = tokio::sync::watch::channel(false);
    tokio::spawn(async move {
        shutdown_signal().await;
        let _ = stopping.send(true);
    });
    let mut deadline = stop.clone();
    let mut graceful = stop;
    let serve = axum::serve(listener, started.router)
        .with_graceful_shutdown(async move { drop(graceful.wait_for(|s| *s).await) });
    tokio::select! {
        result = serve => if let Err(err) = result { fatal(&err.to_string()) },
        _ = async {
            drop(deadline.wait_for(|s| *s).await);
            tokio::time::sleep(Duration::from_secs(3)).await;
        } => {}
    }
    started.link.stop();
}

async fn shutdown_signal() {
    #[cfg(unix)]
    {
        let mut term = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
            .expect("signal handler");
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {}
            _ = term.recv() => {}
        }
    }
    #[cfg(not(unix))]
    let _ = tokio::signal::ctrl_c().await;
}

fn fatal(msg: &str) -> ! {
    tracing::error!("{msg}");
    std::process::exit(1);
}
