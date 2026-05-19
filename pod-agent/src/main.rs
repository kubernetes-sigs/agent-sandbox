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

//! In-pod bubblewrap tenant manager for agent-sandbox multi-tenant Sandboxes.
//!
//! This is the pod-side counterpart of the Go SandboxReconciler running in
//! the controller. The controller addresses one pod-agent per pool pod and
//! drives it via the gRPC API defined in
//! `proto/agentsandbox/podagent/v1/podagent.proto`.
//!
//! Each gRPC RPC is idempotent by `sandbox_uid`. State lives in three
//! places, in order of authority:
//!   * The Sandbox CR (durable, controlled by the controller).
//!   * The pool PVC at `/var/lib/sandboxes/<uid>/` (overlay upper).
//!   * Kernel resources (cgroups, mounts, netns) tagged by UID.
//!
//! There is no state.json: on restart the agent rebuilds its in-memory map
//! by scanning the kernel and PVC.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;

use clap::Parser;
use parking_lot::Mutex;
use tonic::{transport::Server, Request, Response, Status};
use tracing::{error, info};

mod sandbox;
mod ssh;
mod worker_client;

pub mod proto {
    tonic::include_proto!("agentsandbox.podagent.v1");
}

use proto::pod_agent_service_server::{PodAgentService, PodAgentServiceServer};
use proto::{
    CreateSandboxRequest, CreateSandboxResponse, DeleteSandboxRequest, DeleteSandboxResponse,
    GetSandboxRequest, GetSandboxResponse, ListSandboxesRequest, ListSandboxesResponse, Phase,
    SandboxState,
};
use sandbox::{Sandbox, SandboxPaths};

#[derive(Parser, Debug)]
#[command(name = "pod-agent", about = "Agent-sandbox in-pod tenant manager")]
struct Args {
    /// TCP port for the gRPC server.
    #[arg(long, default_value_t = 7443)]
    grpc_port: u16,

    /// TCP port for the HTTP reverse proxy. Reserved; not yet wired.
    #[arg(long, default_value_t = 8080)]
    proxy_port: u16,

    /// TCP port for the embedded SSH server. 0 disables SSH.
    #[arg(long, default_value_t = 2222)]
    ssh_port: u16,

    /// Maximum number of bubblewrap tenants this pod will host.
    #[arg(long, default_value_t = 10)]
    capacity: usize,

    /// Per-tenant overlay upper directories live here, on a PVC mounted by
    /// the pool pod.
    #[arg(long, default_value = "/var/lib/sandboxes")]
    state_dir: PathBuf,

    /// Base rootfs (overlay lower) mounted read-only by the pool pod from
    /// the OCI image volume source.
    #[arg(long, default_value = "/sandbox-image")]
    image_dir: PathBuf,

    /// OCI reference of the image mounted at `--image-dir`. Cross-checked
    /// against each CreateSandbox request; mismatch is an error.
    #[arg(long, env = "POD_AGENT_IMAGE_REFERENCE", default_value = "")]
    image_reference: String,

    /// Host path of the agent-sandbox-worker binary that will be bind-
    /// mounted into each tenant and exec'd as the bwrap payload. Defaults
    /// to the standard install location inside the pod-agent's own image.
    #[arg(long, default_value = "/usr/local/bin/agent-sandbox-worker")]
    worker_bin: PathBuf,
}

/// In-memory tenant registry. The agent is the runtime authority; the
/// controller is the durable authority. Both reconcile against each other.
///
/// Entries hold both the proto-visible state and the live `Sandbox`
/// handle (None for the brief windows when the sandbox is being created
/// or torn down, or when boot recovery has reattached state but not the
/// process).
pub struct Entry {
    pub state: SandboxState,
    pub sandbox: Option<Sandbox>,
    /// Session token (32-byte hex). Compared against the SSH client's
    /// password (username = sandbox_uid).
    pub session_token: String,
}

#[derive(Default)]
pub struct Registry {
    pub entries: HashMap<String, Entry>,
}

struct Agent {
    args: Args,
    state: Arc<Mutex<Registry>>,
}

#[tonic::async_trait]
impl PodAgentService for Agent {
    async fn create_sandbox(
        &self,
        req: Request<CreateSandboxRequest>,
    ) -> Result<Response<CreateSandboxResponse>, Status> {
        let req = req.into_inner();
        if !is_valid_uid(&req.sandbox_uid) {
            return Err(Status::invalid_argument(
                "sandbox_uid must be a non-empty string of [A-Za-z0-9_-] (used as a path component)",
            ));
        }
        if !self.args.image_reference.is_empty()
            && !req.image_reference.is_empty()
            && req.image_reference != self.args.image_reference
        {
            return Err(Status::failed_precondition(format!(
                "image mismatch: pod-agent serves {:?}, request asked for {:?}",
                self.args.image_reference, req.image_reference
            )));
        }

        // Reserve a slot under the lock: idempotency + capacity + slot
        // commit happen atomically, so concurrent callers can't both pass
        // the capacity check and then race to insert. The reservation is
        // a placeholder Entry with tenant=None; the real launch happens
        // outside the lock.
        let creating_state = SandboxState {
            sandbox_uid: req.sandbox_uid.clone(),
            phase: Phase::Creating as i32,
            exit_code: None,
            reason: String::new(),
            message: String::new(),
        };
        {
            let mut reg = self.state.lock();
            if let Some(existing) = reg.entries.get(&req.sandbox_uid) {
                return Ok(Response::new(CreateSandboxResponse {
                    sandbox_uid: req.sandbox_uid.clone(),
                    state: Some(existing.state.clone()),
                }));
            }
            if reg.entries.len() >= self.args.capacity {
                return Err(Status::resource_exhausted(format!(
                    "pod-agent at capacity ({} tenants)",
                    self.args.capacity
                )));
            }
            reg.entries.insert(
                req.sandbox_uid.clone(),
                Entry {
                    state: creating_state.clone(),
                    sandbox: None,
                    session_token: req.session_token.clone(),
                },
            );
        }

        let workspace = if req.workspace_mount_path.is_empty() {
            "/home".to_string()
        } else {
            req.workspace_mount_path.clone()
        };
        let paths = SandboxPaths::for_uid(&self.args.state_dir, &req.sandbox_uid);
        let tenant = match sandbox::launch(
            paths.clone(),
            &self.args.image_dir,
            &self.args.worker_bin,
            &workspace,
        )
        .await
        {
            Ok(t) => t,
            Err(e) => {
                // Roll back the reservation so the slot is freed.
                self.state.lock().entries.remove(&req.sandbox_uid);
                error!(uid = %req.sandbox_uid, error = %format!("{e:#}"), "launch failed");
                return Err(Status::internal(format!("launch failed: {e:#}")));
            }
        };

        let state = SandboxState {
            sandbox_uid: req.sandbox_uid.clone(),
            phase: Phase::Running as i32,
            exit_code: None,
            reason: String::new(),
            message: String::new(),
        };
        {
            let mut reg = self.state.lock();
            // The slot we reserved is still ours: nothing else could have
            // inserted under the same uid (it would have seen our
            // placeholder). Upgrade in place.
            let entry = reg
                .entries
                .get_mut(&req.sandbox_uid)
                .expect("reservation was removed unexpectedly");
            entry.state = state.clone();
            entry.sandbox = Some(tenant);
        }
        info!(uid = %req.sandbox_uid, name = %req.sandbox_name, "tenant launched");
        Ok(Response::new(CreateSandboxResponse {
            sandbox_uid: req.sandbox_uid,
            state: Some(state),
        }))
    }

    async fn get_sandbox(
        &self,
        req: Request<GetSandboxRequest>,
    ) -> Result<Response<GetSandboxResponse>, Status> {
        let uid = req.into_inner().sandbox_uid;
        let reg = self.state.lock();
        match reg.entries.get(&uid) {
            Some(e) => Ok(Response::new(GetSandboxResponse {
                state: Some(e.state.clone()),
            })),
            None => Err(Status::not_found(format!("sandbox {} not found", uid))),
        }
    }

    async fn delete_sandbox(
        &self,
        req: Request<DeleteSandboxRequest>,
    ) -> Result<Response<DeleteSandboxResponse>, Status> {
        let uid = req.into_inner().sandbox_uid;
        let tenant = {
            let mut reg = self.state.lock();
            match reg.entries.remove(&uid) {
                Some(e) => e.sandbox,
                None => return Ok(Response::new(DeleteSandboxResponse {})),
            }
        };
        if let Some(t) = tenant {
            if let Err(e) = t.stop().await {
                error!(uid = %uid, error = %format!("{e:#}"), "tenant stop failed");
                return Err(Status::internal(format!("stop failed: {e:#}")));
            }
        }
        info!(uid = %uid, "tenant deleted");
        Ok(Response::new(DeleteSandboxResponse {}))
    }

    async fn list_sandboxes(
        &self,
        _req: Request<ListSandboxesRequest>,
    ) -> Result<Response<ListSandboxesResponse>, Status> {
        let reg = self.state.lock();
        Ok(Response::new(ListSandboxesResponse {
            sandboxes: reg.entries.values().map(|e| e.state.clone()).collect(),
        }))
    }
}

/// Rejects sandbox_uids that aren't safe to use as a single path
/// component. The controller only ever passes K8s CR UIDs (UUIDs) but
/// the pod-agent gRPC has no authentication today, so we validate
/// defensively to keep callers from writing outside `state_dir`.
fn is_valid_uid(uid: &str) -> bool {
    !uid.is_empty()
        && uid.len() <= 64
        && uid
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
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
    info!(
        grpc_port = args.grpc_port,
        capacity = args.capacity,
        state_dir = %args.state_dir.display(),
        image_dir = %args.image_dir.display(),
        "pod-agent starting"
    );

    std::fs::create_dir_all(&args.state_dir)?;

    let addr = format!("0.0.0.0:{}", args.grpc_port).parse()?;
    let state = Arc::new(Mutex::new(Registry::default()));

    if args.ssh_port != 0 {
        let ssh_state = state.clone();
        let port = args.ssh_port;
        tokio::spawn(async move {
            if let Err(e) = ssh::run(ssh_state, port).await {
                tracing::error!(error = %format!("{e:#}"), "ssh server exited");
            }
        });
    }

    // Run pre-flight checks before serving traffic. Only flip the
    // gRPC health-check to SERVING after they pass; otherwise stay
    // NOT_SERVING (the default), so kubelet's readiness probe keeps
    // the pod out of Service endpoints until we're actually able to
    // launch sandboxes.
    let (mut health_reporter, health_svc) = tonic_health::server::health_reporter();
    match preflight_checks(&args) {
        Ok(()) => {
            health_reporter
                .set_serving::<PodAgentServiceServer<Agent>>()
                .await;
            info!("pre-flight checks passed; pod-agent SERVING");
        }
        Err(e) => {
            tracing::error!(error = %format!("{e:#}"),
                            "pre-flight checks FAILED; pod-agent will stay NOT_SERVING");
        }
    }

    let agent = Agent { args, state };
    // Cap per-message size at 1 MiB. Our payloads (CreateSandboxRequest,
    // SandboxState, etc.) are tiny; we don't want a misbehaving (or
    // compromised) caller to be able to OOM the pod-agent with multi-MiB
    // messages on a long-lived connection.
    let svc = PodAgentServiceServer::new(agent)
        .max_decoding_message_size(1024 * 1024)
        .max_encoding_message_size(1024 * 1024);
    // Wire SIGTERM/SIGINT into tonic's graceful shutdown so kubelet's
    // termination flow drains the gRPC server in < 1 s instead of
    // waiting out the pod's termination grace period. The SSH server +
    // ssh sessions get killed when this future resolves and `main`
    // returns; bwrap children die via `--die-with-parent`.
    Server::builder()
        .add_service(health_svc)
        .add_service(svc)
        .serve_with_shutdown(addr, shutdown_signal()?)
        .await?;
    info!("pod-agent shutdown complete");
    Ok(())
}

fn shutdown_signal() -> Result<impl std::future::Future<Output = ()>, anyhow::Error> {
    use tokio::signal::unix::{signal, SignalKind};
    let mut sigterm =  signal(SignalKind::terminate()).map_err(|e| {
        anyhow::anyhow!("install SIGTERM handler failed: {}", e)
    })?;
  
    let mut sigint = signal(SignalKind::interrupt()).map_err(|e| {
        anyhow::anyhow!("install SIGINT handler failed: {}", e)
    })?;

    Ok(async move {
        tokio::select! {
            _ = sigterm.recv() => info!("SIGTERM received; shutting down"),
            _ = sigint.recv() => info!("SIGINT received; shutting down"),
        }
    })
}

/// Verifies the pool pod is actually able to launch sandboxes. Run once
/// at startup; result drives the gRPC health-check that kubelet's
/// readiness probe consults. Re-runs on resource regression would be
/// nice; defer.
fn preflight_checks(args: &Args) -> anyhow::Result<()> {
    use anyhow::Context as _;

    // `/sandbox-image` must look like a rootfs: needs `usr/`.
    let usr = args.image_dir.join("usr");
    anyhow::ensure!(
        usr.exists(),
        "image volume not ready: {} missing",
        usr.display()
    );

    // State dir must be writable.
    std::fs::create_dir_all(&args.state_dir)
        .with_context(|| format!("mkdir state_dir {}", args.state_dir.display()))?;
    let probe = args.state_dir.join(".health-probe");
    std::fs::write(&probe, b"")
        .with_context(|| format!("write probe file in state_dir {}", args.state_dir.display()))?;
    let _ = std::fs::remove_file(&probe);

    // bwrap installed.
    let status = std::process::Command::new("bwrap")
        .arg("--version")
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status()
        .context("invoke bwrap --version")?;
    anyhow::ensure!(status.success(), "bwrap --version exited non-zero");

    // Worker binary must exist at the configured path.
    anyhow::ensure!(
        args.worker_bin.exists(),
        "worker binary missing at {}",
        args.worker_bin.display()
    );

    Ok(())
}
