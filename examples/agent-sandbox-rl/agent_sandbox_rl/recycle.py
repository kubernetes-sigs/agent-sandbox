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

import asyncio
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
  **tripwires** (drift → quarantine, since this tier does not restore them). Both
  default **off**: they cost a per-reset `pip freeze` / config scan that dominates
  reset time and is Python/git-specific, while env/config drift is rare and already
  bounded by ``max_reuses`` + the determinism canary. Turn ``check_env`` on for
  workloads whose agents mutate site-packages (and accept the cost). ``testbed`` is
  the editable-installed repo dir whose realpath must stay stable. Each phase
  (prime / reset) is one ``exec`` — routed through a persistent `SandboxSession`
  when the handle has one, so resets don't re-connect per command.
  """

  def __init__(self, testbed: str = "/testbed", *, check_env: bool = False,
               check_config: bool = False, wipe=_DEFAULT_WIPE,
               kill_new_procs: bool = True):
    self.testbed = testbed
    self.check_env = check_env
    self.check_config = check_config
    self.wipe = tuple(wipe)
    self.kill_new_procs = kill_new_procs

  # -- shared shell fragments --------------------------------------------- #
  def _fingerprint_sh(self) -> str:
    tb = self.testbed
    # KEY=VALUE lines, parsed by _parse. Only compute what we actually verify —
    # `pip freeze` (the env tripwire) is the expensive part, so it's skipped
    # unless check_env is on (git-only reset is the fast default path).
    parts = [
        f'echo "PRISTINE=$(git -C {tb} rev-parse pristine 2>/dev/null)"',
        f'echo "HEAD=$(git -C {tb} rev-parse HEAD 2>/dev/null)"',
        f'echo "DIRTY=$(git -C {tb} status --porcelain 2>/dev/null | wc -l | tr -d " ")"',
    ]
    if self.check_env:
      parts.append('echo "ENV=$(pip freeze 2>/dev/null | sha256sum | cut -d" " -f1)"')
    if self.check_config:
      # config + hook *content* (names + bodies), mtime-free and order-stable —
      # `ls -la` would hash mtimes and false-positive on every reset.
      parts.append(
          f'echo "CFG=$( (git -C {tb} config --list --local 2>/dev/null | sort; '
          f'for f in {tb}/.git/hooks/*; do [ -f "$f" ] && '
          f'echo "H:$(basename "$f")" && cat "$f"; done 2>/dev/null) '
          f'| sha256sum | cut -d" " -f1)"')
    if self.kill_new_procs:
      parts.append('echo "PROCS=$(ps -eo pid= 2>/dev/null | wc -l | tr -d " ")"')
    return "; ".join(parts)

  @staticmethod
  def _parse(out: str) -> dict:
    d = {}
    for line in out.splitlines():
      if "=" in line:
        k, _, v = line.partition("=")
        d[k.strip()] = v.strip()
    return d

  @staticmethod
  def recyclable(baseline: ResetBaseline) -> bool:
    """Can this sandbox be git-restored? False when `/testbed` has no git repo
    (or `git` is absent / `git tag` failed) — the caller should then process the
    image's tasks with fresh claims instead of quarantining every reset."""
    return bool(baseline.pristine_sha)

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
    try:
      d = self._parse(handle.exec(["bash", "-lc", script]))
    except Exception as e:  # noqa: BLE001 — dead pod / no bash → not recyclable
      logger.warning("prime failed on %s (%s); image treated as non-recyclable",
                     getattr(handle, "pod_name", "?"), e)
      return ResetBaseline()          # empty → recyclable() False → fresh path
    return ResetBaseline(
        # Only trust the `pristine` tag we set; do NOT fall back to HEAD. If
        # `git tag -f pristine` failed, PRISTINE is empty → recyclable() is False
        # → fresh-claim path (never reuse without a verifiable pristine anchor).
        pristine_sha=d.get("PRISTINE", ""),
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
    try:
      d = self._parse(handle.exec(["bash", "-lc", script]))
    except Exception as e:  # noqa: BLE001 — exec failed (dead pod / no bash):
      # never raise into the batch; report dirty so the caller quarantines.
      return ResetOutcome(clean=False, seconds=round(time.monotonic() - t0, 3),
                          reason="exec_error", detail={"error": str(e)})
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
                              reset_timeout: float = 5.0,
                              use_session: bool = True,
                              scale_on_hold: bool = True):
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

  **Non-git images are handled transparently:** if an image has no git repo at
  ``/testbed`` (no pristine anchor at prime), it can't be git-restored, so that
  image's tasks fall back to a **fresh claim per task** (no reset attempts, no
  quarantine churn) — safe to point this at a mixed image set. A reset whose
  ``exec`` fails (dead pod / no bash) is reported dirty and quarantined, never
  raised into the batch.

  **``scale_on_hold`` (default True):** once a recyclable sandbox is claimed and
  held for its whole group, its warm pool is dropped (`unwarm_image`) so the
  controller does not **replenish** a replacement the reuse never claims. This
  removes the *sustained* idle-warm footprint (steady state ~1× the held count
  instead of ~2×) and the ongoing claim-driven replenishment that feeds the
  warm-pool over-creation bug (#1215). Note the *peak* during the initial
  concurrent-claim burst can still transiently reach ~2× (each claim briefly
  replenishes before its `unwarm` lands); stage the claims to cut that too. A
  quarantine/rotation re-claim JIT re-warms the pool first. Verified safe: a
  claimed sandbox survives its pool being scaled/deleted (a `SandboxClaim`
  detaches it from warm-pool ownership). Non-recyclable (fresh-claim-per-task)
  images keep their pools. Set False to keep pools resident (old behavior).
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
    except Exception as e:                    # noqa: BLE001 — capture (let KeyboardInterrupt/SystemExit propagate)
      status = "error"
      results[i] = e
      logger.error("task %s failed: %s", t.id, e)
    finally:
      fleet._obs.task_done(handle.cluster_name, fam, status, time.monotonic() - t0)

  def _run_group(group):
    handle = None
    baseline = None
    uses = 0
    recyclable = None                         # None = undetermined; decided at 1st prime
    try:
      for pos, (i, t) in enumerate(group):
        if handle is None:                    # first task, or after release
          if scale_on_hold and recyclable:    # re-claim: pool was dropped, JIT re-warm
            fleet.warm_image(t.image, replicas_override=1, wait=True)
          handle = fleet.acquire(t)
          if use_session:                     # one held-open exec stream per sandbox
            try:
              handle.open_session()
            except Exception as e:            # noqa: BLE001 — fall back to one-shot exec
              logger.warning("session open failed on %s (%s); one-shot exec",
                             handle.pod_name, e)
          if recyclable is None:              # first claim for this image: prime + decide
            baseline = reset.prime(handle)
            recyclable = reset.recyclable(baseline)
            if not recyclable:
              logger.info("image %s not git-recyclable (%s) — fresh claim per task",
                          t.image, baseline and "no pristine anchor")
          elif recyclable:                    # recyclable image, fresh sandbox after
            baseline = reset.prime(handle)    # rotate/quarantine → re-anchor pristine
          if scale_on_hold and recyclable:    # holding it → stop the pool replenishing
            fleet.unwarm_image(t.image)
          uses = 0
        _process(handle, i, t)
        if pos == len(group) - 1:
          break                               # last task in group: nothing to reset for
        if not recyclable:                    # can't reset → fresh claim, no reset/quarantine
          fleet.release(handle)
          handle = None
          continue
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
    except Exception as e:                    # noqa: BLE001 — infra error (claim/reset/
      # release/warm): capture for this group's unprocessed tasks so one group's
      # failure can't abort the whole batch (KeyboardInterrupt/SystemExit still propagate).
      logger.error("recycle group failed on %s: %s",
                   group[0][1].image if group else "?", e)
      for gi, _gt in group:
        if results[gi] is None:
          results[gi] = e
    finally:
      if handle is not None:
        try:
          fleet.release(handle)
        except Exception:  # noqa: BLE001
          logger.warning("release failed during group cleanup", exc_info=True)

  # Circuit breaker (#1): abort + teardown if live sandboxes run away (the recycle
  # path is exactly where over-creation bit us, so it must be guarded too).
  expected = fleet.plan_.total_replicas if getattr(fleet, "plan_", None) else len(groups)
  with fleet.overcommit_guard(expected):
    if concurrency <= 1:
      for g in groups:
        _run_group(g)
    else:
      with ThreadPoolExecutor(max_workers=concurrency) as ex:
        futs = [ex.submit(_run_group, g) for g in groups]
        for fut in as_completed(futs):
          try:
            fut.result()                        # _run_group captures per-group; this is a backstop
          except Exception:  # noqa: BLE001 — never let one group abort the batch
            logger.error("recycle group raised past its own handler", exc_info=True)
  return results


async def reuse_git_restore_sandbox_async(afleet, tasks, process_fn, concurrency, *,
                                          reset: GitRestoreReset | None = None,
                                          max_reuses: int = 32,
                                          reset_timeout: float = 5.0,
                                          use_session: bool = True,
                                          scale_on_hold: bool = True,
                                          shards_per_image: int = 1,
                                          claim_concurrency: int = 0):
  """Async twin of `reuse_git_restore_sandbox` for `AsyncSandboxFleet`.

  Same semantics — reset-and-reuse a held sandbox, quarantine/rotate, scale_on_hold
  — but each group is an **asyncio coroutine** rather than a thread, so it holds far
  more concurrent sandboxes than OS threads allow (blocking git/exec/claim calls are
  offloaded to the fleet's bounded pool). Effective *exec* concurrency is still
  bounded by that pool + the apiserver exec path — coroutines lift the driver-side
  ceiling, not the exec-plane one.

  **``shards_per_image`` (default 1):** run K sandboxes **per image** in parallel
  (each shard recycles a slice of that image's tasks) — how you saturate a pool from
  a small image set. Requires the image's warm pool to have K replicas (warm with
  ``warm_per_task`` + ``max_warmpool_size=K``). With ``scale_on_hold``, the pool is
  ref-counted to ``desired = active_shards − held`` so ``held + warm == active`` per
  image throughout: K claims never trigger the replenishment storm, and no dangling
  warm is left when shards finish. ``process_fn`` may be sync or async; results are
  returned in ``tasks`` order (per-task exceptions captured, not raised)."""
  reset = reset or GitRestoreReset()
  results: list = [None] * len(tasks)
  by_image: dict[str, list[tuple[int, Task]]] = {}
  for i, t in enumerate(tasks):
    by_image.setdefault(t.image, []).append((i, t))
  K = max(1, shards_per_image)
  groups: list[tuple[str, list]] = []           # (image, [(idx, task), …])
  for img, items in by_image.items():
    if K <= 1:
      groups.append((img, items))
    else:                                       # round-robin split → K balanced shards
      for s in range(K):
        shard = items[s::K]
        if shard:
          groups.append((img, shard))
  sync = afleet._fleet                          # the wrapped SandboxFleet
  obs = sync._obs
  sem = asyncio.Semaphore(max(1, concurrency))
  # Staged claims (E11.4): bound how many groups CLAIM (+ scale-down patch) at once,
  # so reaching a large held count doesn't fire a simultaneous claim burst that
  # 429-saturates the apiserver. 0 = unlimited (all groups claim at once). Groups
  # still *hold* up to `concurrency`; only the acquire+prime+scale phase is throttled.
  claim_sem = asyncio.Semaphore(
      claim_concurrency if (claim_concurrency and claim_concurrency > 0)
      else max(1, len(groups)))
  # per-image ref-count for scale_on_hold: desired replicas = active − held
  active: dict[str, int] = {}
  for img, _g in groups:
    active[img] = active.get(img, 0) + 1
  held: dict[str, int] = {img: 0 for img in active}
  locks: dict[str, asyncio.Lock] = {img: asyncio.Lock() for img in active}

  async def _rescale(img):                       # hold locks[img] around this
    await afleet._to_thread(sync.set_pool_replicas, img, active[img] - held[img])

  async def _process(handle, i, t):
    fam = repo_family(t)
    t0 = time.monotonic()
    status = "ok"
    try:
      with obs.phase("process", cluster=handle.cluster_name, family=fam):
        results[i] = await afleet._call(process_fn, t, handle)
    except Exception as e:                       # noqa: BLE001 — capture (let KeyboardInterrupt/SystemExit propagate)
      status = "error"
      results[i] = e
      logger.error("task %s failed: %s", t.id, e)
    finally:
      obs.task_done(handle.cluster_name, fam, status, time.monotonic() - t0)

  async def _run_group(img, group):
    async with sem:
      handle = None
      baseline = None
      uses = 0
      recyclable = None
      try:
        for pos, (i, t) in enumerate(group):
          if handle is None:
            async with claim_sem:                # staged claims: bound in-flight acquires
              handle = await afleet.acquire(t)
              if use_session:
                try:
                  await afleet._to_thread(handle.open_session)
                except Exception as e:           # noqa: BLE001 — fall back to one-shot exec
                  logger.warning("session open failed on %s (%s); one-shot exec",
                                 handle.pod_name, e)
              if recyclable is None:             # first claim: prime + decide
                baseline = await afleet._to_thread(reset.prime, handle)
                recyclable = reset.recyclable(baseline)
                if not recyclable:
                  logger.info("image %s not git-recyclable — fresh claim per task", t.image)
              elif recyclable:
                baseline = await afleet._to_thread(reset.prime, handle)
              if scale_on_hold and recyclable:   # holding → cancel this claim's replenish
                async with locks[img]:
                  held[img] += 1
                  await _rescale(img)
            uses = 0
          await _process(handle, i, t)
          if pos == len(group) - 1:
            break
          if not recyclable:                      # can't reset → fresh claim per task
            await afleet.release(handle); handle = None; continue
          uses += 1
          if uses >= max_reuses:                  # planned rotation (drift bound)
            with obs.phase("rotate", cluster=handle.cluster_name):
              await afleet.release(handle)
            if scale_on_hold and recyclable:      # released → make a warm for re-claim
              async with locks[img]:
                held[img] -= 1
                await _rescale(img)
            handle = None; continue
          with obs.phase("reset", cluster=handle.cluster_name):
            outcome = await afleet._to_thread(reset.reset, handle, baseline,
                                              timeout=reset_timeout)
          if not outcome.clean:                   # dirty → quarantine, fresh claim next
            logger.warning("quarantine sandbox %s after reset: %s",
                           handle.pod_name, outcome.reason)
            with obs.phase("quarantine", cluster=handle.cluster_name):
              await afleet.release(handle)
            if scale_on_hold and recyclable:
              async with locks[img]:
                held[img] -= 1
                await _rescale(img)
            handle = None
      except Exception as e:                      # noqa: BLE001 — infra error: capture for
        # this group's unprocessed tasks so it can't abort the whole gather
        # (KeyboardInterrupt/SystemExit still propagate).
        logger.error("recycle group failed on %s: %s", img, e)
        for gi, _gt in group:
          if results[gi] is None:
            results[gi] = e
      finally:
        holding = handle is not None
        if holding:
          try:
            await afleet.release(handle)
          except Exception:  # noqa: BLE001
            logger.warning("release failed during group cleanup", exc_info=True)
        if scale_on_hold and recyclable:          # shard done: active−1 (+ held−1 if it
          async with locks[img]:                  # was holding) leaves desired unchanged
            if holding:                           # → no dangling warm at end of run
              held[img] -= 1
            active[img] -= 1
            await _rescale(img)

  # Circuit breaker (#1) around the whole fan-out — the sync guard's monitor thread
  # runs independently of the loop; on trip it tears down (failing in-flight claims,
  # captured per task) and raises FleetOvercommitError here.
  expected = (afleet._fleet.plan_.total_replicas
              if afleet._fleet.plan_ else len(groups))
  with afleet._fleet.overcommit_guard(expected):
    # return_exceptions=True is a backstop — _run_group already captures per-group,
    # so one group's infra error can't discard the whole batch's results.
    await asyncio.gather(*(_run_group(img, g) for img, g in groups),
                         return_exceptions=True)
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
