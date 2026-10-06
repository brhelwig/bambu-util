use std::sync::Arc;

use serde_json::Value;
use tokio::sync::watch;

use super::Fields;

/// What the printer has said, merged, and whether it is connected. Cheap to
/// clone: the fields are shared and never changed in place.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Snapshot {
    pub fields: Arc<Fields>,
    pub connected: bool,
}

/// Merges partial "print" reports into a full picture (after the initial
/// pushall dump the printer only sends changed fields), and tells anyone
/// watching each time it changes.
#[derive(Clone)]
pub struct StateCache {
    tx: watch::Sender<Snapshot>,
}

impl Default for StateCache {
    fn default() -> Self {
        StateCache {
            tx: watch::Sender::new(Snapshot::default()),
        }
    }
}

impl StateCache {
    pub fn new() -> StateCache {
        StateCache::default()
    }

    pub fn merge(&self, fields: &Fields) {
        self.tx
            .send_modify(|s| deep_merge(Arc::make_mut(&mut s.fields), fields));
    }

    /// Forgets everything the printer said. Used when the app is pointed at a
    /// different printer, so the previous one's temperatures and job are not
    /// still on screen a moment later.
    pub fn reset(&self) {
        self.tx.send_replace(Snapshot::default());
    }

    /// A disconnect keeps the fields, stale but visible.
    pub fn set_connected(&self, connected: bool) {
        self.tx
            .send_if_modified(|s| std::mem::replace(&mut s.connected, connected) != connected);
    }

    pub fn snapshot(&self) -> Snapshot {
        self.tx.borrow().clone()
    }

    /// A receiver that wakes on every change.
    pub fn subscribe(&self) -> watch::Receiver<Snapshot> {
        self.tx.subscribe()
    }
}

/// Merges `src` into `dst`. Nested objects merge key by key, so a partial
/// report doesn't wipe sibling fields an earlier fuller report set — an "ams"
/// delta that omits tray_now must not erase it. Arrays and scalars replace
/// wholesale: the printer resends whole arrays.
fn deep_merge(dst: &mut Fields, src: &Fields) {
    for (key, value) in src {
        if let (Value::Object(src_obj), Some(Value::Object(dst_obj))) = (value, dst.get_mut(key)) {
            deep_merge(dst_obj, src_obj);
            continue;
        }
        dst.insert(key.clone(), value.clone());
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn obj(v: Value) -> Fields {
        v.as_object().unwrap().clone()
    }

    #[test]
    fn accumulates_and_overwrites() {
        let c = StateCache::new();
        c.merge(&obj(json!({"a": 1, "b": 2})));
        c.merge(&obj(json!({"b": 3})));
        assert_eq!(*c.snapshot().fields, obj(json!({"a": 1, "b": 3})));
    }

    #[test]
    fn nested_siblings_are_preserved_and_arrays_replaced() {
        let c = StateCache::new();
        c.merge(&obj(json!({"ams": {"tray_now": "1", "ams": [1, 2]}})));
        c.merge(&obj(json!({"ams": {"ams": [3]}})));
        assert_eq!(
            *c.snapshot().fields,
            obj(json!({"ams": {"tray_now": "1", "ams": [3]}}))
        );
        c.merge(&obj(json!({"ams": 5})));
        assert_eq!(*c.snapshot().fields, obj(json!({"ams": 5})));
    }

    #[test]
    fn snapshots_are_independent_of_later_merges() {
        let c = StateCache::new();
        c.merge(&obj(json!({"x": {"y": 1}})));
        let before = c.snapshot();
        c.merge(&obj(json!({"x": {"y": 2}})));
        assert_eq!(before.fields["x"]["y"], 1);
        assert_eq!(c.snapshot().fields["x"]["y"], 2);
    }

    #[test]
    fn connection_flag_and_reset() {
        let c = StateCache::new();
        let mut rx = c.subscribe();
        c.set_connected(true);
        assert!(rx.has_changed().unwrap());
        rx.mark_unchanged();
        c.set_connected(true);
        assert!(
            !rx.has_changed().unwrap(),
            "an unchanged flag should not wake watchers"
        );
        c.merge(&obj(json!({"a": 1})));
        c.set_connected(false);
        assert_eq!(c.snapshot().fields["a"], 1, "a disconnect keeps the fields");
        c.reset();
        assert_eq!(c.snapshot(), Snapshot::default());
    }
}
