//! Keeping shells in a private tmux server so they survive an engine restart.
//!
//! Port of `io/terminal/tmux.go`.

use std::process::{Command, Stdio};
use std::time::Duration;

use crate::pty::Pty;

pub const TMUX_SOCKET: &str = "aixodia";

pub fn available() -> bool {
    std::env::var_os("PATH")
        .map(|paths| {
            std::env::split_paths(&paths).any(|dir| {
                let candidate = dir.join("tmux");
                candidate.is_file()
            })
        })
        .unwrap_or(false)
}

pub fn tmux_name(id: &str) -> String {
    let mut name = String::from("aix-");
    for c in id.chars() {
        if c.is_ascii_alphanumeric() || c == '-' || c == '_' {
            name.push(c);
        } else {
            name.push('_');
        }
    }
    name
}

/// Ends a shell for good. Closing the pty only detaches the client, so the tmux
/// session must be killed or the phone reattaches to a tab it closed.
pub fn kill(id: &str) {
    let _ = Command::new("tmux")
        .args(["-L", TMUX_SOCKET, "kill-session", "-t", &tmux_name(id)])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status();
}

/// One tmux session as the terminal list reports it.
pub struct TmuxSession {
    pub id: String,
    pub pid: i32,
    pub cwd: String,
    pub created_at: i64,
}

/// Sessions that survived an engine restart.
pub fn list() -> Vec<TmuxSession> {
    let output = Command::new("tmux")
        .args([
            "-L",
            TMUX_SOCKET,
            "list-sessions",
            "-F",
            "#{session_name}\u{1f}#{pane_pid}\u{1f}#{pane_current_path}\u{1f}#{session_created}",
        ])
        .output();
    let Ok(output) = output else {
        return Vec::new();
    };
    if !output.status.success() {
        return Vec::new();
    }
    let text = String::from_utf8_lossy(&output.stdout);
    let mut out = Vec::new();
    for line in text.trim().split('\n') {
        let fields: Vec<&str> = line.splitn(4, '\u{1f}').collect();
        if fields.len() < 4 {
            continue;
        }
        let Some(name) = fields[0].strip_prefix("aix-") else {
            continue;
        };
        out.push(TmuxSession {
            id: name.to_string(),
            pid: fields[1].parse().unwrap_or(0),
            cwd: fields[2].to_string(),
            created_at: fields[3].parse().unwrap_or(0),
        });
    }
    out
}

/// Opens a pty that carries a tmux client, creating the session when it is new.
pub fn start(
    id: &str,
    shell: &str,
    cols: u16,
    rows: u16,
    dir: Option<&str>,
) -> std::io::Result<Pty> {
    if !available() {
        return Err(std::io::Error::new(
            std::io::ErrorKind::NotFound,
            "tmux is not installed",
        ));
    }
    let name = tmux_name(id);
    let mut args = vec![
        "-L".to_string(),
        TMUX_SOCKET.to_string(),
        "new-session".to_string(),
        "-A".to_string(),
        "-s".to_string(),
        name.clone(),
    ];
    if let Some(dir) = dir {
        args.push("-c".to_string());
        args.push(dir.to_string());
    }
    args.push(shell.to_string());
    args.push("-l".to_string());
    let pty = Pty::start("tmux", &args, cols, rows, dir)?;
    // The status bar spends a phone row on a session name and a clock.
    let _ = Command::new("tmux")
        .args([
            "-L",
            TMUX_SOCKET,
            "set-option",
            "-t",
            &name,
            "status",
            "off",
        ])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status();
    std::thread::sleep(Duration::from_millis(0));
    Ok(pty)
}
