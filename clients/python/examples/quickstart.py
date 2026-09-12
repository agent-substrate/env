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

"""End-to-end example against a running ate-env-api (default localhost:7777).

Port-forward the service first:

    kubectl port-forward -n ate-env svc/ate-env-api 7777:7777
"""

import asyncio

from ate_env import Client, Signal


async def main():
    # One Client can be shared across tasks/agents; close it on shutdown.
    client = Client("localhost:7777")
    try:
        env = await client.create("demo-env")

        result = await env.shell("echo hello && uname -a")
        print(result.exit_code, result.stdout)

        await env.write_file("/workspace/notes.txt", b"hi\n", mode=0o644)
        print(await env.read_file_bytes("/workspace/notes.txt"))

        proc = await env.start_process(
            ["sh", "-c", "for i in 1 2 3; do echo $i; sleep 1; done"]
        )
        async for out in proc.output(follow=True):
            if out.stdout is not None:
                print("stdout", out.stdout.decode(), end="")
            elif out.stderr is not None:
                print("stderr", out.stderr.decode(), end="")
            else:
                print("exited with", out.exit.exit_code)

        # Interactive stdin and signals:
        cat = await env.start_process(["cat"], stdin=True)
        await cat.write_input(b"hello\n", close=True)
        print(await cat.wait())

        sleeper = await env.start_process(["sleep", "300"])
        await sleeper.signal(Signal.TERM)
        print((await sleeper.wait()).exit_code)  # 143 = 128 + SIGTERM

        await env.delete()
    finally:
        await client.close()


if __name__ == "__main__":
    asyncio.run(main())
