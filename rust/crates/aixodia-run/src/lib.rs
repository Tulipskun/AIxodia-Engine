//! Long-lived agent turns: numbered, replayable events and the run API.
//!
//! Port of the Go `run` package.

pub mod api;
pub mod event;
pub mod manager;
pub mod sink;
pub mod store;

pub use api::router;
pub use event::{
    Attachment, ChatJob, Event, Run, StartRequest, Status, KIND_APPROVAL, KIND_ASSISTANT,
    KIND_ERROR, KIND_FINAL, KIND_REASONING, KIND_STATUS, KIND_TOOL, KIND_TURN, KIND_USER,
};
pub use manager::{sort_events, ApprovalDecision, ChatExecutor, Manager, Subscription};
pub use sink::SseSink;
pub use store::{D1Store, MemoryStore, Store};
