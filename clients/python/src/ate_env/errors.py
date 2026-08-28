"""Exceptions raised by the ate-env client."""

from __future__ import annotations

import grpc

__all__ = [
    "EnvError",
    "NotFoundError",
    "InvalidArgumentError",
    "PermissionDeniedError",
    "RpcError",
    "map_rpc_error",
]


class EnvError(Exception):
    """Base class for all ate-env client errors."""


class NotFoundError(EnvError):
    """Environment, process, file, or directory does not exist."""


class InvalidArgumentError(EnvError):
    """The server rejected the request as malformed."""


class PermissionDeniedError(EnvError):
    """Access denied — e.g. a file path outside the workspace sandbox."""


class RpcError(EnvError):
    """Any other RPC failure; carries the gRPC status code."""

    def __init__(self, message: str, code: grpc.StatusCode | None = None):
        super().__init__(message)
        self.code = code


def map_rpc_error(err: BaseException) -> BaseException:
    """Convert a grpc.RpcError (including grpc.aio.AioRpcError) into an EnvError.

    Non-RPC exceptions pass through unchanged.
    """
    if not isinstance(err, grpc.RpcError):
        return err
    code = err.code()
    details = err.details() or str(err)
    if code == grpc.StatusCode.NOT_FOUND:
        return NotFoundError(details)
    if code == grpc.StatusCode.INVALID_ARGUMENT:
        return InvalidArgumentError(details)
    if code == grpc.StatusCode.PERMISSION_DENIED:
        return PermissionDeniedError(details)
    return RpcError(details, code)
