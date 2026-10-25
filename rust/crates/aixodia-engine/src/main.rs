//! aixodia-engine: the engine binary.
//!
//! Rust port of the Go `main` package. Only the modules that have been ported
//! so far are declared; the boot sequence is still to come (see
//! `rust/MILESTONES.md`), so nothing in this binary calls them yet.

// The boot sequence that would call these modules is not ported, so the
// compiler is right that nothing here reaches them; each module is exercised
// by its own tests. Remove this once `main` is ported.
#![allow(dead_code)]

mod d1log;
mod handover;
mod tunnel;

fn main() {
    eprintln!("aixodia-engine: Rust port in progress (see rust/MILESTONES.md)");
}
