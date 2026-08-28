import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ProcessStatus(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    PROCESS_STATUS_UNSPECIFIED: _ClassVar[ProcessStatus]
    PROCESS_STATUS_RUNNING: _ClassVar[ProcessStatus]
    PROCESS_STATUS_COMPLETED: _ClassVar[ProcessStatus]
    PROCESS_STATUS_FAILED: _ClassVar[ProcessStatus]
    PROCESS_STATUS_TERMINATED: _ClassVar[ProcessStatus]

class OutputSource(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    OUTPUT_SOURCE_UNSPECIFIED: _ClassVar[OutputSource]
    OUTPUT_SOURCE_STDOUT: _ClassVar[OutputSource]
    OUTPUT_SOURCE_STDERR: _ClassVar[OutputSource]
PROCESS_STATUS_UNSPECIFIED: ProcessStatus
PROCESS_STATUS_RUNNING: ProcessStatus
PROCESS_STATUS_COMPLETED: ProcessStatus
PROCESS_STATUS_FAILED: ProcessStatus
PROCESS_STATUS_TERMINATED: ProcessStatus
OUTPUT_SOURCE_UNSPECIFIED: OutputSource
OUTPUT_SOURCE_STDOUT: OutputSource
OUTPUT_SOURCE_STDERR: OutputSource

class Process(_message.Message):
    __slots__ = ("process_id", "status", "exit_code", "started_at", "finished_at")
    PROCESS_ID_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    EXIT_CODE_FIELD_NUMBER: _ClassVar[int]
    STARTED_AT_FIELD_NUMBER: _ClassVar[int]
    FINISHED_AT_FIELD_NUMBER: _ClassVar[int]
    process_id: str
    status: ProcessStatus
    exit_code: int
    started_at: _timestamp_pb2.Timestamp
    finished_at: _timestamp_pb2.Timestamp
    def __init__(self, process_id: _Optional[str] = ..., status: _Optional[_Union[ProcessStatus, str]] = ..., exit_code: _Optional[int] = ..., started_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., finished_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class StartProcessRequest(_message.Message):
    __slots__ = ("command", "cwd", "env")
    class EnvEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    COMMAND_FIELD_NUMBER: _ClassVar[int]
    CWD_FIELD_NUMBER: _ClassVar[int]
    ENV_FIELD_NUMBER: _ClassVar[int]
    command: _containers.RepeatedScalarFieldContainer[str]
    cwd: str
    env: _containers.ScalarMap[str, str]
    def __init__(self, command: _Optional[_Iterable[str]] = ..., cwd: _Optional[str] = ..., env: _Optional[_Mapping[str, str]] = ...) -> None: ...

class StartProcessResponse(_message.Message):
    __slots__ = ("process_id",)
    PROCESS_ID_FIELD_NUMBER: _ClassVar[int]
    process_id: str
    def __init__(self, process_id: _Optional[str] = ...) -> None: ...

class GetProcessRequest(_message.Message):
    __slots__ = ("process_id",)
    PROCESS_ID_FIELD_NUMBER: _ClassVar[int]
    process_id: str
    def __init__(self, process_id: _Optional[str] = ...) -> None: ...

class StreamProcessOutputsRequest(_message.Message):
    __slots__ = ("process_id", "stdout_offset", "stderr_offset", "follow")
    PROCESS_ID_FIELD_NUMBER: _ClassVar[int]
    STDOUT_OFFSET_FIELD_NUMBER: _ClassVar[int]
    STDERR_OFFSET_FIELD_NUMBER: _ClassVar[int]
    FOLLOW_FIELD_NUMBER: _ClassVar[int]
    process_id: str
    stdout_offset: int
    stderr_offset: int
    follow: bool
    def __init__(self, process_id: _Optional[str] = ..., stdout_offset: _Optional[int] = ..., stderr_offset: _Optional[int] = ..., follow: _Optional[bool] = ...) -> None: ...

class OutputChunk(_message.Message):
    __slots__ = ("source", "data")
    SOURCE_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    source: OutputSource
    data: bytes
    def __init__(self, source: _Optional[_Union[OutputSource, str]] = ..., data: _Optional[bytes] = ...) -> None: ...

class KillProcessRequest(_message.Message):
    __slots__ = ("process_id",)
    PROCESS_ID_FIELD_NUMBER: _ClassVar[int]
    process_id: str
    def __init__(self, process_id: _Optional[str] = ...) -> None: ...

class KillProcessResponse(_message.Message):
    __slots__ = ("exit_code",)
    EXIT_CODE_FIELD_NUMBER: _ClassVar[int]
    exit_code: int
    def __init__(self, exit_code: _Optional[int] = ...) -> None: ...

class ReadFileRequest(_message.Message):
    __slots__ = ("path",)
    PATH_FIELD_NUMBER: _ClassVar[int]
    path: str
    def __init__(self, path: _Optional[str] = ...) -> None: ...

class FileChunk(_message.Message):
    __slots__ = ("data",)
    DATA_FIELD_NUMBER: _ClassVar[int]
    data: bytes
    def __init__(self, data: _Optional[bytes] = ...) -> None: ...

class WriteFileRequest(_message.Message):
    __slots__ = ("path", "chunk", "mode")
    PATH_FIELD_NUMBER: _ClassVar[int]
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    MODE_FIELD_NUMBER: _ClassVar[int]
    path: str
    chunk: bytes
    mode: int
    def __init__(self, path: _Optional[str] = ..., chunk: _Optional[bytes] = ..., mode: _Optional[int] = ...) -> None: ...

class WriteFileResponse(_message.Message):
    __slots__ = ("bytes_written",)
    BYTES_WRITTEN_FIELD_NUMBER: _ClassVar[int]
    bytes_written: int
    def __init__(self, bytes_written: _Optional[int] = ...) -> None: ...
