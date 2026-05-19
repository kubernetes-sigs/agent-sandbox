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

//! Bubblewrap tenant launcher. Minimal MVP: overlayfs + bwrap spawn. No
//! per-tenant network namespace, cgroup, landlock, or seccomp yet — the
//! pool pod's own kernel namespaces and capability set provide first-line
//! isolation and we can layer on per-tenant constraints incrementally.
//!
//! Layout per tenant (uid = Sandbox CR UID):
//!   * `{state_dir}/{uid}/upper`         — overlayfs upper (PVC; persisted)
//!   * `{state_dir}/{uid}/work`          — overlayfs work
//!   * `/run/sandboxes/{uid}/merged`     — overlayfs merged (tmpfs)
//!   * `/run/sandboxes/{uid}/worker.sock` — IPC socket (reserved; not wired)
//!
//! Stable naming so kernel/FS alone is enough to rebuild registry state
//! after pod-agent restart.

use std::ffi::OsString;
use std::os::unix::process::ExitStatusExt;
use std::path::{Path, PathBuf};
use std::process::Stdio;

use anyhow::{anyhow, Context};
use nix::mount::{mount, umount2, MntFlags, MsFlags};
use nix::sys::signal::{kill, Signal};
use nix::unistd::Pid;
use tokio::process::{Child, Command};
use tracing::{info, warn};

/// Filesystem layout for a single sandbox.
///
/// One per-sandbox tmpfs root on the host (`sandbox_root`) with three
/// well-defined subdirs. The split exists so we can bind **only what
/// the worker needs** into the (untrusted) sandbox mount namespace —
/// keeping logs and overlay machinery out of the tenant's view:
///
/// ```text
/// /run/sandboxes/<uid>/                ← sandbox_root  (host tmpfs, ephemeral)
///   ├── merged/                        ← bind-mounted into sandbox as `/`
///   ├── run/                           ← bind-mounted into sandbox as
///   │     └── worker.sock                `/run/agentsandbox/`
///   └── logs/                          ← host-only (NOT bound in)
///         ├── bwrap.stdout.log
///         └── bwrap.stderr.log
/// ```
///
/// Earlier layout binding all of `sandbox_root` into the sandbox caused an
/// `/` ↔ `/run/agentsandbox/merged` filesystem loop and also leaked
/// pod-agent's debug logs into the tenant.
#[derive(Clone, Debug)]
pub struct SandboxPaths {
    pub uid: String,
    /// Persistent overlay state on the PVC: `upper/` and `work/`.
    pub state_root: PathBuf,
    /// Host-side per-sandbox tmpfs root (`/run/sandboxes/<uid>`).
    pub sandbox_root: PathBuf,
}

impl SandboxPaths {
    pub fn for_uid(state_dir: &Path, uid: &str) -> Self {
        Self {
            uid: uid.to_string(),
            state_root: state_dir.join(uid),
            sandbox_root: PathBuf::from("/run/sandboxes").join(uid),
        }
    }

    pub fn overlay_upper(&self) -> PathBuf {
        self.state_root.join("upper")
    }
    pub fn overlay_work(&self) -> PathBuf {
        self.state_root.join("work")
    }
    /// Overlayfs merged mountpoint. Bind-mounted into the sandbox as `/`.
    pub fn overlay_merged(&self) -> PathBuf {
        self.sandbox_root.join("merged")
    }
    /// Subdir bind-mounted into the sandbox at [`WORKER_RUN_DIR_IN_SANDBOX`].
    /// Holds nothing on the host except the worker socket created at runtime.
    pub fn worker_run_dir(&self) -> PathBuf {
        self.sandbox_root.join("run")
    }
    /// Path to the worker's Unix socket on the host. The same inode is
    /// visible inside the sandbox at [`WORKER_SOCKET_IN_SANDBOX`].
    #[allow(dead_code)]
    pub fn worker_socket(&self) -> PathBuf {
        self.worker_run_dir().join("worker.sock")
    }
    /// Host-only directory for bwrap's captured stdout/stderr. Never
    /// bind-mounted into the sandbox — debug logs stay out of the tenant.
    pub fn logs_dir(&self) -> PathBuf {
        self.sandbox_root.join("logs")
    }
}

/// A launched tenant. Holding this value keeps the child process tracked;
/// dropping it does not stop the process — call [`Sandbox::stop`] for that.
pub struct Sandbox {
    pub fs: SandboxFs,
    pub child: Child,
}

/// Owns the host-side filesystem state of a sandbox and tears it down
/// **in the only order that's safe** via a single explicit `Drop` impl.
///
/// The dependency between the two pieces of state is encoded by
/// ownership: `SandboxFs` owns the optional [`OverlayMount`], and its
/// `Drop` runs `(1) unmount` then `(2) wipe sandbox_root` in one function
/// body. Doing them in the opposite order would walk the recursive
/// delete INTO the still-mounted overlay and apply deletions to the
/// upper layer on the PVC — corrupting persistent state.
///
/// Refactors elsewhere can't break the ordering: there is no struct
/// field declaration order to reshuffle, no parallel guards racing
/// each other. Anyone who wants to change the sequence has to edit
/// `SandboxFs::drop` directly, which is a deliberate, reviewable change.
pub struct SandboxFs {
    pub paths: SandboxPaths,
    /// Present iff this `Sandbox` is responsible for unmounting the
    /// overlayfs (i.e. `OverlayMount::new` did the mount). When we
    /// re-attached to a mount that was already there (boot recovery),
    /// this is `None` and the umount step is a no-op.
    overlay: Option<OverlayMount>,
}

impl Drop for SandboxFs {
    fn drop(&mut self) {
        // (1) Unmount the overlay (if we mounted it). `OverlayMount::drop`
        //     unmounts and removes the now-empty merged subdir.
        drop(self.overlay.take());
        // (2) Wipe the rest of sandbox_root (logs, worker_run, ...).
        if let Err(e) = std::fs::remove_dir_all(&self.paths.sandbox_root) {
            tracing::warn!(sandbox_root = %self.paths.sandbox_root.display(), %e,
                           "SandboxFs drop: remove_dir_all failed");
        }
    }
}

/// RAII guard for an overlayfs mount we created. Dropping it unmounts
/// (best-effort, `MNT_DETACH`). The guard is constructed only when we
/// actually performed the mount; see [`OverlayMount::new`].
struct OverlayMount {
    merged: PathBuf,
}

impl OverlayMount {
    /// Mount the overlay if not already mounted. Returns `Some(guard)`
    /// when we did the mount (the guard will unmount on drop) and `None`
    /// when an existing mount was already in place (we won't touch it).
    fn new(image_dir: &Path, paths: &SandboxPaths) -> anyhow::Result<Option<Self>> {
        if is_mountpoint(&paths.overlay_merged())? {
            return Ok(None);
        }
        let mut data = OsString::from("lowerdir=");
        data.push(image_dir);
        data.push(",upperdir=");
        data.push(paths.overlay_upper());
        data.push(",workdir=");
        data.push(paths.overlay_work());
        mount(
            Some("overlay"),
            &paths.overlay_merged(),
            Some("overlay"),
            MsFlags::empty(),
            Some(data.as_os_str()),
        )
        .with_context(|| {
            format!(
                "mount overlay merged={} lower={} upper={}",
                paths.overlay_merged().display(),
                image_dir.display(),
                paths.overlay_upper().display(),
            )
        })?;
        Ok(Some(Self {
            merged: paths.overlay_merged(),
        }))
    }
}

impl Drop for OverlayMount {
    fn drop(&mut self) {
        if let Err(e) = umount2(&self.merged, MntFlags::MNT_DETACH) {
            tracing::warn!(merged = %self.merged.display(), %e,
                           "OverlayMount drop: umount failed");
        }
        // After unmount the bind target is an empty dir; remove it so
        // `/run/sandbox-merged/` doesn't accumulate stale per-sandbox
        // entries across the pool pod's lifetime.
        let _ = std::fs::remove_dir(&self.merged);
    }
}

/// Launch a bubblewrap tenant.
///
/// `image_dir` is the read-only overlay lower (the OCI image volume mount
/// from the pool pod). `workspace_mount_path` is the in-sandbox path where
/// the persistent overlay upper is exposed (default "/home" per the
/// controller). `worker_bin` is the host path of the
/// `agent-sandbox-worker` binary that will become the bwrap payload — it
/// is bind-mounted read-only into the sandbox at [`WORKER_BIN_IN_SANDBOX`]
/// and exec'd there.
pub async fn launch(
    paths: SandboxPaths,
    image_dir: &Path,
    worker_bin: &Path,
    workspace_mount_path: &str,
) -> anyhow::Result<Sandbox> {
    std::fs::create_dir_all(paths.overlay_upper())
        .with_context(|| format!("mkdir upper {}", paths.overlay_upper().display()))?;
    std::fs::create_dir_all(paths.overlay_work())
        .with_context(|| format!("mkdir work {}", paths.overlay_work().display()))?;
    std::fs::create_dir_all(paths.overlay_merged())
        .with_context(|| format!("mkdir merged {}", paths.overlay_merged().display()))?;
    // bwrap bind-mounts this whole directory into the sandbox so the
    // worker can create its socket there and the pod-agent can connect
    // to it from outside. It contains *only* the socket — logs live in
    // a sibling dir that we do NOT bind in.
    std::fs::create_dir_all(paths.worker_run_dir())
        .with_context(|| format!("mkdir worker_run_dir {}", paths.worker_run_dir().display()))?;
    std::fs::create_dir_all(paths.logs_dir())
        .with_context(|| format!("mkdir logs_dir {}", paths.logs_dir().display()))?;

    let overlay = OverlayMount::new(image_dir, &paths)?;
    // Wrap paths + overlay into a SandboxFs immediately. From here on,
    // any early return drops `fs` and runs `SandboxFs::drop`, which
    // does the unmount-then-wipe sequence — same as a normal stop().
    let fs = SandboxFs { paths, overlay };

    let child = spawn_bwrap(&fs.paths, worker_bin, workspace_mount_path)?;
    info!(uid = %fs.paths.uid, pid = child.id().unwrap_or(0), "tenant launched");
    Ok(Sandbox { fs, child })
}

/// Path at which the worker binary appears inside the sandbox.
pub const WORKER_BIN_IN_SANDBOX: &str = "/usr/local/bin/agent-sandbox-worker";
/// Directory inside the sandbox holding the worker's Unix socket.
pub const WORKER_RUN_DIR_IN_SANDBOX: &str = "/run/agentsandbox";
/// Path of the worker's Unix socket inside the sandbox. The host-side
/// view of the same file is [`SandboxPaths::worker_socket`].
pub const WORKER_SOCKET_IN_SANDBOX: &str = "/run/agentsandbox/worker.sock";

impl Sandbox {
    /// Stop the tenant: SIGTERM the bwrap process, wait briefly, then
    /// let the overlayfs unmount happen via `OverlayMount`'s Drop impl
    /// when `self` falls out of scope at the end of this function.
    /// The overlay upper subdirectory on the PVC is intentionally
    /// retained for future resume.
    ///
    /// NB: if a future caller cancels this future mid-execution (for
    /// instance the main runtime shuts down while we're inside
    /// `child.wait()`), `self` still drops normally and `OverlayMount`
    /// still unmounts — so kernel state is cleaned up even on cancellation.
    pub async fn stop(mut self) -> anyhow::Result<()> {
        let uid = self.fs.paths.uid.clone();
        if let Some(pid) = self.child.id() {
            if let Err(e) = kill(Pid::from_raw(pid as i32), Signal::SIGTERM) {
                warn!(uid = %uid, %e, "SIGTERM failed (process may have already exited)");
            }
        }
        match tokio::time::timeout(std::time::Duration::from_secs(5), self.child.wait()).await {
            Ok(Ok(status)) => {
                info!(uid = %uid, code = ?status.code(), signal = ?status.signal(), "tenant exited");
            }
            Ok(Err(e)) => warn!(uid = %uid, %e, "wait failed"),
            Err(_) => {
                warn!(uid = %uid, "tenant did not exit on SIGTERM within 5s; sending SIGKILL");
                let _ = self.child.start_kill();
                let _ = self.child.wait().await;
            }
        }
        // Filesystem cleanup happens via field-drop chain:
        //   1. `overlay` drops — `OverlayMount::drop` unmounts the
        //      overlay and removes the (now-empty) `merged/` subdir.
        //   2. `sandbox_root` drops — `PodRootGuard::drop` recursively
        //      wipes the rest of `sandbox_root` (logs, worker_run, etc.).
        // Doing (2) before (1) would apply the recursive delete
        // through the overlay's upper layer and corrupt PVC state.
        // The struct field declaration order guarantees this; the
        // compiler enforces it.
        Ok(())
    }
}

/// Returns true if `p` exists and resides on a different filesystem from
/// its parent — a cheap heuristic for "this directory has something
/// mounted on top of it". Works for overlayfs (distinct st_dev), but
/// would also return true for any other mount stacked here. Good enough
/// for boot-recovery's "did the previous pod-agent already mount this?".
fn is_mountpoint(p: &Path) -> anyhow::Result<bool> {
    if !p.exists() {
        return Ok(false);
    }
    let parent = match p.parent() {
        Some(p) => p,
        None => return Ok(false),
    };
    let p_meta = std::fs::metadata(p)?;
    let parent_meta = std::fs::metadata(parent)?;
    use std::os::unix::fs::MetadataExt;
    Ok(p_meta.dev() != parent_meta.dev())
}

fn spawn_bwrap(
    paths: &SandboxPaths,
    worker_bin: &Path,
    workspace_mount_path: &str,
) -> anyhow::Result<Child> {
    let merged = paths.overlay_merged();
    let upper = paths.overlay_upper();
    let worker_run = paths.worker_run_dir();
    let logs_dir = paths.logs_dir();

    // Capture bwrap stdout/stderr to per-sandbox files for postmortem.
    // Logs go to a host-only dir; they're never visible inside the
    // sandbox (the worker_run dir is what gets bind-mounted).
    let stdout_log = std::fs::File::create(logs_dir.join("bwrap.stdout.log"))
        .with_context(|| format!("create bwrap.stdout.log under {}", logs_dir.display()))?;
    let stderr_log = std::fs::File::create(logs_dir.join("bwrap.stderr.log"))
        .with_context(|| format!("create bwrap.stderr.log under {}", logs_dir.display()))?;

    let mut cmd = Command::new("bwrap");
    cmd.kill_on_drop(false) // TODO: Should this be true?
        .stdin(Stdio::null())
        .stdout(Stdio::from(stdout_log))
        .stderr(Stdio::from(stderr_log));

    cmd.args([
        "--unshare-pid",
        "--unshare-ipc",
        "--unshare-uts",
        "--unshare-cgroup",
        "--die-with-parent",
        "--cap-drop",
        "ALL",
        "--clearenv",
        "--setenv",
        "HOME",
        workspace_mount_path,
        "--setenv",
        "PATH",
        "/usr/local/bin:/usr/bin:/bin",
        "--setenv",
        "LANG",
        "C.UTF-8",
    ]);

    // Rootfs comes from the overlay merged dir. Bind RW because the
    // overlay itself enforces lower=RO; sandbox writes land in the upper
    // layer. Using --ro-bind here would make even the upper RO, breaking
    // any bind-mount target creation (e.g. the worker binary file).
    cmd.arg("--bind").arg(&merged).arg("/");

    // Standard kernel filesystems.
    cmd.args(["--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp"]);

    // Mask host kernel/info files inside /proc with /dev/null so the
    // sandbox can't pull host kASLR symbols, keyring, kmsg, or — most
    // importantly — `/proc/self/mountinfo`, which would leak our PVC
    // and overlay paths into the tenant.
    for path in [
        "/proc/kallsyms",
        "/proc/keys",
        "/proc/kcore",
        "/proc/kmsg",
        "/proc/self/mountinfo",
    ] {
        cmd.args(["--ro-bind", "/dev/null", path]);
    }

    // Networking: we currently share the pool pod's network namespace
    // (no --unshare-net), so the sandbox has working internet via the
    // pod. But the user image's `/etc/resolv.conf` / `/etc/hosts` /
    // `/etc/nsswitch.conf` are wrong for the pod's resolver — DNS
    // would point nowhere. Bind in the pool pod's own files so name
    // resolution works.
    //
    // --ro-bind-try silently no-ops if the source is missing.
    //
    // Per-tenant isolation (separate netns + nftables + egress proxy
    // a-la moat) is tracked in plan.md.
    cmd.args(["--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf"]);
    cmd.args(["--ro-bind-try", "/etc/hosts", "/etc/hosts"]);
    cmd.args(["--ro-bind-try", "/etc/nsswitch.conf", "/etc/nsswitch.conf"]);

    // Per-tenant writable workspace = overlay upper, exposed at the
    // controller-chosen mount path (default /home).
    cmd.arg("--bind").arg(&upper).arg(workspace_mount_path);

    // Worker binary (read-only) and its Unix socket directory (read-write
    // so the worker can create the socket). The pod-agent connects to the
    // socket from the host side via paths.worker_socket().
    cmd.arg("--ro-bind")
        .arg(worker_bin)
        .arg(WORKER_BIN_IN_SANDBOX);
    cmd.arg("--bind")
        .arg(&worker_run)
        .arg(WORKER_RUN_DIR_IN_SANDBOX);

    cmd.args([WORKER_BIN_IN_SANDBOX, "--socket", WORKER_SOCKET_IN_SANDBOX]);

    cmd.spawn()
        .map_err(|e| anyhow!("spawn bwrap (is bubblewrap installed?): {e}"))
}
