//! OpenCode Zen adapter: the free-tier request contract and the fallback ladder.
//!
//! Port of the engine's Go `provider/opencode` package.
//!
//! The contract, measured against the live endpoint:
//!
//! 1. `User-Agent: opencode/latest/2.0.27/cli` (or whatever `agent` is set to).
//! 2. `x-opencode-session: ses_<12 hex><14 base62>`, replaced hourly. The gate
//!    reads the low twelve bits of the inverted six-byte head (the counter) and
//!    requires it below 64, and the tail must be fourteen letters/digits.
//! 3. No `x-opencode-request` header of our own: the gate refuses a request
//!    that names its own request id.
//! 4. The body must declare `shell` and `read` tools, and `stream` must be true.
//! 5. The Responses API is tried first: the gate reads tools at the top level,
//!    which is how that API spells them.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use anyhow::{anyhow, Result};
use rand::RngCore;

use crate::openai;
use crate::types::{
    APIError, Adapter as AdapterTrait, Emitter, Message, Provider, Request, Response, ToolCall,
    ToolDef,
};

/// The name the client itself sends; read from its source and confirmed against
/// a recorded request.
pub const CLIENT_AGENT_MEASURED: &str = "opencode/latest/2.0.27/cli";

const SHELL_TOOL: &str = "shell";
const READ_TOOL: &str = "read";
const B62: &[u8] = b"0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ";

/// Supplies the real builtin tool declarations, so the contract injection uses
/// the same rich definitions the gateway offers instead of stubs. Wiring this to
/// `aixodia_tools::builtin` keeps the layering acyclic (tools depends on
/// provider, not the other way round).
pub type BuiltinFn = Arc<dyn Fn() -> HashMap<String, ToolDef> + Send + Sync>;

#[derive(Default)]
struct SessionState {
    session: String,
    session_at: Option<Instant>,
    agent: Option<String>,
}

/// The OpenCode adapter.
pub struct Adapter {
    inner: openai::Adapter,
    state: Arc<Mutex<SessionState>>,
    builtin: BuiltinFn,
}

impl Adapter {
    /// Uses placeholder `shell`/`read` declarations. Call
    /// [`with_builtin`](Self::with_builtin) with `aixodia_tools::builtin` for
    /// the real ones.
    pub fn new() -> Self {
        let state = Arc::new(Mutex::new(SessionState {
            session: new_id("ses_"),
            session_at: Some(Instant::now()),
            agent: None,
        }));
        let headers_state = state.clone();
        let inner = openai::Adapter::new().with_header_func(Arc::new(move || {
            let (session, agent) = session_and_agent(&headers_state);
            headers(&session, agent.as_deref())
        }));
        Self {
            inner,
            state,
            builtin: Arc::new(HashMap::new),
        }
    }

    /// Injects the real builtin declarations for the contract.
    pub fn with_builtin(mut self, builtin: BuiltinFn) -> Self {
        self.builtin = builtin;
        self
    }

    /// Overrides the user agent.
    pub fn with_agent(self, agent: impl Into<String>) -> Self {
        self.state.lock().unwrap().agent = Some(agent.into());
        self
    }

    pub fn user_agent(&self) -> String {
        self.state
            .lock()
            .unwrap()
            .agent
            .clone()
            .unwrap_or_else(|| CLIENT_AGENT_MEASURED.to_string())
    }

    /// The id this engine calls itself in every header that carries one. It is
    /// replaced once an hour so a long-running engine never holds the same id it
    /// started with.
    pub fn session_id(&self) -> String {
        session_and_agent(&self.state).0
    }

    /// Answers a request, and knows one thing the ladder does not: a picture may
    /// be the reason the whole ladder failed.
    async fn stream_ladder(
        &self,
        req: &Request,
        prov: &Provider,
        key: &str,
        emit: &mut dyn Emitter,
    ) -> Result<Response> {
        let emitted = Arc::new(AtomicBool::new(false));
        let result = {
            let mut guard = |s: &str| -> Result<()> {
                emitted.store(true, Ordering::SeqCst);
                emit.emit(s)
            };
            self.ladder(req, prov, key, &mut guard).await
        };
        let resp = match result {
            Ok(resp) => return Ok(resp),
            Err(err) => err,
        };
        if emitted.load(Ordering::SeqCst)
            || !carries_images(&req.messages)
            || !worth_retrying_without_images(&resp)
        {
            return Err(resp);
        }
        let stripped = without_images(req);
        let mut guard = |s: &str| -> Result<()> {
            emitted.store(true, Ordering::SeqCst);
            emit.emit(s)
        };
        match self.ladder(&stripped, prov, key, &mut guard).await {
            Ok(retried) => Ok(retried),
            // Report the original failure: it is the one that describes what
            // was actually asked for.
            Err(_) => Err(resp),
        }
    }

    async fn ladder(
        &self,
        req: &Request,
        prov: &Provider,
        key: &str,
        guarded: &mut dyn Emitter,
    ) -> Result<Response> {
        let emitted = Arc::new(AtomicBool::new(false));
        let note = |trace: &mut Vec<String>, name: &str, err: &anyhow::Error| {
            trace.push(format!("{name}: {}", describe_err(err)));
        };
        let fail = |trace: &[String], err: anyhow::Error| -> anyhow::Error {
            let attempts = trace.join(" | ");
            if let Some(api) = err.downcast_ref::<APIError>() {
                return APIError::new(
                    api.status_code,
                    format!("{} [attempts: {attempts}]", api.message),
                )
                .into_anyhow();
            }
            if trace.is_empty() {
                err
            } else {
                anyhow!("{err} [attempts: {attempts}]")
            }
        };

        {
            let mut guard = |s: &str| -> Result<()> {
                emitted.store(true, Ordering::SeqCst);
                guarded.emit(s)
            };

            let mut trace: Vec<String> = Vec::new();

            // Attempt 1: the Responses API with the contract tools. This order
            // is the whole fix for the free tier.
            let mut prepared = req.clone();
            prepared.tools = self.contract_tools(&req.tools);
            match self
                .inner
                .stream_via_responses(&prepared, prov, key, &mut guard)
                .await
            {
                Ok(resp) => return Ok(resp),
                Err(err) => {
                    note(&mut trace, "responses-stream-tools", &err);
                    if emitted.load(Ordering::SeqCst) || !is_tool_protocol_error(&err) {
                        return Err(fail(&trace, err));
                    }
                }
            }

            // Attempt 2: chat completions with the contract tools.
            match self.inner.stream(&prepared, prov, key, &mut guard).await {
                Ok(resp) => return Ok(resp),
                Err(err) => {
                    note(&mut trace, "chat-stream-tools", &err);
                    if emitted.load(Ordering::SeqCst) || !is_tool_protocol_error(&err) {
                        return Err(fail(&trace, err));
                    }
                }
            }

            // Attempt 3: same streaming mode, no tools.
            let mut stripped = req.clone();
            stripped.tools = Vec::new();
            match self.inner.stream(&stripped, prov, key, &mut guard).await {
                Ok(resp) => return Ok(resp),
                Err(err) => {
                    note(&mut trace, "chat-stream-bare", &err);
                    if emitted.load(Ordering::SeqCst) || !is_tool_protocol_error(&err) {
                        return Err(fail(&trace, err));
                    }
                }
            }

            // Attempt 4: non-streaming, no tools.
            match self.inner.complete(&stripped, prov, key).await {
                Ok(bare) => {
                    guarded.emit(&bare.content)?;
                    return Ok(bare);
                }
                Err(err) => {
                    note(&mut trace, "chat-complete-bare", &err);
                    if !is_tool_protocol_error(&err) {
                        return Err(fail(&trace, err));
                    }
                }
            }

            // Attempt 5: Responses API, streaming, full tool set.
            match self
                .inner
                .stream_via_responses(&prepared, prov, key, &mut guard)
                .await
            {
                Ok(resp) => return Ok(resp),
                Err(err) => {
                    note(&mut trace, "responses-stream-tools", &err);
                    if emitted.load(Ordering::SeqCst) || !is_tool_protocol_error(&err) {
                        return Err(fail(&trace, err));
                    }
                }
            }

            // Attempt 6: Responses API, full tool set, non-streaming.
            match self
                .inner
                .complete_via_responses(&prepared, prov, key)
                .await
            {
                Ok(resp) => {
                    guarded.emit(&resp.content)?;
                    return Ok(resp);
                }
                Err(err) => {
                    note(&mut trace, "responses-complete-tools", &err);
                    if emitted.load(Ordering::SeqCst) || !is_tool_protocol_error(&err) {
                        return Err(fail(&trace, err));
                    }
                }
            }

            // Attempt 7: Responses API bare.
            match self
                .inner
                .complete_via_responses(&stripped, prov, key)
                .await
            {
                Ok(resp) => {
                    guarded.emit(&resp.content)?;
                    Ok(resp)
                }
                Err(err) => {
                    note(&mut trace, "responses-complete-bare", &err);
                    Err(fail(&trace, err))
                }
            }
        }
    }

    fn contract_tools(&self, given: &[ToolDef]) -> Vec<ToolDef> {
        let mut present: HashMap<&str, bool> = HashMap::new();
        for def in given {
            present.insert(def.name.as_str(), true);
        }
        let mut out = given.to_vec();
        let real = (self.builtin)();
        // The free-tier gate only passes the full rich declarations (it rejects
        // stub shells), so inject the real builtin defs — never minimal
        // sketches.
        for name in [SHELL_TOOL, READ_TOOL] {
            if !present.contains_key(name) {
                out.push(real.get(name).cloned().unwrap_or_else(|| placeholder(name)));
            }
        }
        out
    }
}

impl Default for Adapter {
    fn default() -> Self {
        Self::new()
    }
}

#[async_trait::async_trait]
impl AdapterTrait for Adapter {
    fn name(&self) -> &str {
        "opencode"
    }

    async fn complete(&self, req: &Request, prov: &Provider, key: &str) -> Result<Response> {
        let mut content = String::new();
        let mut resp = self
            .stream(req, prov, key, &mut |delta: &str| {
                content.push_str(delta);
                Ok(())
            })
            .await?;
        // Prefer streamed deltas, but some paths (e.g. the Responses API
        // fallback resolved from a completed object) return full content
        // without emitting.
        if !content.is_empty() {
            resp.content = content;
        }
        Ok(resp)
    }

    async fn stream(
        &self,
        req: &Request,
        prov: &Provider,
        key: &str,
        emit: &mut dyn Emitter,
    ) -> Result<Response> {
        self.stream_ladder(req, prov, key, emit).await
    }
}

fn session_and_agent(state: &Arc<Mutex<SessionState>>) -> (String, Option<String>) {
    let mut state = state.lock().unwrap();
    let stale = match state.session_at {
        Some(at) => at.elapsed() > Duration::from_secs(3600),
        None => true,
    };
    if stale {
        state.session = new_id("ses_");
        state.session_at = Some(Instant::now());
    }
    (state.session.clone(), state.agent.clone())
}

/// Everything the request-id headers would have carried is already in the
/// message and session headers, so the engine sends none of its own: the gate
/// reads an `x-opencode-request` header as a request that did not come from the
/// client and refuses it.
fn headers(session: &str, agent: Option<&str>) -> HashMap<String, String> {
    let agent = agent.unwrap_or(CLIENT_AGENT_MEASURED);
    HashMap::from([
        ("User-Agent".to_string(), agent.to_string()),
        ("x-opencode-client".to_string(), "cli".to_string()),
        ("x-opencode-project".to_string(), "global".to_string()),
        ("x-opencode-session".to_string(), session.to_string()),
        ("x-opencode-session-id".to_string(), session.to_string()),
        ("x-session-affinity".to_string(), session.to_string()),
        ("x-session-id".to_string(), session.to_string()),
    ])
}

/// Whether any message would put a picture on the wire.
fn carries_images(messages: &[Message]) -> bool {
    messages.iter().any(|m| !m.images.is_empty())
}

/// The same conversation with the pictures taken out and the last user message
/// told why, so the answer can be honest about what it saw.
fn without_images(req: &Request) -> Request {
    let mut out = req.clone();
    let mut last = None;
    for i in (0..out.messages.len()).rev() {
        if !out.messages[i].images.is_empty() {
            out.messages[i].images.clear();
            if last.is_none() && out.messages[i].role == "user" {
                last = Some(i);
            }
        }
    }
    if let Some(i) = last {
        let trimmed = out.messages[i].content.trim_end_matches('\n');
        out.messages[i].content = format!(
            "{trimmed}\n\n[the attached pictures were removed: this model does not accept images, so answer from the text alone and say that you could not see them]"
        );
    }
    out
}

/// Whether asking again without the pictures could plausibly succeed. The only
/// failures that genuinely cannot be helped by removing the pictures are the ones
/// about the key or the balance, so those are excluded and everything else is
/// retried once.
fn worth_retrying_without_images(err: &anyhow::Error) -> bool {
    let Some(api) = err.downcast_ref::<APIError>() else {
        return true;
    };
    match api.status_code {
        401 | 402 => false,
        403 => {
            // A 403 is also what a payload rejection looks like, so it is only
            // skipped when the message is plainly about credentials.
            let message = api.message.to_lowercase();
            !["key", "credit", "quota", "balance", "payment"]
                .iter()
                .any(|hint| message.contains(hint))
        }
        _ => true,
    }
}

/// Condenses an upstream error to `status: snippet` for traces.
fn describe_err(err: &anyhow::Error) -> String {
    let message = match err.downcast_ref::<APIError>() {
        Some(api) => format!("http{} {}", api.status_code, api.message),
        None => err.to_string(),
    };
    if message.chars().count() > 140 {
        let mut truncated: String = message.chars().take(140).collect();
        truncated.push('…');
        truncated
    } else {
        message
    }
}

/// Whether `err` looks like an upstream rejection of the function-calling
/// payload itself, in which case a tools-less retry is worth one attempt.
fn is_tool_protocol_error(err: &anyhow::Error) -> bool {
    let Some(api) = err.downcast_ref::<APIError>() else {
        return false;
    };
    let message = api.message.to_lowercase();
    // The free-tier gate rejects shapes without the full tool declarations,
    // usually as 403. A later attempt with the full tool set may still pass.
    if message.contains("free tier") && message.contains("opencode") {
        return true;
    }
    if api.status_code != 400 {
        return false;
    }
    if message.contains("protocol") {
        return true;
    }
    let has_tool_word = message.contains("tool") || message.contains("function");
    let has_reject_word = message.contains("support")
        || message.contains("invalid")
        || message.contains("not allowed")
        || message.contains("refuse");
    has_tool_word && has_reject_word
}

fn placeholder(name: &str) -> ToolDef {
    // The zen free-tier contract expects shell/read to be declared. They are
    // executed for real by the gateway, so describe them truthfully.
    let (description, properties, required) = if name == SHELL_TOOL {
        (
            "Executes a shell command on the host (sh -c) and returns combined stdout and stderr output.",
            serde_json::json!({
                "command": { "type": "string", "description": "Shell command to execute." }
            }),
            vec!["command"],
        )
    } else {
        (
            "Reads a text file from the host filesystem and returns its contents.",
            serde_json::json!({
                "path": { "type": "string", "description": "Absolute path of the file to read." }
            }),
            vec!["path"],
        )
    };
    let parameters = serde_json::json!({
        "type": "object",
        "properties": properties,
        "required": required,
    })
    .as_object()
    .cloned()
    .unwrap_or_default();
    ToolDef {
        name: name.to_string(),
        description: description.to_string(),
        parameters,
    }
}

// --- id generation -------------------------------------------------------

static ID_STATE: Mutex<(i64, i64)> = Mutex::new((0, 0));

/// The six-byte head the client puts on every id it hands out: the current
/// milliseconds shifted up twelve bits to leave room for a per-millisecond
/// counter, added, inverted so that ids sort newest-first as text, and kept to
/// six bytes.
///
/// The free tier reads two things out of the id: the low twelve bits (the
/// counter, which must be below 64) and the tail (fourteen letters/digits). The
/// timestamp in between is not consulted.
fn stamp() -> i64 {
    let millis = now_millis();
    let mut state = ID_STATE.lock().unwrap();
    if millis != state.0 {
        state.0 = millis;
        state.1 = 0;
    }
    state.1 += 1;
    !((millis << 12).wrapping_add(state.1))
}

fn now_millis() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

fn new_id(prefix: &str) -> String {
    let encoded = stamp();
    let mut raw = [0u8; 6];
    for (i, byte) in raw.iter_mut().enumerate() {
        *byte = (encoded >> (40 - 8 * i)) as u8;
    }

    let mut noise = [0u8; 14];
    if rand::thread_rng().try_fill_bytes(&mut noise).is_err() {
        for (i, byte) in noise.iter_mut().enumerate() {
            *byte = (encoded >> ((i % 8) * 8)) as u8;
        }
    }
    let suffix: String = noise
        .iter()
        .map(|byte| B62[*byte as usize % B62.len()] as char)
        .collect();
    format!("{prefix}{}{}", hex::encode(raw), suffix)
}

/// Re-exported for the gateway's tool-call trail.
pub fn tool_call(id: &str, name: &str, arguments: &str) -> ToolCall {
    ToolCall {
        id: id.to_string(),
        name: name.to_string(),
        arguments: arguments.to_string(),
    }
}
