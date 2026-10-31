//! Shared provider types and the adapter interface.
//!
//! Port of the engine's Go `provider` package. `session`, `tools` and the
//! gateway all speak these types.

use std::pin::Pin;
use std::sync::Arc;

use anyhow::{anyhow, Result};
use bytes::Bytes;
use futures_util::{Stream, StreamExt};
use serde_json::{Map, Value};
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio_util::io::StreamReader;

/// The user agent every outbound request must carry. Cloudflare answers a 403
/// HTML challenge for the default client user agent.
pub const USER_AGENT: &str = "aixodia-engine/1.0";

/// One picture carried by a message. `data` is the encoded file and `media_type`
/// its MIME type (`image/jpeg`); both wire formats need the type to decode the
/// bytes, and neither accepts a bare blob.
#[derive(Debug, Clone, Default)]
pub struct Image {
    pub media_type: String,
    pub data: Vec<u8>,
}

#[derive(Debug, Clone, Default)]
pub struct Message {
    pub role: String,
    /// The text of the message. When `images` is empty it is what the wire
    /// carries; otherwise the text becomes one part of a multi-part body.
    pub content: String,
    pub images: Vec<Image>,
    pub tool_calls: Vec<ToolCall>,
    pub tool_call_id: String,
    pub tool_name: String,
    /// What a reasoning model wrote before the answer. Providers that serve a
    /// reasoning model validate the next request against it: an assistant turn
    /// replayed without the field is refused with a missing-parameter 400.
    pub reasoning_content: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ToolCall {
    pub id: String,
    pub name: String,
    pub arguments: String,
}

#[derive(Debug, Clone, Default)]
pub struct ToolDef {
    pub name: String,
    pub description: String,
    pub parameters: Map<String, Value>,
}

/// Callback that receives only reasoning text actually exposed by upstream.
pub type ReasoningFn = Arc<dyn Fn(&str) -> Result<()> + Send + Sync>;

#[derive(Clone, Default)]
pub struct Request {
    pub model: String,
    pub messages: Vec<Message>,
    /// This engine's session. An adapter that keeps its own conversation — the
    /// OpenCode client does — names it so the two stay in step without a
    /// mapping table.
    pub session_id: String,
    pub max_tokens: i64,
    pub temperature: Option<f64>,
    pub top_p: Option<f64>,
    pub stream: bool,
    pub tools: Vec<ToolDef>,
    pub user: String,
    pub on_reasoning: Option<ReasoningFn>,
}

impl std::fmt::Debug for Request {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Request")
            .field("model", &self.model)
            .field("messages", &self.messages.len())
            .field("session_id", &self.session_id)
            .field("max_tokens", &self.max_tokens)
            .field("stream", &self.stream)
            .field("tools", &self.tools.len())
            .finish()
    }
}

#[derive(Debug, Clone, Default)]
pub struct Response {
    pub id: String,
    pub model: String,
    pub provider: String,
    pub content: String,
    pub tool_calls: Vec<ToolCall>,
    pub usage: Usage,
    pub finish_reason: String,
    /// The reasoning the model wrote before `content`. It belongs in the
    /// assistant message the turn leaves behind.
    pub reasoning_content: String,
    /// The tool steps taken while producing `content`, in order: an assistant
    /// message carrying tool calls, then one tool message per call.
    pub trail: Vec<Message>,
    /// Time spent inside the provider calls, summed over every round.
    pub generation_ms: i64,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Usage {
    pub prompt_tokens: i64,
    pub completion_tokens: i64,
    pub total_tokens: i64,
    pub cache_read_tokens: i64,
    pub cache_write_tokens: i64,
    pub reasoning_tokens: i64,
    /// Whether `prompt_tokens` already contains the cache read. Providers
    /// disagree, and guessing wrong doubles or halves the prompt in the footer,
    /// so it is recorded rather than assumed.
    pub input_includes_cache: bool,
}

#[derive(Debug, Clone, Default)]
pub struct Provider {
    pub name: String,
    pub endpoint: String,
    pub keys: Vec<String>,
    pub adapter: String,
    pub model: String,
}

/// An upstream HTTP failure. Constructing it through [`APIError`] lets
/// [`retryable`] find it again with a downcast.
#[derive(Debug, Clone)]
pub struct APIError {
    pub status_code: u16,
    pub message: String,
}

impl std::fmt::Display for APIError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        if self.message.is_empty() {
            write!(f, "upstream HTTP {}", self.status_code)
        } else {
            write!(f, "upstream HTTP {}: {}", self.status_code, self.message)
        }
    }
}

impl std::error::Error for APIError {}

impl APIError {
    pub fn new(status_code: u16, message: impl Into<String>) -> Self {
        Self {
            status_code,
            message: message.into(),
        }
    }

    /// Wraps into an `anyhow::Error` whose downcast recovers the `APIError`.
    pub fn into_anyhow(self) -> anyhow::Error {
        anyhow::Error::new(self)
    }
}

/// Whether a 429 is a hard quota rather than a momentary throttle. A free tier
/// that has run out says so ("Free usage limit reached", "subscribe", "quota");
/// waiting seconds will not refill it, so the paced keyless retry only makes
/// the phone stare at "Sending request to provider…" for half a minute before
/// the same answer. A plain "rate limit" / "too many requests" is the opposite:
/// a per-IP window that a short retry rides out.
fn quota_is_terminal(message: &str) -> bool {
    let m = message.to_lowercase();
    m.contains("free usage limit")
        || m.contains("limit reached")
        || m.contains("subscribe")
        || m.contains("upgrade")
        || m.contains("quota")
        || m.contains("insufficient")
        || m.contains("out of funds")
        || m.contains("billing")
}

/// Whether the gateway should try again: a transport failure, or one of the
/// statuses providers use for transient conditions.
pub fn retryable(err: &anyhow::Error) -> bool {
    match err.downcast_ref::<APIError>() {
        Some(api) => {
            if api.status_code == 429 && quota_is_terminal(&api.message) {
                return false;
            }
            api.status_code == 401
                || api.status_code == 402
                || api.status_code == 408
                || api.status_code == 429
                || api.status_code >= 500
        }
        None => true,
    }
}

/// Resolves the endpoint for one operation, appending `suffix` when the caller
/// gave a base URL.
///
/// Port of Go `provider.Endpoint`: a missing scheme becomes `https://`, a path
/// that already ends in `suffix` is left alone, an empty path becomes
/// `default_path`, and anything else gets `suffix` appended.
pub fn endpoint(endpoint: &str, default_path: &str, suffix: &str) -> Result<String> {
    let endpoint = endpoint.trim();
    if endpoint.is_empty() {
        return Err(anyhow!("provider endpoint is empty"));
    }
    let with_scheme = if endpoint.contains("://") {
        endpoint.to_string()
    } else {
        format!("https://{endpoint}")
    };

    let mut parsed =
        url::Url::parse(&with_scheme).map_err(|e| anyhow!("provider endpoint: {e}"))?;

    let path = parsed.path().trim_end_matches('/').to_string();
    if path.ends_with(suffix) {
        // Already the operation path.
    } else if path.is_empty() {
        parsed.set_path(default_path);
    } else {
        parsed.set_path(&format!("{path}{suffix}"));
    }
    Ok(parsed.to_string())
}

/// Turns an upstream error body into the message a client should see.
pub fn error_message(body: &[u8]) -> String {
    let text = String::from_utf8_lossy(body);
    let text = text.trim();
    if text.is_empty() {
        return String::new();
    }

    #[derive(serde::Deserialize)]
    struct Payload {
        #[serde(default)]
        error: Nested,
        #[serde(default)]
        errors: Vec<Nested>,
    }
    #[derive(serde::Deserialize, Default)]
    struct Nested {
        #[serde(default)]
        message: String,
    }

    if let Ok(payload) = serde_json::from_str::<Payload>(text) {
        if !payload.error.message.is_empty() {
            return payload.error.message;
        }
        if let Some(first) = payload.errors.first() {
            if !first.message.is_empty() {
                return first.message.clone();
            }
        }
    }

    #[derive(serde::Deserialize)]
    struct Flat {
        #[serde(default)]
        message: String,
    }
    if let Ok(flat) = serde_json::from_str::<Flat>(text) {
        if !flat.message.is_empty() {
            return flat.message;
        }
    }

    let text = if text.chars().count() > 512 {
        text.chars().take(512).collect::<String>()
    } else {
        text.to_string()
    };
    text
}

/// Reads a Server-Sent Events body, calling `on_data` once per event with the
/// joined `data:` payload.
///
/// Port of Go `provider.ReadSSE`: `data:` lines accumulate (joined with `\n`),
/// a blank line dispatches, and whatever is left is dispatched at end of input.
pub async fn read_sse<S, E>(stream: S, mut on_data: impl FnMut(&str) -> Result<()>) -> Result<()>
where
    S: Stream<Item = std::result::Result<Bytes, E>>,
    E: std::error::Error + Send + Sync + 'static,
{
    let stream = Box::pin(stream.map(|item| item.map_err(|e| std::io::Error::other(e))));
    let reader = StreamReader::new(stream);
    let mut lines = BufReader::new(reader).lines();

    let mut data = String::new();
    while let Some(line) = lines.next_line().await? {
        if let Some(rest) = line.strip_prefix("data:") {
            if !data.is_empty() {
                data.push('\n');
            }
            data.push_str(rest.strip_prefix(' ').unwrap_or(rest));
        } else if line.is_empty() && !data.is_empty() {
            on_data(&data)?;
            data.clear();
        }
    }
    if !data.is_empty() {
        on_data(&data)?;
    }
    Ok(())
}

/// Helper for adapters that need the same stream type name.
pub type ByteStream =
    Pin<Box<dyn Stream<Item = std::result::Result<Bytes, reqwest::Error>> + Send>>;

/// Receives streamed answer text.
///
/// A concrete trait rather than a boxed `FnMut`, because `dyn FnMut(&str)`
/// passed through several async layers needs a higher-ranked bound that the
/// compilers cannot infer from a `&mut dyn` parameter.
pub trait Emitter: Send {
    fn emit(&mut self, delta: &str) -> Result<()>;
}

impl<F> Emitter for F
where
    F: FnMut(&str) -> Result<()> + Send,
{
    fn emit(&mut self, delta: &str) -> Result<()> {
        self(delta)
    }
}

/// The contract every adapter implements.
#[async_trait::async_trait]
pub trait Adapter: Send + Sync {
    fn name(&self) -> &str;

    async fn complete(&self, req: &Request, prov: &Provider, key: &str) -> Result<Response>;

    async fn stream(
        &self,
        req: &Request,
        prov: &Provider,
        key: &str,
        emit: &mut dyn Emitter,
    ) -> Result<Response>;
}

#[cfg(test)]
mod tests {
    use super::*;

    fn api(status: u16, message: &str) -> anyhow::Error {
        APIError::new(status, message).into_anyhow()
    }

    #[test]
    fn a_momentary_throttle_is_worth_retrying() {
        assert!(retryable(&api(429, "Too many requests, slow down")));
        assert!(retryable(&api(
            429,
            "rate limit exceeded, retry in 30 seconds"
        )));
        assert!(retryable(&api(503, "temporarily unavailable")));
    }

    #[test]
    fn a_spent_free_quota_fails_fast_instead_of_stalling() {
        // The Opencode free-tier answer: waiting seconds will not refill it, so
        // the paced retry only keeps the phone staring at a spinner.
        let spent = api(
            429,
            "Free usage limit reached. Free models work in OpenCode; subscribe to OpenCode Go to keep going.",
        );
        assert!(!retryable(&spent));
        assert!(!retryable(&api(429, "Monthly quota exceeded.")));
        assert!(!retryable(&api(
            429,
            "Insufficient credits, upgrade your plan."
        )));
    }
}
