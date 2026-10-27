//! Streamed tool calls against an OpenAI-compatible upstream.
//!
//! A provider names a function once, in the fragment that opens the call, and
//! sends the arguments as bare fragments after it. The fold has to keep both, or
//! the tool arrives with the right name and nothing to act on.

use aixodia_provider::openai;
use aixodia_provider::{Adapter as AdapterTrait, Message, Provider, Request};
use axum::body::Bytes;
use axum::routing::post;
use axum::Router;

/// gpt-oss spelled exactly as it streams: id and name in the opening fragment,
/// then argument text with neither.
const STREAM: &str = r#"data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"write_file","arguments":""}}]}}]}

data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"function":{"arguments":"{\"path\""}}]}}]}

data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"function":{"arguments":": \"/tmp/x\"}"}}]}}]}

data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"tool_calls"}]}

data: [DONE]

"#;

async fn upstream(_: Bytes) -> impl axum::response::IntoResponse {
    ([("content-type", "text/event-stream")], STREAM)
}

#[tokio::test]
async fn streamed_tool_arguments_fold_into_one_call() {
    let app = Router::new().route("/v1/chat/completions", post(upstream));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });

    let adapter = openai::Adapter::new();
    let request = Request {
        model: "m".to_string(),
        messages: vec![Message {
            role: "user".to_string(),
            content: "write it".to_string(),
            ..Message::default()
        }],
        ..Request::default()
    };

    let mut emitted = String::new();
    let mut emit = |delta: &str| {
        emitted.push_str(delta);
        Ok(())
    };
    let response = adapter
        .stream(
            &request,
            &Provider {
                name: "openai-compat".to_string(),
                endpoint: format!("http://{address}"),
                keys: vec!["k".to_string()],
                ..Provider::default()
            },
            "k",
            &mut emit,
        )
        .await
        .expect("stream");

    assert_eq!(response.tool_calls.len(), 1, "one call folded");
    assert_eq!(response.tool_calls[0].id, "call-1");
    assert_eq!(response.tool_calls[0].name, "write_file");
    assert_eq!(response.tool_calls[0].arguments, r#"{"path": "/tmp/x"}"#);
    assert_eq!(response.finish_reason, "tool_calls");
    assert!(emitted.is_empty(), "a tool-only turn emits no text");
}
