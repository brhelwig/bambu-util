//! bambu-util serves a phone-friendly control page for a Bambu P1S on the
//! local network: bed actions and live status over the printer's MQTT
//! interface, and its chamber camera, recorded continuously into a rolling
//! history buffer.

mod activity;
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

/// 32 random bytes as unpadded base64url: long enough that guessing one is not
/// worth attempting.
pub fn random_token() -> String {
    use base64::Engine;
    let mut raw = [0u8; 32];
    getrandom::getrandom(&mut raw).expect("random bytes");
    base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(raw)
}

fn main() {}
