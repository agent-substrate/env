import pytest
from pydantic import ValidationError

from ate_env.config import FleetConfig


def test_unknown_fields_are_rejected():
    # Previously silently dropped by pydantic (e.g. the NeMo/TML doc snippets).
    with pytest.raises(ValidationError, match="warm_pool_size"):
        FleetConfig(warm_pool_size=64)
    with pytest.raises(ValidationError, match="warmpool_replicas"):
        FleetConfig.from_dict({"backend": "mock", "warmpool_replicas": 4})


@pytest.mark.parametrize("kwargs", [
    {"batch_size": 0},
    {"max_concurrent": 0},
    {"max_warmpool_replicas": -1},
    {"acquire_timeout_s": 0},
    {"tenancy": "Bad_Name"},
    {"tenancy": "-leading-dash"},
    {"strategy": "rolling"},
    {"data_plane": "grpc"},  # requires grpc_endpoint
    {"grpc_endpoint": "{actor}.svc:50051"},  # unknown placeholder
])
def test_invalid_values_are_rejected(kwargs):
    with pytest.raises(ValidationError):
        FleetConfig(**kwargs)


def test_grpc_endpoint_placeholders_are_accepted():
    cfg = FleetConfig(data_plane="grpc", grpc_endpoint="{actor_id}.{atespace}.svc:50051")
    assert cfg.grpc_endpoint == "{actor_id}.{atespace}.svc:50051"


def test_auth_token_is_secret_and_round_trips():
    cfg = FleetConfig(auth_token="s3cr3t")
    assert "s3cr3t" not in repr(cfg)
    assert "s3cr3t" not in str(cfg.model_dump())
    # model_dump() -> from_dict() is how configs are shipped to Ray workers.
    again = FleetConfig.from_dict(cfg.model_dump())
    assert again.auth_token.get_secret_value() == "s3cr3t"
