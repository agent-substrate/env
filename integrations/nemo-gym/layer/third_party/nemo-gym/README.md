# NeMo Gym exec server

`exec_server.py` is the `EXEC_SERVER_SCRIPT` string from
https://github.com/NVIDIA-NeMo/Gym, path
`nemo_gym/sandbox/providers/ecs_fargate/engine.py`, at commit `d4fd5e8`
(2026-10), written out as a file; the text is unchanged. Licensed by NVIDIA
under Apache-2.0 (`LICENSE`). It is the zero-dependency HTTP exec server the ECS
Fargate provider injects into its sandboxes: `POST /exec` with `cmd` and
`timeout`, `POST /upload` with `path`, base64 `content` and optional `mode`,
`GET /download?path=`, `GET /health`. It reads `TB_EXEC_BIND` and
`TB_EXEC_PORT`; `nemo_runtime.exec_server` sets them from its flags and runs
the script. To update, copy the string out of the new `engine.py` and bump the
commit here.
