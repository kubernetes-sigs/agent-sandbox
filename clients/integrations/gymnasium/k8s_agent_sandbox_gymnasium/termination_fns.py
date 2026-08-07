from .termination_fn import TerminationFn


class SparseTaskTermination(TerminationFn):
    """
    Returns True if obs["reward"] == 1 and False otherwise

    Example:
        termination_fn = SparseTaskTermination()
    """
    def __init__(self, success_fn):
        self.success_fn = success_fn

    def __call__(self, obs, info, task) -> bool:
        return self.success_fn(obs, info)
