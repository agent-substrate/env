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

from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class EnvironmentStatus(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    ENVIRONMENT_STATUS_UNSPECIFIED: _ClassVar[EnvironmentStatus]
    ENVIRONMENT_STATUS_RESUMING: _ClassVar[EnvironmentStatus]
    ENVIRONMENT_STATUS_RUNNING: _ClassVar[EnvironmentStatus]
    ENVIRONMENT_STATUS_SUSPENDING: _ClassVar[EnvironmentStatus]
    ENVIRONMENT_STATUS_SUSPENDED: _ClassVar[EnvironmentStatus]
    ENVIRONMENT_STATUS_PAUSING: _ClassVar[EnvironmentStatus]
    ENVIRONMENT_STATUS_PAUSED: _ClassVar[EnvironmentStatus]
    ENVIRONMENT_STATUS_CRASHED: _ClassVar[EnvironmentStatus]
    ENVIRONMENT_STATUS_DELETING: _ClassVar[EnvironmentStatus]
ENVIRONMENT_STATUS_UNSPECIFIED: EnvironmentStatus
ENVIRONMENT_STATUS_RESUMING: EnvironmentStatus
ENVIRONMENT_STATUS_RUNNING: EnvironmentStatus
ENVIRONMENT_STATUS_SUSPENDING: EnvironmentStatus
ENVIRONMENT_STATUS_SUSPENDED: EnvironmentStatus
ENVIRONMENT_STATUS_PAUSING: EnvironmentStatus
ENVIRONMENT_STATUS_PAUSED: EnvironmentStatus
ENVIRONMENT_STATUS_CRASHED: EnvironmentStatus
ENVIRONMENT_STATUS_DELETING: EnvironmentStatus

class Template(_message.Message):
    __slots__ = ("name", "atespace")
    NAME_FIELD_NUMBER: _ClassVar[int]
    ATESPACE_FIELD_NUMBER: _ClassVar[int]
    name: str
    atespace: str
    def __init__(self, name: _Optional[str] = ..., atespace: _Optional[str] = ...) -> None: ...

class Environment(_message.Message):
    __slots__ = ("id", "atespace", "template", "status")
    ID_FIELD_NUMBER: _ClassVar[int]
    ATESPACE_FIELD_NUMBER: _ClassVar[int]
    TEMPLATE_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    id: str
    atespace: str
    template: Template
    status: EnvironmentStatus
    def __init__(self, id: _Optional[str] = ..., atespace: _Optional[str] = ..., template: _Optional[_Union[Template, _Mapping]] = ..., status: _Optional[_Union[EnvironmentStatus, str]] = ...) -> None: ...

class CreateEnvironmentRequest(_message.Message):
    __slots__ = ("id", "atespace", "template")
    ID_FIELD_NUMBER: _ClassVar[int]
    ATESPACE_FIELD_NUMBER: _ClassVar[int]
    TEMPLATE_FIELD_NUMBER: _ClassVar[int]
    id: str
    atespace: str
    template: Template
    def __init__(self, id: _Optional[str] = ..., atespace: _Optional[str] = ..., template: _Optional[_Union[Template, _Mapping]] = ...) -> None: ...

class CreateEnvironmentResponse(_message.Message):
    __slots__ = ("environment",)
    ENVIRONMENT_FIELD_NUMBER: _ClassVar[int]
    environment: Environment
    def __init__(self, environment: _Optional[_Union[Environment, _Mapping]] = ...) -> None: ...

class GetEnvironmentRequest(_message.Message):
    __slots__ = ("id", "atespace")
    ID_FIELD_NUMBER: _ClassVar[int]
    ATESPACE_FIELD_NUMBER: _ClassVar[int]
    id: str
    atespace: str
    def __init__(self, id: _Optional[str] = ..., atespace: _Optional[str] = ...) -> None: ...

class GetEnvironmentResponse(_message.Message):
    __slots__ = ("environment",)
    ENVIRONMENT_FIELD_NUMBER: _ClassVar[int]
    environment: Environment
    def __init__(self, environment: _Optional[_Union[Environment, _Mapping]] = ...) -> None: ...

class SuspendEnvironmentRequest(_message.Message):
    __slots__ = ("id", "atespace")
    ID_FIELD_NUMBER: _ClassVar[int]
    ATESPACE_FIELD_NUMBER: _ClassVar[int]
    id: str
    atespace: str
    def __init__(self, id: _Optional[str] = ..., atespace: _Optional[str] = ...) -> None: ...

class SuspendEnvironmentResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class DeleteEnvironmentRequest(_message.Message):
    __slots__ = ("id", "atespace")
    ID_FIELD_NUMBER: _ClassVar[int]
    ATESPACE_FIELD_NUMBER: _ClassVar[int]
    id: str
    atespace: str
    def __init__(self, id: _Optional[str] = ..., atespace: _Optional[str] = ...) -> None: ...

class DeleteEnvironmentResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...
