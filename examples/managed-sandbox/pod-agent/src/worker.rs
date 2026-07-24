// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//! In-sandbox worker binary. Runs as the bwrap payload; exposes
//! WorkerService on a Unix socket inside the sandbox. The pod-agent is
//! the only client.

use std::os::fd::{AsFd, AsRawFd, OwnedFd};
use std::os::unix::process::ExitStatusExt;
use std::path::PathBuf;
use std::pin::Pin;
use std::process::Stdio;
use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use clap::Parser;
use futures::{Stream, StreamExt};
use nix::pty::openpty;
use tokio::io::unix::AsyncFd;
use tokio::io::AsyncReadExt;
use tokio::net::UnixListener;
use tokio::process::Command;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::{transport::Server, Request, Response, Status, Streaming};
use tracing::{error, info};

pub mod proto {
    tonic::include_proto!("agentsandbox.worker.v1");
}

use proto::exec_response::Kind as ExecKind;
use proto::open_shell_request::Kind as ShellReqKind;
use proto::open_shell_response::Kind as ShellRespKind;
use proto::worker_service_server::{WorkerService, WorkerServiceServer};
use proto::{
    ExecExit, ExecRequest, ExecResponse, OpenShellRequest, OpenShellResponse, PingRequest,
    PingResponse,
};

#[derive(Parser, Debug)]
#[command(name = "agent-sandbox-worker")]
struct Args {
    /// Unix socket path the worker listens on. Bind-mounted into the
    /// sandbox by the pod-agent.
    #[arg(long, default_value = "/run/agentsandbox/worker.sock")]
    socket: PathBuf,
}

struct Worker {
    pid: i64,
    start_time_unix_micros: i64,
}

#[tonic::async_trait]
impl WorkerService for Worker {
    async fn ping(&self, _req: Request<PingRequest>) -> Result<Response<PingResponse>, Status> {
        Ok(Response::new(PingResponse {
            pid: self.pid,
            start_time_unix_micros: self.start_time_unix_micros,
        }))
    }

    type ExecStream = Pin<Box<dyn Stream<Item = Result<ExecResponse, Status>> + Send + 'static>>;

    type OpenShellStream =
        Pin<Box<dyn Stream<Item = Result<OpenShellResponse, Status>> + Send + 'static>>;

    async fn open_shell(
        &self,
        req: Request<Streaming<OpenShellRequest>>,
    ) -> Result<Response<Self::OpenShellStream>, Status> {
        let stream = open_shell_impl(req.into_inner()).await?;
        Ok(Response::new(stream))
    }

    async fn exec(&self, req: Request<ExecRequest>) -> Result<Response<Self::ExecStream>, Status> {
        let req = req.into_inner();
        if req.argv.is_empty() {
            return Err(Status::invalid_argument("argv must be non-empty"));
        }

        let mut cmd = Command::new(&req.argv[0]);
        cmd.args(&req.argv[1..]);
        for (k, v) in &req.env {
            cmd.env(k, v);
        }
        if !req.cwd.is_empty() {
            cmd.current_dir(&req.cwd);
        }
        cmd.stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true);

        let mut child = cmd
            .spawn()
            .map_err(|e| Status::internal(format!("spawn {:?}: {e}", req.argv[0])))?;
        let mut stdout = child.stdout.take().expect("stdout piped");
        let mut stderr = child.stderr.take().expect("stderr piped");

        let (tx, rx) = tokio::sync::mpsc::channel::<Result<ExecResponse, Status>>(32);

        let tx_out = tx.clone();
        let out_jh = tokio::spawn(async move {
            pump(&mut stdout, &tx_out, true).await;
        });
        let tx_err = tx.clone();
        let err_jh = tokio::spawn(async move {
            pump(&mut stderr, &tx_err, false).await;
        });
        tokio::spawn(async move {
            let status = match child.wait().await {
                Ok(s) => s,
                Err(e) => {
                    let _ = tx
                        .send(Err(Status::internal(format!("wait failed: {e}"))))
                        .await;
                    return;
                }
            };
            let _ = out_jh.await;
            let _ = err_jh.await;
            let exit = ExecExit {
                exit_code: status.code().unwrap_or(-1),
                signal: status.signal().unwrap_or(0),
            };
            let _ = tx
                .send(Ok(ExecResponse {
                    kind: Some(ExecKind::Exit(exit)),
                }))
                .await;
        });

        let stream = tokio_stream::wrappers::ReceiverStream::new(rx);
        Ok(Response::new(Box::pin(stream) as Self::ExecStream))
    }
}

/// open_shell impl. Reads Start frame (first), then forks bash on a fresh
/// PTY. One task reads stdin frames → writes to PTY primary. Another reads
/// PTY primary → emits Stdout frames. A third waits on the child and emits
/// the Exit frame.
async fn open_shell_impl(
    mut input: Streaming<OpenShellRequest>,
) -> Result<Pin<Box<dyn Stream<Item = Result<OpenShellResponse, Status>> + Send + 'static>>, Status>
{
    let first = input
        .next()
        .await
        .ok_or_else(|| Status::invalid_argument("stream closed before Start"))?
        .map_err(|e| Status::internal(format!("recv: {e}")))?;
    let start = match first.kind {
        Some(ShellReqKind::Start(s)) => s,
        _ => return Err(Status::invalid_argument("first frame must be Start")),
    };

    let pty = openpty(None, None).map_err(|e| Status::internal(format!("openpty: {e}")))?;
    let primary: OwnedFd = pty.master;
    let secondary: OwnedFd = pty.slave;

    // Primary fd must be non-blocking so tokio's AsyncFd can drive both
    // read and write readiness without ever blocking the runtime.
    if let Err(e) = set_nonblocking(primary.as_raw_fd()) {
        return Err(Status::internal(format!("set primary O_NONBLOCK: {e}")));
    }

    if start.cols > 0 && start.rows > 0 {
        apply_winsize(primary.as_raw_fd(), start.cols, start.rows);
    }

    // Default to a login shell, preferring bash but falling back to sh.
    // Image-vs-binary mismatch (alpine ships sh, not bash) was a common
    // cause of "spawn shell: No such file or directory". The worker
    // runs inside the sandbox's mount namespace, so this Path::exists
    // check sees the user image's rootfs.
    let argv: Vec<String> = if start.argv.is_empty() {
        let shell = if std::path::Path::new("/bin/bash").exists() {
            "/bin/bash"
        } else {
            "/bin/sh"
        };
        vec![shell.to_string(), "-l".to_string()]
    } else {
        start.argv
    };

    // Safe dup via BorrowedFd::try_clone_to_owned — wraps the dup
    // syscall but returns an OwnedFd that the type system tracks.
    // Stdio::from(OwnedFd) takes ownership and closes on drop; no
    // raw-fd footguns.
    let secondary_ref = secondary.as_fd();
    let stdin = secondary_ref
        .try_clone_to_owned()
        .map_err(|e| Status::internal(format!("dup pty secondary (stdin): {e}")))?;
    let stdout = secondary_ref
        .try_clone_to_owned()
        .map_err(|e| Status::internal(format!("dup pty secondary (stdout): {e}")))?;
    let stderr = secondary_ref
        .try_clone_to_owned()
        .map_err(|e| Status::internal(format!("dup pty secondary (stderr): {e}")))?;

    let mut cmd = Command::new(&argv[0]);
    cmd.args(&argv[1..]);
    for (k, v) in &start.env {
        cmd.env(k, v);
    }
    if !start.cwd.is_empty() {
        cmd.current_dir(&start.cwd);
    }
    cmd.stdin(Stdio::from(stdin))
        .stdout(Stdio::from(stdout))
        .stderr(Stdio::from(stderr))
        .kill_on_drop(true);
    // SAFETY: pre_exec runs in the forked child between fork() and
    // execve(). The closure must be async-signal-safe and must not
    // touch shared memory the parent owns. We only call setsid +
    // TIOCSCTTY, both of which are signal-safe syscalls.
    unsafe {
        cmd.pre_exec(|| {
            nix::unistd::setsid().map_err(std::io::Error::from)?;
            // No safe wrapper for TIOCSCTTY in nix 0.29; the ioctl
            // attaches the current fd (stdin, fd 0) as the new
            // session's controlling tty.
            if libc::ioctl(0, libc::TIOCSCTTY as _, 0) < 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }
    let mut child = cmd
        .spawn()
        .map_err(|e| Status::internal(format!("spawn shell: {e}")))?;
    drop(secondary);

    let (tx, rx) = tokio::sync::mpsc::channel::<Result<OpenShellResponse, Status>>(32);

    // Wrap the PTY primary fd in an `AsyncFd` shared by both the read
    // pump and the write loop. Lifetime is managed by the Arc; once
    // every task drops its clone the underlying `OwnedFd` closes
    // naturally — no "keep this alive forever" task hacks.
    let primary = Arc::new(
        AsyncFd::new(primary)
            .map_err(|e| Status::internal(format!("AsyncFd::new(primary): {e}")))?,
    );

    // primary → client.stdout
    let tx_out = tx.clone();
    let primary_r = Arc::clone(&primary);
    let out_jh = tokio::spawn(async move {
        let mut buf = [0u8; 4096];
        loop {
            // async_io drives WouldBlock retries internally — much
            // tidier than the readable()/clear_ready() pattern.
            let n = match primary_r
                .async_io(tokio::io::Interest::READABLE, |fd| {
                    nix::unistd::read(fd.as_raw_fd(), &mut buf).map_err(std::io::Error::from)
                })
                .await
            {
                Ok(n) => n,
                Err(_) => break,
            };
            if n == 0 {
                break;
            }
            let chunk = buf[..n].to_vec();
            if tx_out
                .send(Ok(OpenShellResponse {
                    kind: Some(ShellRespKind::Stdout(chunk)),
                }))
                .await
                .is_err()
            {
                break;
            }
        }
    });

    // client → primary, resize, eof
    let primary_w = Arc::clone(&primary);
    let input_jh = tokio::spawn(async move {
        while let Some(req) = input.next().await {
            let req = match req {
                Ok(r) => r,
                Err(_) => break,
            };
            match req.kind {
                Some(ShellReqKind::Stdin(bytes)) => {
                    if !pty_write_all(&primary_w, &bytes).await {
                        break;
                    }
                }
                Some(ShellReqKind::Resize(rs)) => {
                    apply_winsize(primary_w.get_ref().as_raw_fd(), rs.cols, rs.rows);
                }
                Some(ShellReqKind::Eof(_)) => {
                    // closing the primary closes the slave's stdin too
                    break;
                }
                Some(ShellReqKind::Start(_)) | None => {}
            }
        }
    });

    // child wait → emit Exit and close
    let tx_exit = tx.clone();
    tokio::spawn(async move {
        let status = match child.wait().await {
            Ok(s) => s,
            Err(e) => {
                let _ = tx_exit
                    .send(Err(Status::internal(format!("wait: {e}"))))
                    .await;
                return;
            }
        };
        input_jh.abort();
        let _ = input_jh.await;
        let _ = out_jh.await;
        let exit = ExecExit {
            exit_code: status.code().unwrap_or(-1),
            signal: status.signal().unwrap_or(0),
        };
        let _ = tx_exit
            .send(Ok(OpenShellResponse {
                kind: Some(ShellRespKind::Exit(exit)),
            }))
            .await;
    });

    Ok(Box::pin(tokio_stream::wrappers::ReceiverStream::new(rx))
        as Pin<
            Box<dyn Stream<Item = Result<OpenShellResponse, Status>> + Send + 'static>,
        >)
}

/// Async write loop over a non-blocking PTY primary. Returns true on
/// success, false on a permanent error (caller should give up). Handles
/// EAGAIN via `AsyncFd::writable()` and partial writes by looping.
async fn pty_write_all(fd: &AsyncFd<OwnedFd>, data: &[u8]) -> bool {
    let mut off = 0;
    while off < data.len() {
        let res = fd
            .async_io(tokio::io::Interest::WRITABLE, |fd| {
                nix::unistd::write(fd.as_fd(), &data[off..]).map_err(std::io::Error::from)
            })
            .await;
        match res {
            Ok(0) => return false, // peer gone
            Ok(n) => off += n,
            Err(_) => return false, // EPIPE etc.
        }
    }
    true
}

fn set_nonblocking(fd: std::os::fd::RawFd) -> nix::Result<()> {
    use nix::fcntl::{fcntl, FcntlArg, OFlag};
    let flags = fcntl(fd, FcntlArg::F_GETFL)?;
    let flags = OFlag::from_bits_truncate(flags) | OFlag::O_NONBLOCK;
    fcntl(fd, FcntlArg::F_SETFL(flags))?;
    Ok(())
}

fn apply_winsize(fd: i32, cols: u32, rows: u32) {
    let ws = libc::winsize {
        ws_col: cols as u16,
        ws_row: rows as u16,
        ws_xpixel: 0,
        ws_ypixel: 0,
    };
    // SAFETY: TIOCSWINSZ takes a const pointer to `winsize`. We pass a
    // stack-allocated, fully-initialized struct; the kernel reads it
    // synchronously and copies the values. No safe nix wrapper for
    // TIOCSWINSZ in 0.29.
    unsafe {
        libc::ioctl(fd, libc::TIOCSWINSZ as _, &ws);
    }
}

async fn pump<R: AsyncReadExt + Unpin>(
    r: &mut R,
    tx: &tokio::sync::mpsc::Sender<Result<ExecResponse, Status>>,
    is_stdout: bool,
) {
    let mut buf = [0u8; 8192];
    loop {
        match r.read(&mut buf).await {
            Ok(0) => return,
            Ok(n) => {
                let chunk = buf[..n].to_vec();
                let resp = ExecResponse {
                    kind: Some(if is_stdout {
                        ExecKind::Stdout(chunk)
                    } else {
                        ExecKind::Stderr(chunk)
                    }),
                };
                if tx.send(Ok(resp)).await.is_err() {
                    return;
                }
            }
            Err(_) => return,
        }
    }
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .init();

    let args = Args::parse();
    if let Some(parent) = args.socket.parent() {
        std::fs::create_dir_all(parent)?;
    }
    // Clean stale socket from a prior process.
    let _ = std::fs::remove_file(&args.socket);

    let listener = UnixListener::bind(&args.socket)?;
    let incoming = UnixListenerStream::new(listener);
    info!(socket = %args.socket.display(), "worker listening");

    let worker = Worker {
        pid: std::process::id() as i64,
        start_time_unix_micros: SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_micros() as i64)
            .unwrap_or(0),
    };

    // Cap per-message size at 1 MiB. Stdin frames over OpenShell are
    // small (4 KiB typical); larger messages on the worker socket would
    // be either a bug or someone pumping abuse traffic. Keep memory use
    // bounded.
    let svc = WorkerServiceServer::new(worker)
        .max_decoding_message_size(1024 * 1024)
        .max_encoding_message_size(1024 * 1024);
    if let Err(e) = Server::builder()
        .add_service(svc)
        .serve_with_incoming(incoming)
        .await
    {
        error!(error = %e, "worker server exited with error");
        return Err(e.into());
    }
    Ok(())
}
