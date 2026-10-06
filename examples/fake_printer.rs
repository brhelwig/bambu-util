//! Runs a pretend P1S on 127.0.0.1:8883 (MQTT) and :6000 (camera), part-way
//! through a print, so the app can be run and looked at without a printer.
//!
//!     cargo run --example fake_printer
//!
//! Then point the app's Settings at 127.0.0.1, serial FAKE0001, access code
//! 12345678.

#[allow(dead_code)]
#[path = "../src/testing/printer.rs"]
mod printer;

use serde_json::json;

#[tokio::main]
async fn main() {
    let printer =
        printer::FakePrinter::start("127.0.0.1", 8883, 6000, "FAKE0001", "12345678").await;
    printer.set_state(json!({
        "gcode_state": "RUNNING",
        "subtask_name": "Benchy",
        "gcode_file": "benchy.gcode",
        "mc_percent": 42,
        "layer_num": 84,
        "total_layer_num": 200,
        "mc_remaining_time": 73,
        "bed_temper": 59.8,
        "bed_target_temper": 60,
        "nozzle_temper": 219.5,
        "nozzle_target_temper": 220,
        "cooling_fan_speed": "15",
        "big_fan1_speed": "0",
        "big_fan2_speed": "0",
        "lights_report": [{"node": "chamber_light", "mode": "on"}],
        "hms": [],
        "print_error": 0,
        "ams": {
            "tray_now": "1",
            "ams": [{
                "id": "0", "humidity": "4", "humidity_raw": "31",
                "tray": [
                    {"id": "0", "tray_type": "PLA", "tray_color": "FF6B35FF", "nozzle_temp_min": "190", "nozzle_temp_max": "230", "tray_info_idx": "GFA00"},
                    {"id": "1", "tray_type": "PETG", "tray_color": "2E86ABFF", "nozzle_temp_min": "220", "nozzle_temp_max": "260", "tray_info_idx": "GFG00"},
                    {"id": "2"},
                    {"id": "3", "tray_type": "PLA", "tray_color": "000000FF", "nozzle_temp_min": "190", "nozzle_temp_max": "230", "tray_info_idx": "GFA01"}
                ]
            }]
        }
    }));
    println!(
        "fake printer on 127.0.0.1: mqtt {} camera {}, serial FAKE0001, access code 12345678",
        printer.mqtt_port, printer.camera_port
    );
    loop {
        tokio::time::sleep(std::time::Duration::from_secs(2)).await;
        // A little life, so live updates have something to show.
        printer.report(
            json!({"bed_temper": 59.5 + rand_tenth(), "nozzle_temper": 219.5 + rand_tenth()}),
        );
    }
}

fn rand_tenth() -> f64 {
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .subsec_nanos();
    (nanos % 10) as f64 / 10.0
}
