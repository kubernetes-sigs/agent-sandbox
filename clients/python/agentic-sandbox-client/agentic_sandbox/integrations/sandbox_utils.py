from functools import wraps

from agentic_sandbox import SandboxClient


class SandboxSettings:
    """
    A container class that stores all settings required for a creation of a particular agent sandbox.

    Its constructor signature is identical to 'agentic_sandbox.SandboxClient'.
    """

    def __init__(
        self,
        template_name: str,
        namespace: str = "default",
        gateway_name: str | None = None,
        gateway_namespace: str = "default",
        api_url: str | None = None,
        server_port: int = 8888,
        sandbox_ready_timeout: int = 180,
        gateway_ready_timeout: int = 180,
        port_forward_ready_timeout: int = 30,
    ):
        self._template_name = template_name
        self._namespace = namespace
        self._gateway_name = gateway_name
        self._gateway_namespace = gateway_namespace
        self._api_url = api_url  # If provided, we skip discovery
        self._server_port = server_port
        self._sandbox_ready_timeout = sandbox_ready_timeout
        self._gateway_ready_timeout = gateway_ready_timeout
        self._port_forward_ready_timeout = port_forward_ready_timeout

    def create_client(self) -> SandboxClient:
        """Creates an instance of the 'SandboxClient' class"""

        return SandboxClient(
            self._template_name,
            namespace=self._namespace,
            gateway_name=self._gateway_name,
            gateway_namespace=self._gateway_namespace,
            api_url=self._api_url,
            server_port=self._server_port,
            sandbox_ready_timeout=self._sandbox_ready_timeout,
            gateway_ready_timeout=self._gateway_ready_timeout,
            port_forward_ready_timeout=self._port_forward_ready_timeout,
        )


def sandbox_in_kwargs(sandbox_settings: SandboxSettings):
    """
    Decorator that injects an instance of the 'SandboxSettings' class as a keyword argument with name 'sandbox',
    so the original function can use it to start interacting with Agent Sandbox.

    Args:
        sandbox_settings: Sandbox settings to be passed to the original function inside the 'sandbox' keyword argument
    """

    def _create_wrapper(func):

        @wraps(func)
        def _wrapper(*args, **kwargs):

            updated_kwargs = kwargs.copy()
            updated_kwargs["sandbox"] = sandbox_settings
            return func(*args, **updated_kwargs)

        return _wrapper

    return _create_wrapper
