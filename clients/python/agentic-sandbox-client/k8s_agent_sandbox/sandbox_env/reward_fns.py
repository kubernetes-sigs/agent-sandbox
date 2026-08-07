from .reward_fn import RewardFn


class ExitCodeReward(RewardFn):
    """
    Simplest reward: +1 for success, -1 for crash.
    Good as a baseline or for debugging the training loop.
    """
    def __call__(self, action, obs, info, task):
        return 1.0 if info["exit_code"] == 0 else -1.0


class SparseTaskReward(RewardFn):
    """
    0.0 at every step; +1.0 only when a user-defined predicate fires.
    Good for wrapping test-suite checks.

    Example:
        reward_fn = SparseTaskReward(
            success_fn=lambda obs, info: "manage.py" in obs and info["exit_code"] == 0
        )
    """
    def __init__(self, success_fn):
        self.success_fn = success_fn

    def __call__(self, action, obs, info, task):
        return 1.0 if self.success_fn(obs, info) else 0.0


class StepPenaltyReward(RewardFn):
    """
    Wraps another reward function and subtracts a small per-step penalty.
    Encourages the agent to solve tasks in fewer steps.

    Example:
        reward_fn = StepPenaltyReward(base=ExitCodeReward(), penalty=0.05)
    """
    def __init__(self, base: RewardFn, penalty: float = 0.01):
        self.base    = base
        self.penalty = penalty

    def reset(self, task):
        self.base.reset(task)

    def __call__(self, action, obs, info, task):
        return self.base(action, obs, info, task) - self.penalty


class FileCreatedReward(RewardFn):
    """
    Stateful reward: gives +1 the first time a target filename appears in output,
    0 afterwards. Useful for multi-step file-creation tasks.

    Example:
        reward_fn = FileCreatedReward(target_file="manage.py")
    """
    def __init__(self, target_file: str):
        self.target_file = target_file
        self._awarded    = False

    def reset(self, task):
        self._awarded = False   # crucial: resets per-episode state

    def __call__(self, action, obs, info, task):
        if not self._awarded and self.target_file in obs:
            self._awarded = True
            return 1.0
        return 0.0
