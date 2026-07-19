# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Sandbox recycling — reset-and-reuse a claimed sandbox across same-image tasks.

For G rollouts on one image, claiming→deleting→re-claiming per rollout makes claims
scale with *tasks*; recycling makes them scale with *problems* (÷G). The crux is the
**pollution problem**: a reused sandbox must look byte-identical to a fresh one, or it
silently biases RL rewards. See `plans/sandbox-recycling.md` for the full design and
the v2 deep-research findings this implements.

This module ships the **git-restore** reset tier (the cheapest safe mechanism):
restore the `/testbed` working tree to a pristine tag, sweep processes + `/tmp`, then
**verify** cleanliness — repo clean at the pristine SHA, and (tripwires, not fixes) the
Python env, git config, and hooks unchanged. Drift in a surface we don't actively
restore → the reset returns False → the executor **quarantines** the sandbox (releases
it and claims a fresh one). It deliberately does NOT restore the conda/site-packages
env or overlay-restart (the costlier tiers): those are detected and escalated to a
fresh claim instead. `determinism_canary` is the ground-truth check that a reset
actually produced identical outputs.
"""

from __future__ import annotations

import logging
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field

from .handles import SandboxHandle
from .observability import repo_family
from .sources import Task

logger = logging.getLogger("agent_sandbox_rl.recycle")

_DEFAULT_WIPE = ("/tmp/*", "/var/tmp/*", "$HOME/.cache/*")


@dataclass
class ResetBaseline:
  """Pristine fingerprint captured at first claim; compared on every reset."""

  pristine_sha: str = ""
  env_hash: str = ""
  config_hash: str = ""
  proc_count: int = 0


@dataclass
class ResetOutcome:
  """Result of one reset attempt. ``clean`` False → caller must quarantine."""

  clean: bool
  seconds: float
  reason: str = ""            # first failing probe (empty when clean)
  detail: dict = field(default_factory=dict)


class GitRestoreReset:
  """Restore a sandbox to its pristine baseline via git + a cleanliness verify.

  ``check_env`` / ``check_config`` toggle the site-packages and git-config/hooks
  **tripwires** (drift → quarantine, since this tier does not restore them).
  ``testbed`` is the editable-installed repo dir whose realpath must stay stable.
  All work is one ``exec`` round-trip per phase (prime / reset) to keep the reset
  off the critical path budget.
  """

  def __init__(self, testbed: str = "/testbed", *, check_env: bool = True,
               check_config: bool = True, wipe=_DEFAULT_WIPE,
               kill_new_procs: bool = True):
    self.testbed = testbed
    self.check_env = check_env
    self.check_config = check_config
    self.wipe = tuple(wipe)
    self.kill_new_procs = kill_new_procs

  # -- shared shell fragments --------------------------------------------- #
  def _fingerprint_sh(self) -> str:
    tb = self.testbed
    # KEY=VALUE lines, parsed by _parse. pip freeze is the site-packages tripwire;
    # config+hooks listing is the booby-trap tripwire (v2: agent `git config`
    # writes land in shared .git/config even with worktreeConfig).
    return (
        f'echo "PRISTINE=$(git -C {tb} rev-parse pristine 2>/dev/null)"; '
        f'echo "HEAD=$(git -C {tb} rev-parse HEAD 2>/dev/null)"; '
        f'echo "DIRTY=$(git -C {tb} status --porcelain 2>/dev/null | wc -l | tr -d " ")"; '
        f'echo "ENV=$(pip freeze 2>/dev/null | sha256sum | cut -d" " -f1)"; '
        f'echo "CFG=$( (git -C {tb} config --list; ls -la {tb}/.git/hooks 2>/dev/null) '
        f'| sha256sum | cut -d" " -f1)"; '
        f'echo "PROCS=$(ps -eo pid= 2>/dev/null | wc -l | tr -d " ")"')

  @staticmethod
  def _parse(out: str) -> dict:
    d = {}
    for line in out.splitlines():
      if "=" in line:
        k, _, v = line.partition("=")
        d[k.strip()] = v.strip()
    return d

  # -- prime: once per sandbox at first claim ----------------------------- #
  def prime(self, handle: SandboxHandle) -> ResetBaseline:
    tb = self.testbed
    script = (
        "set +e; "
        # anchor a pristine ref that survives agent branching, and kill all
        # implicit maintenance so the (shared, for worktrees) object store is
        # never gc'd concurrently — v2 findings.
        f'git -C {tb} tag -f pristine >/dev/null 2>&1; '
        f'git -C {tb} config gc.auto 0; '
        f'git -C {tb} config maintenance.auto false; '
        f'git -C {tb} config gc.pruneExpire never; '
        f'git -C {tb} checkout -q --detach pristine >/dev/null 2>&1; '
        + self._fingerprint_sh())
    d = self._parse(handle.exec(["bash", "-lc", script]))
    return ResetBaseline(
        pristine_sha=d.get("PRISTINE", "") or d.get("HEAD", ""),
        env_hash=d.get("ENV", ""),
        config_hash=d.get("CFG", ""),
        proc_count=int(d.get("PROCS", "0") or 0))

  # -- reset: between rollouts -------------------------------------------- #
  def reset(self, handle: SandboxHandle, baseline: ResetBaseline,
            *, timeout: float = 5.0) -> ResetOutcome:
    tb = self.testbed
    wipe = " ".join(self.wipe)
    kill = ""
    if self.kill_new_procs:
      # Clean-slate: kill every process except init (pid1 = the keepalive) and
      # our own reset shell/parent. Not a baseline diff — SWE-bench images run
      # nothing but pid1 + exec transients, so a full sweep is the safe reset;
      # for images with legitimate long-lived daemons, set kill_new_procs=False.
      # Process hygiene across k8s exec is unverified (research sub-Q4); the PROCS
      # tripwire below is the backstop.
      kill = (
          'for p in $(ps -eo pid= 2>/dev/null | tr -d " "); do '
          '[ "$p" = "1" ] || [ "$p" = "$$" ] || [ "$p" = "$PPID" ] || '
          'kill -9 "$p" 2>/dev/null; done; ')
    script = (
        "set +e; "
        + kill
        + f'git -C {tb} reset -q --hard pristine >/dev/null 2>&1; '
        + f'git -C {tb} clean -qxdff >/dev/null 2>&1; '
        + f'git -C {tb} checkout -q --detach pristine >/dev/null 2>&1; '
        + f'rm -rf {wipe} >/dev/null 2>&1; '
        + self._fingerprint_sh())
    t0 = time.monotonic()
    d = self._parse(handle.exec(["bash", "-lc", script]))
    elapsed = time.monotonic() - t0

    reason = self._first_failure(d, baseline, elapsed, timeout)
    return ResetOutcome(clean=(reason == ""), seconds=round(elapsed, 3),
                        reason=reason, detail=d)

  def _first_failure(self, d: dict, base: ResetBaseline, elapsed: float,
                     timeout: float) -> str:
    # No pristine anchor (non-git /testbed, or `git tag` failed at prime) means
    # the git-restore reset is a no-op we cannot verify — never claim clean.
    if not base.pristine_sha:
      return "no_pristine_anchor"
    if elapsed > timeout:
      return "timeout"                       # advisory: measured, not kill-enforced
    if d.get("HEAD", "") != base.pristine_sha:
      return "head_mismatch"
    if (d.get("DIRTY", "0") or "0") != "0":
      return "worktree_dirty"
    if self.check_env and base.env_hash and d.get("ENV", "") != base.env_hash:
      return "env_drift"                     # site-packages tripwire (the big hole)
    if self.check_config and base.config_hash and d.get("CFG", "") != base.config_hash:
      return "config_or_hooks_drift"         # booby-trap tripwire
    # process-count backstop: allow a little slack for transient exec children
    try:
      if base.proc_count and int(d.get("PROCS", "0")) > base.proc_count + 2:
        return "process_leak"
    except ValueError:
      pass
    return ""


def reuse_git_restore_sandbox(fleet, tasks, process_fn, concurrency, *,
                              reset: GitRestoreReset | None = None,
                              max_reuses: int = 32,
                              reset_timeout: float = 5.0):
  """Execute ``tasks`` reusing one sandbox per image (claims scale ÷ tasks-per-image).

  Tasks are grouped by image; each group runs sequentially inside a single claimed
  sandbox, git-restore-reset between rollouts. Groups run in parallel up to
  ``concurrency`` (= the concurrent-sandbox budget). ``max_reuses`` bounds how many
  tasks one sandbox serves before a **rotation** (release + fresh claim) to cap drift.
  A reset that fails cleanliness verification or exceeds ``reset_timeout`` triggers a
  **quarantine** (also release + fresh claim). ``reset_timeout`` is advisory — it flags
  a *slow* reset after the fact; it does not interrupt a *hung* ``exec`` (that blocks
  the group's worker thread). Returns one result per task in ``tasks`` order (a per-task
  exception is captured, not raised) — matching ``strategies.process_parallel``.

  Records ``reset`` / ``quarantine`` / ``rotate`` phases in the RunReport so the
  claim-economics win is measurable next to the baseline strategies.
  """
  reset = reset or GitRestoreReset()
  results = [None] * len(tasks)
  by_image: dict[str, list[tuple[int, Task]]] = {}
  for i, t in enumerate(tasks):
    by_image.setdefault(t.image, []).append((i, t))
  groups = list(by_image.values())

  def _process(handle, i, t):
    fam = repo_family(t)
    t0 = time.monotonic()
    status = "ok"
    try:
      with fleet._obs.phase("process", cluster=handle.cluster_name, family=fam):
        results[i] = process_fn(t, handle)
    except BaseException as e:                # noqa: BLE001 — capture, keep batch alive
      status = "error"
      results[i] = e
      logger.error("task %s failed: %s", t.id, e)
    finally:
      fleet._obs.task_done(handle.cluster_name, fam, status, time.monotonic() - t0)

  def _run_group(group):
    handle = None
    baseline = None
    uses = 0
    try:
      for pos, (i, t) in enumerate(group):
        if handle is None:                    # first task, or after a quarantine
          handle = fleet.acquire(t)
          baseline = reset.prime(handle)
          uses = 0
        _process(handle, i, t)
        if pos == len(group) - 1:
          break                               # nothing to reset for
        uses += 1
        if uses >= max_reuses:                # planned rotation (drift bound)
          with fleet._obs.phase("rotate", cluster=handle.cluster_name):
            fleet.release(handle)
          handle = None
          continue
        with fleet._obs.phase("reset", cluster=handle.cluster_name):
          outcome = reset.reset(handle, baseline, timeout=reset_timeout)
        if not outcome.clean:                 # dirty → quarantine, fresh claim next
          logger.warning("quarantine sandbox %s after reset: %s",
                         handle.pod_name, outcome.reason)
          with fleet._obs.phase("quarantine", cluster=handle.cluster_name):
            fleet.release(handle)
          handle = None
    finally:
      if handle is not None:
        fleet.release(handle)

  if concurrency <= 1:
    for g in groups:
      _run_group(g)
    return results
  with ThreadPoolExecutor(max_workers=concurrency) as ex:
    futs = [ex.submit(_run_group, g) for g in groups]
    for fut in as_completed(futs):
      fut.result()                            # surface executor-level errors
  return results


def determinism_canary(fleet, task, process_fn, *,
                       reset: GitRestoreReset | None = None):
  """Run ``task`` twice in ONE recycled sandbox with a reset between; report whether
  the two outputs are byte-identical (any difference == contamination by definition).

  This is the ground-truth verification gate from the design doc — run it before
  trusting recycling for training. Returns a dict with ``identical`` / ``reset_clean``
  / the two results / the reset outcome. Releases the sandbox on exit.
  """
  reset = reset or GitRestoreReset()
  handle = fleet.acquire(task)
  try:
    baseline = reset.prime(handle)
    first = process_fn(task, handle)
    outcome = reset.reset(handle, baseline)
    second = process_fn(task, handle)
    return {
        "identical": first == second,
        "reset_clean": outcome.clean,
        "reset_reason": outcome.reason,
        "reset_seconds": outcome.seconds,
        "first": first,
        "second": second,
    }
  finally:
    fleet.release(handle)
