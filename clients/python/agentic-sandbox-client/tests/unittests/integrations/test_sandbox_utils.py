from agentic_sandbox.integrations.sandbox_utils import sandbox_in_kwargs

from agentic_sandbox.integrations.sandbox_utils import SandboxSettings


def test_sandbox_in_kwargs_decorator():
    settings = SandboxSettings(
        template_name="some template",
        namespace="some namespace",
    )

    def func(**kwargs):
        return kwargs

    func_with_sandbox = sandbox_in_kwargs(settings)(func)
    func_kwargs = func_with_sandbox()

    assert "sandbox" in func_kwargs
    assert func_kwargs["sandbox"] is settings
