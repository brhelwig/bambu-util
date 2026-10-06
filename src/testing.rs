//! Stand-ins for the printer, for tests.

use std::sync::Arc;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;
use tokio::sync::Mutex;
use tokio_rustls::TlsAcceptor;

/// A TLS server configuration with a fresh self-signed certificate.
pub fn server_tls() -> TlsAcceptor {
    let cert = rcgen::generate_simple_self_signed(vec!["localhost".into()]).unwrap();
    let key = rustls::pki_types::PrivateKeyDer::try_from(cert.signing_key.serialize_der()).unwrap();
    let config = rustls::ServerConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .unwrap()
    .with_no_client_auth()
    .with_single_cert(vec![cert.cert.der().clone()], key)
    .unwrap();
    TlsAcceptor::from(Arc::new(config))
}

/// A camera that accepts one connection, reads the 80-byte auth packet, sends
/// `frames` and hangs up.
pub struct FakeCamera {
    pub port: u16,
    auth: Arc<Mutex<Vec<u8>>>,
}

impl FakeCamera {
    pub async fn start(frames: Vec<Vec<u8>>) -> FakeCamera {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        let auth = Arc::new(Mutex::new(Vec::new()));
        let tls = server_tls();
        let seen = auth.clone();
        tokio::spawn(async move {
            let (tcp, _) = listener.accept().await.unwrap();
            let mut conn = tls.accept(tcp).await.unwrap();
            let mut packet = vec![0u8; 80];
            conn.read_exact(&mut packet).await.unwrap();
            *seen.lock().await = packet;
            for jpeg in frames {
                let mut header = [0u8; 16];
                header[0..4].copy_from_slice(&(jpeg.len() as u32).to_le_bytes());
                conn.write_all(&header).await.unwrap();
                conn.write_all(&jpeg).await.unwrap();
            }
            conn.shutdown().await.ok();
        });
        FakeCamera { port, auth }
    }

    /// The auth packet the client sent.
    pub async fn auth(&self) -> Vec<u8> {
        self.auth.lock().await.clone()
    }
}

/// A fresh directory under the system's temporary directory.
pub fn tempdir() -> std::path::PathBuf {
    let dir = std::env::temp_dir().join(format!("bambu-util-test-{}", crate::random_token()));
    std::fs::create_dir_all(&dir).unwrap();
    dir
}
