# NeMo Skills local sandbox server

`local_sandbox_server.py` is copied verbatim from
https://github.com/NVIDIA/NeMo-Skills, path
`nemo_skills/code_execution/local_sandbox/local_sandbox_server.py`, at commit
`bcf059a` (2026-10). Licensed by NVIDIA under Apache-2.0 (`LICENSE`). It is the
Flask application behind NeMo Skills' `LocalSandbox` client: `POST /execute`
with `generated_code`, `language` (`ipython`, `python`, `shell`, `lean4`),
`timeout`, optional `std_input` and `session_id`; `GET /health`; and the
IPython session routes. `nemo_runtime.sandbox_server` loads it and serves it
on the host and port the sidecar command names. To update, copy the new file,
bump the commit here, and re-lock `requirements.in` against NeMo Skills'
`requirements/sandbox.lock`.
