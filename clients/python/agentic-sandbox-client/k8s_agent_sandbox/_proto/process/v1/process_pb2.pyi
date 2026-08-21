from google.protobuf import empty_pb2 as _empty_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class Signal(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SIGNAL_UNSPECIFIED: _ClassVar[Signal]
    SIGNAL_SIGINT: _ClassVar[Signal]
    SIGNAL_SIGKILL: _ClassVar[Signal]
    SIGNAL_SIGTERM: _ClassVar[Signal]
SIGNAL_UNSPECIFIED: Signal
SIGNAL_SIGINT: Signal
SIGNAL_SIGKILL: Signal
SIGNAL_SIGTERM: Signal

class PTY(_message.Message):
    __slots__ = ("cols", "rows")
    COLS_FIELD_NUMBER: _ClassVar[int]
    ROWS_FIELD_NUMBER: _ClassVar[int]
    cols: int
    rows: int
    def __init__(self, cols: _Optional[int] = ..., rows: _Optional[int] = ...) -> None: ...

class ProcessConfig(_message.Message):
    __slots__ = ("command", "env_vars", "cwd")
    class EnvVarsEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    COMMAND_FIELD_NUMBER: _ClassVar[int]
    ENV_VARS_FIELD_NUMBER: _ClassVar[int]
    CWD_FIELD_NUMBER: _ClassVar[int]
    command: _containers.RepeatedScalarFieldContainer[str]
    env_vars: _containers.ScalarMap[str, str]
    cwd: str
    def __init__(self, command: _Optional[_Iterable[str]] = ..., env_vars: _Optional[_Mapping[str, str]] = ..., cwd: _Optional[str] = ...) -> None: ...

class StartRequest(_message.Message):
    __slots__ = ("config", "pty")
    CONFIG_FIELD_NUMBER: _ClassVar[int]
    PTY_FIELD_NUMBER: _ClassVar[int]
    config: ProcessConfig
    pty: PTY
    def __init__(self, config: _Optional[_Union[ProcessConfig, _Mapping]] = ..., pty: _Optional[_Union[PTY, _Mapping]] = ...) -> None: ...

class InitEvent(_message.Message):
    __slots__ = ("process_id",)
    PROCESS_ID_FIELD_NUMBER: _ClassVar[int]
    process_id: int
    def __init__(self, process_id: _Optional[int] = ...) -> None: ...

class ExitEvent(_message.Message):
    __slots__ = ("exit_code",)
    EXIT_CODE_FIELD_NUMBER: _ClassVar[int]
    exit_code: int
    def __init__(self, exit_code: _Optional[int] = ...) -> None: ...

class StartResponse(_message.Message):
    __slots__ = ("init", "stdout", "stderr", "exit")
    INIT_FIELD_NUMBER: _ClassVar[int]
    STDOUT_FIELD_NUMBER: _ClassVar[int]
    STDERR_FIELD_NUMBER: _ClassVar[int]
    EXIT_FIELD_NUMBER: _ClassVar[int]
    init: InitEvent
    stdout: bytes
    stderr: bytes
    exit: ExitEvent
    def __init__(self, init: _Optional[_Union[InitEvent, _Mapping]] = ..., stdout: _Optional[bytes] = ..., stderr: _Optional[bytes] = ..., exit: _Optional[_Union[ExitEvent, _Mapping]] = ...) -> None: ...

class ExecuteRequest(_message.Message):
    __slots__ = ("config",)
    CONFIG_FIELD_NUMBER: _ClassVar[int]
    config: ProcessConfig
    def __init__(self, config: _Optional[_Union[ProcessConfig, _Mapping]] = ...) -> None: ...

class ExecuteResponse(_message.Message):
    __slots__ = ("exit_code", "stdout", "stderr")
    EXIT_CODE_FIELD_NUMBER: _ClassVar[int]
    STDOUT_FIELD_NUMBER: _ClassVar[int]
    STDERR_FIELD_NUMBER: _ClassVar[int]
    exit_code: int
    stdout: bytes
    stderr: bytes
    def __init__(self, exit_code: _Optional[int] = ..., stdout: _Optional[bytes] = ..., stderr: _Optional[bytes] = ...) -> None: ...

class WriteStdinRequest(_message.Message):
    __slots__ = ("process_id", "input", "eof")
    PROCESS_ID_FIELD_NUMBER: _ClassVar[int]
    INPUT_FIELD_NUMBER: _ClassVar[int]
    EOF_FIELD_NUMBER: _ClassVar[int]
    process_id: int
    input: bytes
    eof: _empty_pb2.Empty
    def __init__(self, process_id: _Optional[int] = ..., input: _Optional[bytes] = ..., eof: _Optional[_Union[_empty_pb2.Empty, _Mapping]] = ...) -> None: ...

class WriteStdinResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class SendSignalRequest(_message.Message):
    __slots__ = ("process_id", "signal")
    PROCESS_ID_FIELD_NUMBER: _ClassVar[int]
    SIGNAL_FIELD_NUMBER: _ClassVar[int]
    process_id: int
    signal: Signal
    def __init__(self, process_id: _Optional[int] = ..., signal: _Optional[_Union[Signal, str]] = ...) -> None: ...

class SendSignalResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ResizeTTYRequest(_message.Message):
    __slots__ = ("process_id", "cols", "rows")
    PROCESS_ID_FIELD_NUMBER: _ClassVar[int]
    COLS_FIELD_NUMBER: _ClassVar[int]
    ROWS_FIELD_NUMBER: _ClassVar[int]
    process_id: int
    cols: int
    rows: int
    def __init__(self, process_id: _Optional[int] = ..., cols: _Optional[int] = ..., rows: _Optional[int] = ...) -> None: ...

class ResizeTTYResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...
