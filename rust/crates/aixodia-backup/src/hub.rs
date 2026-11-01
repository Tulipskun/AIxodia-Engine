//! The bucket's HTTP surface: tree listing, path existence, batch add/delete
//! and `resolve` downloads.
//!
//! Uploads are not here. A bucket only accepts content that has gone through
//! Xet, so [`crate::xet`] pushes bytes to the CAS and this module registers the
//! resulting file hashes under a key.

use std::path::Path;

use http::header::{ACCEPT_ENCODING, AUTHORIZATION, CONTENT_TYPE, LINK, USER_AGENT};
use serde::Deserialize;

use crate::{encode_key, Config, Error, Result};

/// Operations per `/batch` request. The payload is one small line per key, so
/// the limit is about keeping a single failure from muddling a large run.
const BATCH_OPS: usize = 500;
/// Paths per `/paths-info` request, which is what the hub itself uses.
const PATHS_INFO_BATCH: usize = 1000;

/// One file as the bucket reports it.
#[derive(Clone, Debug)]
pub struct FileItem {
    pub path: String,
    pub size: u64,
    /// The Xet Merkle hash of the content, needed to copy an object to another
    /// key without moving its bytes.
    pub xet_hash: Option<String>,
}

#[derive(Deserialize)]
struct TreeItem {
    #[serde(rename = "type")]
    kind: String,
    path: String,
    #[serde(default)]
    size: Option<u64>,
    #[serde(default, rename = "xetHash")]
    xet_hash: Option<String>,
}

#[derive(Deserialize)]
struct BatchReply {
    #[serde(default)]
    failed: Vec<serde_json::Value>,
}

/// The bucket API, bound to one bucket and one token.
pub struct Hub {
    bucket: String,
    /// The key prefix this backup owns, so callers of a `Hub` need only a
    /// manifest name to name an object.
    prefix: String,
    /// `{endpoint}/api/buckets/{bucket}`.
    api: String,
    /// `{endpoint}/buckets/{bucket}/resolve`.
    resolve: String,
    token: String,
    client: reqwest::blocking::Client,
}

impl Hub {
    /// A client for `cfg`'s bucket. Must be built off any tokio runtime, like
    /// everything else in this crate.
    pub fn new(cfg: &Config) -> Result<Self> {
        cfg.validate()?;
        let client = reqwest::blocking::Client::builder()
            .timeout(std::time::Duration::from_secs(cfg.timeout_secs.max(10)))
            .build()?;
        Ok(Self {
            bucket: cfg.bucket.clone(),
            prefix: cfg.prefix.clone(),
            api: format!(
                "{}/api/buckets/{}",
                trim_endpoint(&cfg.endpoint),
                cfg.bucket
            ),
            resolve: format!(
                "{}/buckets/{}/resolve",
                trim_endpoint(&cfg.endpoint),
                cfg.bucket
            ),
            token: cfg.token.clone(),
            client,
        })
    }

    pub fn manifest_key(&self, name: &str) -> String {
        format!("{}/manifests/{}.json", self.prefix, name)
    }

    pub fn latest_key(&self) -> String {
        format!("{}/latest.json", self.prefix)
    }

    pub fn manifests_prefix(&self) -> String {
        format!("{}/manifests", self.prefix)
    }

    pub fn objects_prefix(&self) -> String {
        format!("{}/objects", self.prefix)
    }

    pub fn bucket(&self) -> &str {
        &self.bucket
    }

    fn get(&self, path: &str) -> reqwest::blocking::RequestBuilder {
        self.client
            .get(path)
            .header(AUTHORIZATION, format!("Bearer {}", self.token))
    }

    fn post(&self, path: &str) -> reqwest::blocking::RequestBuilder {
        self.client
            .post(path)
            .header(AUTHORIZATION, format!("Bearer {}", self.token))
    }

    /// `key` must already be percent-encoded for the URL, and `operation` is
    /// what a failure names itself as.
    fn check(status: reqwest::StatusCode, body: String, operation: &str, key: &str) -> Result<()> {
        if status.is_success() {
            return Ok(());
        }
        Err(Error::Hub {
            operation: operation.to_string(),
            key: key.to_string(),
            status: status.as_u16(),
            body: trim_body(&body),
        })
    }

    /// Every file under `prefix`, recursively. Pagination is the hub's `Link`
    /// header, the same as its other list endpoints.
    pub fn list_files(&self, prefix: &str) -> Result<Vec<FileItem>> {
        let mut out = Vec::new();
        let mut url = format!("{}/tree/{}?recursive=true", self.api, encode_key(prefix));
        loop {
            let response = self
                .get(&url)
                .header(USER_AGENT, "aixodia-engine-backup")
                .send()?;
            let status = response.status();
            let link = link_next(response.headers());
            let body = response.text()?;
            Self::check(status, body.clone(), "list", prefix)?;
            let page: Vec<TreeItem> = serde_json::from_str(&body)
                .map_err(|e| Error::Manifest(prefix.to_string(), e.to_string()))?;
            for item in page {
                if item.kind == "file" {
                    out.push(FileItem {
                        path: item.path,
                        size: item.size.unwrap_or(0),
                        xet_hash: item.xet_hash,
                    });
                }
            }
            match link {
                Some(next) => url = next,
                None => return Ok(out),
            }
        }
    }

    /// The keys among `keys` that the bucket actually holds. Missing keys are
    /// not an error, so this is how a caller asks "is this content already up?".
    pub fn existing_keys(&self, keys: &[String]) -> Result<Vec<String>> {
        let mut found = Vec::new();
        for chunk in keys.chunks(PATHS_INFO_BATCH) {
            let response = self
                .post(&format!("{}/paths-info", self.api))
                .json(&serde_json::json!({ "paths": chunk }))
                .send()?;
            let status = response.status();
            let body = response.text()?;
            Self::check(
                status,
                body.clone(),
                "paths-info",
                &format!("{} keys", chunk.len()),
            )?;
            let items: Vec<TreeItem> = serde_json::from_str(&body)
                .map_err(|e| Error::Manifest("paths-info".into(), e.to_string()))?;
            found.extend(
                items
                    .into_iter()
                    .filter(|i| i.kind == "file")
                    .map(|i| i.path),
            );
        }
        Ok(found)
    }

    /// `GET /buckets/{bucket}/resolve/{key}`. Returns `None` when the key is
    /// not there; the hub redirects to the CDN and the redirect is followed.
    pub fn get_bytes(&self, key: &str) -> Result<Option<Vec<u8>>> {
        let url = format!("{}/{}", self.resolve, encode_key(key));
        let response = self.get(&url).send()?;
        let status = response.status();
        if status == reqwest::StatusCode::NOT_FOUND {
            return Ok(None);
        }
        if !status.is_success() {
            let body = response.text().unwrap_or_default();
            return Err(Error::Hub {
                operation: "download".into(),
                key: key.to_string(),
                status: status.as_u16(),
                body: trim_body(&body),
            });
        }
        let body = response.bytes()?;
        Ok(Some(body.to_vec()))
    }

    /// Downloads `key` straight into `path`, writing a temporary sibling so a
    /// half-received file never poses as a restored one.
    pub fn download_to(&self, key: &str, path: &Path) -> Result<u64> {
        let url = format!("{}/{}", self.resolve, encode_key(key));
        let response = self.get(&url).header(ACCEPT_ENCODING, "identity").send()?;
        let status = response.status();
        if status == reqwest::StatusCode::NOT_FOUND {
            return Err(Error::Hub {
                operation: "download".into(),
                key: key.to_string(),
                status: 404,
                body: "the object is not in the bucket".into(),
            });
        }
        if !status.is_success() {
            let body = response.text().unwrap_or_default();
            return Err(Error::Hub {
                operation: "download".into(),
                key: key.to_string(),
                status: status.as_u16(),
                body: trim_body(&body),
            });
        }
        let tmp = temp_path(path);
        let mut file = std::fs::File::create(&tmp).map_err(|source| Error::Io {
            path: tmp.display().to_string(),
            source,
        })?;
        let mut stream = response;
        let written = std::io::copy(&mut stream, &mut file).map_err(|source| {
            let _ = std::fs::remove_file(&tmp);
            Error::Io {
                path: tmp.display().to_string(),
                source,
            }
        })?;
        drop(file);
        std::fs::rename(&tmp, path).map_err(|source| Error::Io {
            path: path.display().to_string(),
            source,
        })?;
        Ok(written)
    }

    /// Registers `items` as files: `(key, xet hash, modification time in
    /// milliseconds, content type)`. This is the step that makes uploaded
    /// content visible under a path.
    pub fn add_files(&self, items: &[(String, String, i64, Option<&str>)]) -> Result<()> {
        for chunk in items.chunks(BATCH_OPS) {
            let mut payload = String::new();
            for (key, hash, mtime_ms, content_type) in chunk {
                let mut line = serde_json::json!({
                    "type": "addFile",
                    "path": key,
                    "xetHash": hash,
                    "mtime": mtime_ms,
                });
                if let Some(ct) = content_type {
                    line["contentType"] = serde_json::Value::String(ct.to_string());
                }
                payload.push_str(&line.to_string());
                payload.push('\n');
            }
            self.batch("add", &payload, chunk.len())?;
        }
        Ok(())
    }

    /// Deletes keys. Buckets are not versioned, so this is the end of them.
    pub fn delete_keys(&self, keys: &[String]) -> Result<()> {
        for chunk in keys.chunks(BATCH_OPS) {
            let mut payload = String::new();
            for key in chunk {
                let line = serde_json::json!({"type": "deleteFile", "path": key});
                payload.push_str(&line.to_string());
                payload.push('\n');
            }
            self.batch("delete", &payload, chunk.len())?;
        }
        Ok(())
    }

    /// The URL the Xet client calls for a fresh write token.
    pub fn write_token_url(&self) -> String {
        format!("{}/xet-write-token", self.api)
    }

    fn batch(&self, operation: &str, payload: &str, sent: usize) -> Result<()> {
        let response = self
            .post(&format!("{}/batch", self.api))
            .header(CONTENT_TYPE, "application/x-ndjson")
            .body(payload.to_string())
            .send()?;
        let status = response.status();
        let body = response.text()?;
        // A bucket batch answers 200 with the operations it refused, so the
        // status alone never tells the whole story.
        if status.is_success() || status.as_u16() == 422 {
            if let Ok(reply) = serde_json::from_str::<BatchReply>(&body) {
                if !reply.failed.is_empty() {
                    return Err(Error::Api {
                        operation: format!("{operation} batch"),
                        message: format!(
                            "{} of {sent} operations failed: {}",
                            reply.failed.len(),
                            trim_body(&serde_json::to_string(&reply.failed).unwrap_or_default())
                        ),
                    });
                }
            }
        }
        Self::check(status, body, operation, &format!("{sent} keys"))
    }
}

fn trim_endpoint(endpoint: &str) -> String {
    endpoint.trim_end_matches('/').to_string()
}

fn trim_body(body: &str) -> String {
    const KEEP: usize = 400;
    let flat = body.replace('\n', " ");
    if flat.len() <= KEEP {
        flat
    } else {
        format!("{}…", &flat[..KEEP])
    }
}

/// GitHub-style `Link: <url>; rel="next"`.
fn link_next(headers: &http::HeaderMap) -> Option<String> {
    for value in headers.get_all(LINK) {
        let Ok(text) = value.to_str() else { continue };
        for part in text.split(',') {
            let mut bits = part.splitn(2, ';');
            let url = bits.next()?.trim();
            let rest = bits.next().unwrap_or("").trim();
            if rest.replace(' ', "").contains("rel=\"next\"") {
                return Some(url.trim_matches('<').trim_end_matches('>').to_string());
            }
        }
    }
    None
}

fn temp_path(path: &Path) -> std::path::PathBuf {
    let name = path
        .file_name()
        .map(|n| n.to_string_lossy().to_string())
        .unwrap_or_else(|| "object".to_string());
    path.parent()
        .unwrap_or_else(|| Path::new("."))
        .join(format!(".{name}.aixodia-part"))
}
