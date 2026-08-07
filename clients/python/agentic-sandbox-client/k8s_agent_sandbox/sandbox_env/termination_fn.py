from abc import ABC, abstractmethod


class TerminationFn(ABC):
    """
    Base class for termination functions. Implement __call__ to define
    your termination logic.
    """

    def reset(self, task: str) -> None:
        pass

    @abstractmethod
    def __call__(
        self,
        obs: str,
        info: dict,
        task: str,
    ) -> bool:
        ...
