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

//! Embedded SSH server in the pod-agent.
//!
//! ## Auth
//!   * Username = Sandbox CR UID
//!   * Password = 32-byte hex `session_token` provided by the controller
//!     when it called `CreateSandbox`.
//!
//! ## Shell spawn
//!   * The pod-agent terminates SSH and authenticates the user.
//!   * The shell is spawned **inside the sandbox** by the per-tenant
//!     worker (`WorkerService.OpenShell`), reached over the Unix socket
//!     that was bind-mounted into the sandbox by bwrap.
//!   * No `nsenter` / `setns` on the pod-agent side. This shape ports
//!     directly to Firecracker: swap the Unix-socket transport for vsock.

use std::path::PathBuf;
use std::sync::Arc;

use async_trait::async_trait;
use parking_lot::Mutex;
use russh::server::{Auth, Handle, Msg, Server, Session};
use russh::MethodSet;
use russh::{Channel, ChannelId, CryptoVec};
use ssh_key::{rand_core::OsRng, Algorithm as SshAlgorithm, PrivateKey};
use subtle::ConstantTimeEq;
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;

use crate::worker_client::worker_proto::{
    open_shell_request::Kind as ShellReqKind, open_shell_response::Kind as ShellRespKind,
    worker_service_client::WorkerServiceClient, OpenShellRequest, OpenShellResize, OpenShellStart,
};
use crate::Registry;

/// Start the SSH server bound to `0.0.0.0:port`. Runs forever (or until the
/// listener task errors out).
pub async fn run(state: Arc<Mutex<Registry>>, port: u16) -> anyhow::Result<()> {
    let key = PrivateKey::random(&mut OsRng, SshAlgorithm::Ed25519)
        .map_err(|e| anyhow::anyhow!("generate ssh host key: {e}"))?;
    let config = russh::server::Config {
        methods: MethodSet::PASSWORD,
        inactivity_timeout: Some(std::time::Duration::from_secs(3600)),
        auth_rejection_time: std::time::Duration::from_secs(1),
        keys: vec![key],
        ..Default::default()
    };
    let config = Arc::new(config);
    let mut server = SshServer { state };
    let addr = format!("0.0.0.0:{port}");
    tracing::info!(addr, "ssh server listening");
    server.run_on_address(config, addr).await?;
    Ok(())
}

#[derive(Clone)]
pub struct SshServer {
    state: Arc<Mutex<Registry>>,
}

impl Server for SshServer {
    type Handler = SshSession;
    fn new_client(&mut self, _peer: Option<std::net::SocketAddr>) -> SshSession {
        SshSession {
            state: self.state.clone(),
            uid: None,
            shell_tx: None,
            initial_pty: None,
        }
    }
}

pub struct SshSession {
    state: Arc<Mutex<Registry>>,
    /// Sandbox UID this session authenticated as.
    uid: Option<String>,
    /// Sender into the open `OpenShell` request stream. None until
    /// `shell_request` succeeds.
    shell_tx: Option<mpsc::Sender<OpenShellRequest>>,
    /// PTY params received via `pty_request` before `shell_request`.
    initial_pty: Option<(u32, u32)>,
}

#[async_trait]
impl russh::server::Handler for SshSession {
    type Error = russh::Error;

    async fn auth_password(&mut self, user: &str, password: &str) -> Result<Auth, Self::Error> {
        let reg = self.state.lock();
        let entry = match reg.entries.get(user) {
            Some(e) => e,
            None => return Ok(Auth::Reject { proceed_with_methods: None }),
        };
        if entry.session_token.is_empty() {
            return Ok(Auth::Reject { proceed_with_methods: None });
        }
        // Constant-time compare so a network attacker can't probe the
        // token byte-by-byte via response-time differences.
        if entry
            .session_token
            .as_bytes()
            .ct_eq(password.as_bytes())
            .unwrap_u8()
            == 0
        {
            return Ok(Auth::Reject { proceed_with_methods: None });
        }
        drop(reg);
        self.uid = Some(user.to_string());
        tracing::info!(uid = %user, "ssh auth success");
        Ok(Auth::Accept)
    }

    async fn auth_publickey(
        &mut self,
        _user: &str,
        _key: &russh::keys::ssh_key::PublicKey,
    ) -> Result<Auth, Self::Error> {
        Ok(Auth::Reject { proceed_with_methods: None })
    }

    async fn channel_open_session(
        &mut self,
        _channel: Channel<Msg>,
        _session: &mut Session,
    ) -> Result<bool, Self::Error> {
        Ok(true)
    }

    async fn pty_request(
        &mut self,
        _channel: ChannelId,
        _term: &str,
        col_width: u32,
        row_height: u32,
        _pix_width: u32,
        _pix_height: u32,
        _modes: &[(russh::Pty, u32)],
        _session: &mut Session,
    ) -> Result<(), Self::Error> {
        self.initial_pty = Some((col_width, row_height));
        Ok(())
    }

    async fn window_change_request(
        &mut self,
        _channel: ChannelId,
        col_width: u32,
        row_height: u32,
        _pix_width: u32,
        _pix_height: u32,
        _session: &mut Session,
    ) -> Result<(), Self::Error> {
        if let Some(tx) = &self.shell_tx {
            let _ = tx
                .send(OpenShellRequest {
                    kind: Some(ShellReqKind::Resize(OpenShellResize {
                        cols: col_width,
                        rows: row_height,
                    })),
                })
                .await;
        }
        Ok(())
    }

    async fn shell_request(
        &mut self,
        channel: ChannelId,
        session: &mut Session,
    ) -> Result<(), Self::Error> {
        let uid = match &self.uid {
            Some(u) => u.clone(),
            None => {
                let _ = session.close(channel);
                return Ok(());
            }
        };
        let worker_sock = match worker_socket_for(&self.state, &uid) {
            Some(p) => p,
            None => {
                let _ = session.data(
                    channel,
                    CryptoVec::from("tenant has no worker socket\r\n".to_string()),
                );
                let _ = session.close(channel);
                return Ok(());
            }
        };

        // Build the bidi stream: an mpsc we can push into (stdin/resize),
        // wrapped as a ReceiverStream we hand to tonic.
        let (tx, rx) = mpsc::channel::<OpenShellRequest>(64);
        let (cols, rows) = self.initial_pty.unwrap_or((80, 24));
        if tx
            .send(OpenShellRequest {
                kind: Some(ShellReqKind::Start(OpenShellStart {
                    // Empty argv → worker picks /bin/bash or /bin/sh
                    // depending on what the sandbox image actually ships.
                    argv: vec![],
                    env: Default::default(),
                    cwd: String::new(),
                    cols,
                    rows,
                })),
            })
            .await
            .is_err()
        {
            let _ = session.close(channel);
            return Ok(());
        }

        let handle = session.handle();
        let uid_for_task = uid.clone();
        tokio::spawn(async move {
            if let Err(e) =
                run_shell_session(worker_sock, rx, handle.clone(), channel).await
            {
                tracing::warn!(uid = %uid_for_task, error = %format!("{e:#}"), "shell session ended");
                let _ = handle
                    .data(channel, CryptoVec::from(format!("shell error: {e}\r\n")))
                    .await;
                let _ = handle.close(channel).await;
            }
        });

        self.shell_tx = Some(tx);
        Ok(())
    }

    async fn data(
        &mut self,
        _channel: ChannelId,
        data: &[u8],
        _session: &mut Session,
    ) -> Result<(), Self::Error> {
        if let Some(tx) = &self.shell_tx {
            let _ = tx
                .send(OpenShellRequest {
                    kind: Some(ShellReqKind::Stdin(data.to_vec())),
                })
                .await;
        }
        Ok(())
    }
}

fn worker_socket_for(state: &Arc<Mutex<Registry>>, uid: &str) -> Option<PathBuf> {
    let reg = state.lock();
    reg.entries
        .get(uid)
        .and_then(|e| e.sandbox.as_ref())
        .map(|t| t.fs.paths.worker_socket())
}

async fn run_shell_session(
    worker_sock: PathBuf,
    rx: mpsc::Receiver<OpenShellRequest>,
    handle: Handle,
    channel: ChannelId,
) -> anyhow::Result<()> {
    let chan = crate::worker_client::connect(worker_sock).await?;
    let mut client = WorkerServiceClient::new(chan);
    let response = client.open_shell(ReceiverStream::new(rx)).await?;
    let mut stream = response.into_inner();
    while let Some(msg) = stream.message().await? {
        match msg.kind {
            Some(ShellRespKind::Stdout(bytes)) | Some(ShellRespKind::Stderr(bytes)) => {
                if handle.data(channel, CryptoVec::from(bytes)).await.is_err() {
                    break;
                }
            }
            Some(ShellRespKind::Exit(exit)) => {
                let code = if exit.signal != 0 {
                    // Signal-terminated; report 128+signal as ssh convention.
                    (128 + exit.signal) as u32
                } else {
                    exit.exit_code.max(0) as u32
                };
                let _ = handle.exit_status_request(channel, code).await;
                let _ = handle.close(channel).await;
                return Ok(());
            }
            None => {}
        }
    }
    let _ = handle.close(channel).await;
    Ok(())
}
