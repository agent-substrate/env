# Copyright 2026 Google LLC
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

import datetime

from google.protobuf import duration_pb2 as _duration_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ProcessState(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    PROCESS_STATE_UNSPECIFIED: _ClassVar[ProcessState]
    PROCESS_STATE_RUNNING: _ClassVar[ProcessState]
    PROCESS_STATE_EXITED: _ClassVar[ProcessState]

class Signal(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SIGNAL_UNSPECIFIED: _ClassVar[Signal]
    SIGNAL_HUP: _ClassVar[Signal]
    SIGNAL_INT: _ClassVar[Signal]
    SIGNAL_QUIT: _ClassVar[Signal]
    SIGNAL_ILL: _ClassVar[Signal]
    SIGNAL_TRAP: _ClassVar[Signal]
    SIGNAL_ABRT: _ClassVar[Signal]
    SIGNAL_BUS: _ClassVar[Signal]
    SIGNAL_FPE: _ClassVar[Signal]
    SIGNAL_KILL: _ClassVar[Signal]
    SIGNAL_USR1: _ClassVar[Signal]
    SIGNAL_SEGV: _ClassVar[Signal]
    SIGNAL_USR2: _ClassVar[Signal]
    SIGNAL_PIPE: _ClassVar[Signal]
    SIGNAL_ALRM: _ClassVar[Signal]
    SIGNAL_TERM: _ClassVar[Signal]
    SIGNAL_CHLD: _ClassVar[Signal]
    SIGNAL_CONT: _ClassVar[Signal]
    SIGNAL_STOP: _ClassVar[Signal]
    SIGNAL_TSTP: _ClassVar[Signal]
    SIGNAL_TTIN: _ClassVar[Signal]
    SIGNAL_TTOU: _ClassVar[Signal]
    SIGNAL_URG: _ClassVar[Signal]
    SIGNAL_XCPU: _ClassVar[Signal]
    SIGNAL_XFSZ: _ClassVar[Signal]
    SIGNAL_VTALRM: _ClassVar[Signal]
    SIGNAL_PROF: _ClassVar[Signal]
    SIGNAL_WINCH: _ClassVar[Signal]
    SIGNAL_IO: _ClassVar[Signal]
    SIGNAL_SYS: _ClassVar[Signal]
PROCESS_STATE_UNSPECIFIED: ProcessState
PROCESS_STATE_RUNNING: ProcessState
PROCESS_STATE_EXITED: ProcessState
SIGNAL_UNSPECIFIED: Signal
SIGNAL_HUP: Signal
SIGNAL_INT: Signal
SIGNAL_QUIT: Signal
SIGNAL_ILL: Signal
SIGNAL_TRAP: Signal
SIGNAL_ABRT: Signal
SIGNAL_BUS: Signal
SIGNAL_FPE: Signal
SIGNAL_KILL: Signal
SIGNAL_USR1: Signal
SIGNAL_SEGV: Signal
SIGNAL_USR2: Signal
SIGNAL_PIPE: Signal
SIGNAL_ALRM: Signal
SIGNAL_TERM: Signal
SIGNAL_CHLD: Signal
SIGNAL_CONT: Signal
SIGNAL_STOP: Signal
SIGNAL_TSTP: Signal
SIGNAL_TTIN: Signal
SIGNAL_TTOU: Signal
SIGNAL_URG: Signal
SIGNAL_XCPU: Signal
SIGNAL_XFSZ: Signal
SIGNAL_VTALRM: Signal
SIGNAL_PROF: Signal
SIGNAL_WINCH: Signal
SIGNAL_IO: Signal
SIGNAL_SYS: Signal

class Process(_message.Message):
    __slots__ = ("process_id", "command", "pid", "state", "exit_code", "started_at", "finished_at")
    PROCESS_ID_FIELD_NUMBER: _ClassVar[int]
    COMMAND_FIELD_NUMBER: _ClassVar[int]
    PID_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    EXIT_CODE_FIELD_NUMBER: _ClassVar[int]
    STARTED_AT_FIELD_NUMBER: _ClassVar[int]
    FINISHED_AT_FIELD_NUMBER: _ClassVar[int]
    process_id: str
    command: _containers.RepeatedScalarFieldContainer[str]
    pid: int
    state: ProcessState
    exit_code: int
    started_at: _timestamp_pb2.Timestamp
    finished_at: _timestamp_pb2.Timestamp
    def __init__(self, process_id: _Optional[str] = ..., command: _Optional[_Iterable[str]] = ..., pid: _Optional[int] = ..., state: _Optional[_Union[ProcessState, str]] = ..., exit_code: _Optional[int] = ..., started_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., finished_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class StartProcessRequest(_message.Message):
    __slots__ = ("command", "cwd", "env", "stdin", "timeout")
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
    STDIN_FIELD_NUMBER: _ClassVar[int]
    TIMEOUT_FIELD_NUMBER: _ClassVar[int]
    command: _containers.RepeatedScalarFieldContainer[str]
    cwd: str
    env: _containers.ScalarMap[str, str]
    stdin: bool
    timeout: _duration_pb2.Duration
    def __init__(self, command: _Optional[_Iterable[str]] = ..., cwd: _Optional[str] = ..., env: _Optional[_Mapping[str, str]] = ..., stdin: _Optional[bool] = ..., timeout: _Optional[_Union[datetime.timedelta, _duration_pb2.Duration, _Mapping]] = ...) -> None: ...

class GetProcessRequest(_message.Message):
    __slots__ = ("process_id",)
    PROCESS_ID_FIELD_NUMBER: _ClassVar[int]
    process_id: str
    def __init__(self, process_id: _Optional[str] = ...) -> None: ...

class StreamProcessOutputRequest(_message.Message):
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

class ProcessOutput(_message.Message):
    __slots__ = ("stdout", "stderr", "exit")
    STDOUT_FIELD_NUMBER: _ClassVar[int]
    STDERR_FIELD_NUMBER: _ClassVar[int]
    EXIT_FIELD_NUMBER: _ClassVar[int]
    stdout: bytes
    stderr: bytes
    exit: Process
    def __init__(self, stdout: _Optional[bytes] = ..., stderr: _Optional[bytes] = ..., exit: _Optional[_Union[Process, _Mapping]] = ...) -> None: ...

class WriteProcessInputRequest(_message.Message):
    __slots__ = ("process_id", "data", "close")
    PROCESS_ID_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    CLOSE_FIELD_NUMBER: _ClassVar[int]
    process_id: str
    data: bytes
    close: bool
    def __init__(self, process_id: _Optional[str] = ..., data: _Optional[bytes] = ..., close: _Optional[bool] = ...) -> None: ...

class WriteProcessInputResponse(_message.Message):
    __slots__ = ("bytes_written",)
    BYTES_WRITTEN_FIELD_NUMBER: _ClassVar[int]
    bytes_written: int
    def __init__(self, bytes_written: _Optional[int] = ...) -> None: ...

class SignalProcessRequest(_message.Message):
    __slots__ = ("process_id", "signal")
    PROCESS_ID_FIELD_NUMBER: _ClassVar[int]
    SIGNAL_FIELD_NUMBER: _ClassVar[int]
    process_id: str
    signal: Signal
    def __init__(self, process_id: _Optional[str] = ..., signal: _Optional[_Union[Signal, str]] = ...) -> None: ...

class ReadFileRequest(_message.Message):
    __slots__ = ("path", "mode")
    PATH_FIELD_NUMBER: _ClassVar[int]
    MODE_FIELD_NUMBER: _ClassVar[int]
    path: str
    mode: int
    def __init__(self, path: _Optional[str] = ..., mode: _Optional[int] = ...) -> None: ...

class ReadFileResponse(_message.Message):
    __slots__ = ("chunk",)
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    chunk: bytes
    def __init__(self, chunk: _Optional[bytes] = ...) -> None: ...

class WriteFileRequest(_message.Message):
    __slots__ = ("path", "mode", "seek_offset", "chunk")
    PATH_FIELD_NUMBER: _ClassVar[int]
    MODE_FIELD_NUMBER: _ClassVar[int]
    SEEK_OFFSET_FIELD_NUMBER: _ClassVar[int]
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    path: str
    mode: int
    seek_offset: int
    chunk: bytes
    def __init__(self, path: _Optional[str] = ..., mode: _Optional[int] = ..., seek_offset: _Optional[int] = ..., chunk: _Optional[bytes] = ...) -> None: ...

class WriteFileResponse(_message.Message):
    __slots__ = ("bytes_written",)
    BYTES_WRITTEN_FIELD_NUMBER: _ClassVar[int]
    bytes_written: int
    def __init__(self, bytes_written: _Optional[int] = ...) -> None: ...
