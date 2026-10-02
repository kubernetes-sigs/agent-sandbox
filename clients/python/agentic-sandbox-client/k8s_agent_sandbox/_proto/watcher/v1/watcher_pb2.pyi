from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class FileEventType(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    FILE_EVENT_TYPE_UNSPECIFIED: _ClassVar[FileEventType]
    FILE_EVENT_TYPE_CREATE: _ClassVar[FileEventType]
    FILE_EVENT_TYPE_WRITE: _ClassVar[FileEventType]
    FILE_EVENT_TYPE_REMOVE: _ClassVar[FileEventType]
    FILE_EVENT_TYPE_RENAME: _ClassVar[FileEventType]
    FILE_EVENT_TYPE_CHMOD: _ClassVar[FileEventType]
    FILE_EVENT_TYPE_ERROR: _ClassVar[FileEventType]
FILE_EVENT_TYPE_UNSPECIFIED: FileEventType
FILE_EVENT_TYPE_CREATE: FileEventType
FILE_EVENT_TYPE_WRITE: FileEventType
FILE_EVENT_TYPE_REMOVE: FileEventType
FILE_EVENT_TYPE_RENAME: FileEventType
FILE_EVENT_TYPE_CHMOD: FileEventType
FILE_EVENT_TYPE_ERROR: FileEventType

class WatchDirRequest(_message.Message):
    __slots__ = ("path", "recursive")
    PATH_FIELD_NUMBER: _ClassVar[int]
    RECURSIVE_FIELD_NUMBER: _ClassVar[int]
    path: str
    recursive: bool
    def __init__(self, path: _Optional[str] = ..., recursive: _Optional[bool] = ...) -> None: ...

class FileEvent(_message.Message):
    __slots__ = ("type", "path", "old_path", "error")
    TYPE_FIELD_NUMBER: _ClassVar[int]
    PATH_FIELD_NUMBER: _ClassVar[int]
    OLD_PATH_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    type: FileEventType
    path: str
    old_path: str
    error: str
    def __init__(self, type: _Optional[_Union[FileEventType, str]] = ..., path: _Optional[str] = ..., old_path: _Optional[str] = ..., error: _Optional[str] = ...) -> None: ...
