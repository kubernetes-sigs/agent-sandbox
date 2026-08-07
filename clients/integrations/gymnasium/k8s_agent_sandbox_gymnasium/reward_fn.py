from abc import ABC, abstractmethod


class RewardFn(ABC):
    """
    Base class for reward functions. Implement __call__ to define
    your reward logic. Override reset() for stateful rewards.
    """

    def reset(self, task: str) -> None:
        """Called at the start of every episode. Override for stateful rewards."""
        pass

    @abstractmethod
    def __call__(
        self,
        action: str,   # the shell command / code the agent ran
        obs: str,      # stdout or stderr returned by the sandbox
        info: dict,    # exit_code, stderr, elapsed_ms, ...
        task: str,     # the current task description
    ) -> float:
        ...
