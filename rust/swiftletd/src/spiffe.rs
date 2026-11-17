//! SPIFFE for the built-in egress broker: the sandbox's X.509-SVID from the
//! SPIFFE Workload API, and a rustls client config that presents it.
//!
//! The Workload API is gRPC over a unix socket
//! (`SPIFFE_ENDPOINT_SOCKET=unix:///run/spiffe/<agent socket>`, mounted by
//! the controller). `FetchX509SVID` is a server stream: the agent sends the
//! current SVID and bundle, then a new message on every rotation. [`watch`]
//! keeps the latest in a `tokio::sync::watch` channel and reconnects with a
//! backoff. Only the four fields used are decoded (spiffe_id, x509_svid,
//! x509_svid_key, bundle), by hand: tonic and prost would add dozens of
//! crates for one request.
//!
//! TLS to the upstream follows SPIFFE X.509-SVID verification: the upstream's
//! chain must lead to the trust domain's bundle and its leaf must carry one
//! URI SAN `spiffe://<trust domain>/...`. Host names are not checked (an SVID
//! carries none). Any workload of the trust domain is accepted: the allow
//! rule already names the destination.
use std::sync::Arc;
use std::time::Duration;

use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::crypto::CryptoProvider;
use rustls::{DigitallySignedStruct, SignatureScheme};
use rustls_pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer, ServerName, UnixTime};

/// Environment variable naming the Workload API socket.
pub const SOCKET_ENV: &str = "SPIFFE_ENDPOINT_SOCKET";

/// The sandbox's current X.509-SVID.
#[derive(Clone)]
pub struct Svid {
    pub id: String,
    /// Client config presenting this SVID and verifying upstreams against
    /// the trust domain's bundle.
    pub tls: Arc<rustls::ClientConfig>,
}

impl std::fmt::Debug for Svid {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Svid")
            .field("id", &self.id)
            .finish_non_exhaustive()
    }
}

/// The socket path from `SPIFFE_ENDPOINT_SOCKET`, or None when unset.
pub fn socket_from_env() -> Option<String> {
    let v = std::env::var(SOCKET_ENV).ok()?;
    let p = v.strip_prefix("unix://").unwrap_or(&v);
    (!p.is_empty()).then(|| p.to_string())
}

/// The trust domain of a SPIFFE ID.
pub fn trust_domain(id: &str) -> Option<&str> {
    let rest = id.strip_prefix("spiffe://")?;
    let td = rest.split('/').next()?;
    (!td.is_empty()).then_some(td)
}

/// Keeps the latest SVID in the returned channel, forever.
pub fn watch(socket: String) -> tokio::sync::watch::Receiver<Option<Arc<Svid>>> {
    let (tx, rx) = tokio::sync::watch::channel(None);
    tokio::spawn(async move {
        let mut backoff = Duration::from_millis(200);
        loop {
            match stream(&socket, &tx).await {
                Ok(()) => log::warn!("spiffe_watch_closed socket={}", socket),
                Err(e) => log::warn!("spiffe_watch_error socket={} error={}", socket, e),
            }
            tokio::time::sleep(backoff).await;
            backoff = (backoff * 2).min(Duration::from_secs(5));
        }
    });
    rx
}

/// Waits up to `timeout` for an SVID.
pub async fn current(
    rx: &mut tokio::sync::watch::Receiver<Option<Arc<Svid>>>,
    timeout: Duration,
) -> Option<Arc<Svid>> {
    let got = tokio::time::timeout(timeout, rx.wait_for(|s| s.is_some())).await;
    match got {
        Ok(Ok(s)) => s.clone(),
        _ => None,
    }
}

async fn stream(
    socket: &str,
    tx: &tokio::sync::watch::Sender<Option<Arc<Svid>>>,
) -> Result<(), String> {
    let io = tokio::net::UnixStream::connect(socket)
        .await
        .map_err(|e| format!("connect: {}", e))?;
    let (mut client, conn) = h2::client::handshake(io)
        .await
        .map_err(|e| format!("h2: {}", e))?;
    tokio::spawn(async move {
        let _ = conn.await;
    });
    let req = http::Request::builder()
        .method("POST")
        .uri("http://localhost/SpiffeWorkloadAPI/FetchX509SVID")
        .header("content-type", "application/grpc")
        .header("te", "trailers")
        .header("workload.spiffe.io", "true")
        .body(())
        .map_err(|e| e.to_string())?;
    let (resp, mut send) = client
        .send_request(req, false)
        .map_err(|e| format!("request: {}", e))?;
    // An empty X509SVIDRequest, gRPC-framed: uncompressed, length 0.
    send.send_data(bytes::Bytes::from_static(&[0, 0, 0, 0, 0]), true)
        .map_err(|e| format!("send: {}", e))?;
    let resp = resp.await.map_err(|e| format!("response: {}", e))?;
    if resp.status() != http::StatusCode::OK {
        return Err(format!("status {}", resp.status()));
    }
    if let Some(s) = resp.headers().get("grpc-status") {
        if s != "0" {
            return Err(format!(
                "grpc-status {} {}",
                s.to_str().unwrap_or("?"),
                grpc_message(resp.headers())
            ));
        }
    }
    let mut body = resp.into_body();
    let mut buf: Vec<u8> = Vec::new();
    while let Some(chunk) = body.data().await {
        let chunk = chunk.map_err(|e| format!("read: {}", e))?;
        let _ = body.flow_control().release_capacity(chunk.len());
        buf.extend_from_slice(&chunk);
        while let Some(msg) = next_grpc_message(&mut buf)? {
            match svid_from_response(&msg) {
                Ok(svid) => {
                    log::info!("spiffe_svid id={}", svid.id);
                    let _ = tx.send(Some(Arc::new(svid)));
                }
                Err(e) => log::warn!("spiffe_svid_invalid error={}", e),
            }
        }
    }
    match body.trailers().await {
        Ok(Some(t)) => Err(format!(
            "stream ended: grpc-status {} {}",
            t.get("grpc-status")
                .and_then(|v| v.to_str().ok())
                .unwrap_or("?"),
            grpc_message(&t)
        )),
        _ => Ok(()),
    }
}

fn grpc_message(h: &http::HeaderMap) -> String {
    h.get("grpc-message")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string()
}

const MAX_MESSAGE: usize = 4 << 20;

/// Takes one complete gRPC message off the front of buf.
fn next_grpc_message(buf: &mut Vec<u8>) -> Result<Option<Vec<u8>>, String> {
    if buf.len() < 5 {
        return Ok(None);
    }
    if buf[0] != 0 {
        return Err("compressed gRPC message".into());
    }
    let len = u32::from_be_bytes([buf[1], buf[2], buf[3], buf[4]]) as usize;
    if len > MAX_MESSAGE {
        return Err(format!("gRPC message of {} bytes", len));
    }
    if buf.len() < 5 + len {
        return Ok(None);
    }
    let msg = buf[5..5 + len].to_vec();
    buf.drain(..5 + len);
    Ok(Some(msg))
}

/// One protobuf field: number, wire type, and the bytes of a
/// length-delimited value (empty otherwise).
type ProtoField<'a> = (u64, u8, &'a [u8]);

/// The fields of a protobuf message.
fn proto_fields(mut b: &[u8]) -> Result<Vec<ProtoField<'_>>, String> {
    fn varint(b: &mut &[u8]) -> Result<u64, String> {
        let mut v = 0u64;
        for i in 0..10 {
            let (&x, rest) = b.split_first().ok_or("truncated varint")?;
            *b = rest;
            v |= u64::from(x & 0x7f) << (7 * i);
            if x & 0x80 == 0 {
                return Ok(v);
            }
        }
        Err("varint too long".into())
    }
    let mut out = Vec::new();
    while !b.is_empty() {
        let key = varint(&mut b)?;
        let (field, wire) = (key >> 3, (key & 7) as u8);
        match wire {
            0 => {
                varint(&mut b)?;
                out.push((field, wire, &b[..0]));
            }
            1 | 5 => {
                let n = if wire == 1 { 8 } else { 4 };
                if b.len() < n {
                    return Err("truncated fixed field".into());
                }
                b = &b[n..];
                out.push((field, wire, &b[..0]));
            }
            2 => {
                let n = varint(&mut b)? as usize;
                if b.len() < n {
                    return Err("truncated field".into());
                }
                out.push((field, wire, &b[..n]));
                b = &b[n..];
            }
            w => return Err(format!("wire type {}", w)),
        }
    }
    Ok(out)
}

/// The first SVID of an X509SVIDResponse:
/// `repeated X509SVID svids = 1`, with `string spiffe_id = 1`,
/// `bytes x509_svid = 2` (DER chain), `bytes x509_svid_key = 3` (PKCS#8),
/// `bytes bundle = 4` (DER CA certificates).
fn svid_from_response(msg: &[u8]) -> Result<Svid, String> {
    let first = proto_fields(msg)?
        .into_iter()
        .find(|(f, w, _)| *f == 1 && *w == 2)
        .ok_or("response holds no SVID")?
        .2;
    let (mut id, mut chain, mut key, mut bundle) = (None, None, None, None);
    for (f, w, v) in proto_fields(first)? {
        if w != 2 {
            continue;
        }
        match f {
            1 => id = Some(String::from_utf8(v.to_vec()).map_err(|_| "spiffe_id is not UTF-8")?),
            2 => chain = Some(split_der(v)?),
            3 => key = Some(v.to_vec()),
            4 => bundle = Some(split_der(v)?),
            _ => {}
        }
    }
    let id = id.ok_or("SVID without spiffe_id")?;
    let chain = chain
        .filter(|c| !c.is_empty())
        .ok_or("SVID without certificates")?;
    let key = key.filter(|k| !k.is_empty()).ok_or("SVID without key")?;
    let bundle = bundle
        .filter(|b| !b.is_empty())
        .ok_or("SVID without bundle")?;
    let td = trust_domain(&id)
        .ok_or("SPIFFE ID without trust domain")?
        .to_string();
    let tls = client_config(&chain, &key, &bundle, &td)?;
    Ok(Svid { id, tls })
}

/// One DER element's total length (tag, length and contents) at the start of b.
fn der_len(b: &[u8]) -> Result<(usize, usize), String> {
    if b.len() < 2 {
        return Err("truncated DER".into());
    }
    let (hdr, len) = match b[1] {
        n if n < 0x80 => (2, n as usize),
        0x81..=0x84 => {
            let k = (b[1] & 0x7f) as usize;
            if b.len() < 2 + k {
                return Err("truncated DER length".into());
            }
            (
                2 + k,
                b[2..2 + k]
                    .iter()
                    .fold(0usize, |a, x| (a << 8) | *x as usize),
            )
        }
        _ => return Err("unsupported DER length".into()),
    };
    if b.len() < hdr + len {
        return Err("truncated DER element".into());
    }
    Ok((hdr, len))
}

/// Splits concatenated DER certificates.
fn split_der(mut b: &[u8]) -> Result<Vec<CertificateDer<'static>>, String> {
    let mut out = Vec::new();
    while !b.is_empty() {
        if b[0] != 0x30 {
            return Err("certificate is not a DER SEQUENCE".into());
        }
        let (h, l) = der_len(b)?;
        out.push(CertificateDer::from(b[..h + l].to_vec()));
        b = &b[h + l..];
    }
    Ok(out)
}

/// The URI subjectAltNames of a DER certificate.
pub fn uri_sans(cert: &[u8]) -> Vec<String> {
    // id-ce-subjectAltName 2.5.29.17, then an optional critical BOOLEAN,
    // then an OCTET STRING holding GeneralNames (a SEQUENCE); a URI is [6].
    const OID: &[u8] = &[0x06, 0x03, 0x55, 0x1d, 0x11];
    let mut out = Vec::new();
    let Some(at) = cert.windows(OID.len()).position(|w| w == OID) else {
        return out;
    };
    let mut b = &cert[at + OID.len()..];
    if b.first() == Some(&0x01) {
        let Ok((h, l)) = der_len(b) else { return out };
        b = &b[h + l..];
    }
    if b.first() != Some(&0x04) {
        return out;
    }
    let Ok((h, _)) = der_len(b) else { return out };
    b = &b[h..];
    if b.first() != Some(&0x30) {
        return out;
    }
    let Ok((h, l)) = der_len(b) else { return out };
    let mut names = &b[h..h + l];
    while !names.is_empty() {
        let Ok((h, l)) = der_len(names) else { break };
        if names[0] == 0x86 {
            if let Ok(s) = std::str::from_utf8(&names[h..h + l]) {
                out.push(s.to_string());
            }
        }
        names = &names[h + l..];
    }
    out
}

fn client_config(
    chain: &[CertificateDer<'static>],
    key: &[u8],
    bundle: &[CertificateDer<'static>],
    trust_domain: &str,
) -> Result<Arc<rustls::ClientConfig>, String> {
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let verifier = Arc::new(SpiffeVerifier::new(bundle, trust_domain, provider.clone())?);
    let cfg = rustls::ClientConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()
        .map_err(|e| e.to_string())?
        .dangerous()
        .with_custom_certificate_verifier(verifier)
        .with_client_auth_cert(
            chain.to_vec(),
            PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.to_vec())),
        )
        .map_err(|e| format!("client certificate: {}", e))?;
    Ok(Arc::new(cfg))
}

/// SPIFFE X.509-SVID verification of the upstream.
#[derive(Debug)]
struct SpiffeVerifier {
    roots: Vec<CertificateDer<'static>>,
    trust_domain: String,
    provider: Arc<CryptoProvider>,
}

impl SpiffeVerifier {
    fn new(
        bundle: &[CertificateDer<'static>],
        trust_domain: &str,
        provider: Arc<CryptoProvider>,
    ) -> Result<Self, String> {
        for c in bundle {
            webpki::anchor_from_trusted_cert(c).map_err(|e| format!("bundle: {}", e))?;
        }
        Ok(Self {
            roots: bundle.to_vec(),
            trust_domain: trust_domain.to_string(),
            provider,
        })
    }
}

impl ServerCertVerifier for SpiffeVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, rustls::Error> {
        let anchors: Vec<_> = self
            .roots
            .iter()
            .filter_map(|c| webpki::anchor_from_trusted_cert(c).ok())
            .collect();
        let ee = webpki::EndEntityCert::try_from(end_entity)
            .map_err(|e| rustls::Error::General(format!("upstream certificate: {}", e)))?;
        ee.verify_for_usage(
            self.provider.signature_verification_algorithms.all,
            &anchors,
            intermediates,
            now,
            webpki::KeyUsage::server_auth(),
            None,
            None,
        )
        .map_err(|e| rustls::Error::General(format!("upstream chain: {}", e)))?;
        let uris = uri_sans(end_entity);
        match uris.as_slice() {
            [one] if trust_domain(one) == Some(self.trust_domain.as_str()) => {
                Ok(ServerCertVerified::assertion())
            }
            _ => Err(rustls::Error::General(format!(
                "upstream is not an SVID of {} (URI SANs {:?})",
                self.trust_domain, uris
            ))),
        }
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls12_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.provider
            .signature_verification_algorithms
            .supported_schemes()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ca(td: &str) -> (rcgen::Certificate, rcgen::Issuer<'static, rcgen::KeyPair>) {
        let key = rcgen::KeyPair::generate().unwrap();
        let mut p = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        p.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        p.subject_alt_names = vec![rcgen::SanType::URI(
            format!("spiffe://{}", td).try_into().unwrap(),
        )];
        let cert = p.self_signed(&key).unwrap();
        (cert, rcgen::Issuer::new(p, key))
    }

    fn leaf(
        id: &str,
        issuer: &rcgen::Issuer<'_, rcgen::KeyPair>,
    ) -> (CertificateDer<'static>, Vec<u8>) {
        let key = rcgen::KeyPair::generate().unwrap();
        let mut p = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        p.subject_alt_names = vec![rcgen::SanType::URI(id.to_string().try_into().unwrap())];
        p.extended_key_usages = vec![
            rcgen::ExtendedKeyUsagePurpose::ServerAuth,
            rcgen::ExtendedKeyUsagePurpose::ClientAuth,
        ];
        let cert = p.signed_by(&key, issuer).unwrap();
        (cert.der().clone(), key.serialize_der())
    }

    fn pb(field: u64, v: &[u8]) -> Vec<u8> {
        let mut out = vec![((field << 3) | 2) as u8];
        let mut n = v.len();
        loop {
            let b = (n & 0x7f) as u8;
            n >>= 7;
            if n == 0 {
                out.push(b);
                break;
            }
            out.push(b | 0x80);
        }
        out.extend_from_slice(v);
        out
    }

    #[test]
    fn decodes_an_svid_response() {
        let (ca_cert, issuer) = ca("kubeswift.test");
        let id = "spiffe://kubeswift.test/ns/a/pool/p/slot/p-x";
        let (cert, key) = leaf(id, &issuer);
        let mut svid = pb(1, id.as_bytes());
        svid.extend(pb(2, &cert));
        svid.extend(pb(3, &key));
        let mut bundle = ca_cert.der().to_vec();
        bundle.extend_from_slice(ca_cert.der());
        svid.extend(pb(4, &bundle));
        svid.extend(pb(5, b"hint"));
        let resp = pb(1, &svid);
        let mut framed = vec![0, 0, 0, 0, resp.len() as u8];
        if resp.len() > 255 {
            framed = [0u8]
                .iter()
                .copied()
                .chain((resp.len() as u32).to_be_bytes())
                .collect();
        }
        framed.extend_from_slice(&resp);
        let mut buf = framed[..3].to_vec();
        assert!(next_grpc_message(&mut buf).unwrap().is_none());
        buf = framed.clone();
        let msg = next_grpc_message(&mut buf).unwrap().unwrap();
        assert!(buf.is_empty());
        let s = svid_from_response(&msg).unwrap();
        assert_eq!(s.id, id);
        assert_eq!(split_der(&bundle).unwrap().len(), 2);
        assert_eq!(uri_sans(&cert), vec![id.to_string()]);
        assert!(svid_from_response(&pb(1, &pb(1, id.as_bytes()))).is_err());
    }

    #[test]
    fn verifies_upstreams_of_the_trust_domain_only() {
        let (ca_cert, issuer) = ca("kubeswift.test");
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let v = SpiffeVerifier::new(&[ca_cert.der().clone()], "kubeswift.test", provider.clone())
            .unwrap();
        let name = ServerName::try_from("10.0.0.1").unwrap();
        let now = UnixTime::now();
        let (good, _) = leaf("spiffe://kubeswift.test/ns/a/target", &issuer);
        assert!(v.verify_server_cert(&good, &[], &name, &[], now).is_ok());
        let (foreign_id, _) = leaf("spiffe://other.test/x", &issuer);
        assert!(v
            .verify_server_cert(&foreign_id, &[], &name, &[], now)
            .is_err());
        let (_, other) = ca("kubeswift.test");
        let (untrusted, _) = leaf("spiffe://kubeswift.test/ns/a/target", &other);
        assert!(v
            .verify_server_cert(&untrusted, &[], &name, &[], now)
            .is_err());
    }

    /// The decoded SVID's client config completes mTLS with a server of
    /// the trust domain that requires a client certificate, and the server
    /// sees the sandbox's SPIFFE ID.
    #[tokio::test]
    async fn originates_mtls_with_the_svid() {
        let (ca_cert, issuer) = ca("kubeswift.test");
        let id = "spiffe://kubeswift.test/ns/a/sandbox/s";
        let (cert, key) = leaf(id, &issuer);
        let mut svid = pb(1, id.as_bytes());
        svid.extend(pb(2, &cert));
        svid.extend(pb(3, &key));
        svid.extend(pb(4, ca_cert.der()));
        let svid = svid_from_response(&pb(1, &svid)).unwrap();

        let (server_cert, server_key) = leaf("spiffe://kubeswift.test/ns/a/target", &issuer);
        let mut roots = rustls::RootCertStore::empty();
        roots.add(ca_cert.der().clone()).unwrap();
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let client_verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
            Arc::new(roots),
            provider.clone(),
        )
        .build()
        .unwrap();
        let server = rustls::ServerConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_client_cert_verifier(client_verifier)
            .with_single_cert(
                vec![server_cert],
                PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(server_key)),
            )
            .unwrap();
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let acceptor = tokio_rustls::TlsAcceptor::from(Arc::new(server));
        let srv = tokio::spawn(async move {
            let (s, _) = listener.accept().await.unwrap();
            let tls = acceptor.accept(s).await.unwrap();
            let peer = tls.get_ref().1.peer_certificates().unwrap()[0].clone();
            uri_sans(&peer)
        });
        let tcp = tokio::net::TcpStream::connect(addr).await.unwrap();
        let name = ServerName::IpAddress(addr.ip().into());
        let mut c = tokio_rustls::TlsConnector::from(svid.tls.clone())
            .connect(name, tcp)
            .await
            .unwrap();
        use tokio::io::AsyncWriteExt;
        c.write_all(b"x").await.unwrap();
        assert_eq!(srv.await.unwrap(), vec![id.to_string()]);
    }

    #[test]
    fn trust_domains_and_sockets() {
        assert_eq!(trust_domain("spiffe://td.example/ns/x"), Some("td.example"));
        assert_eq!(trust_domain("spiffe:///x"), None);
        assert_eq!(trust_domain("https://x"), None);
    }
}
