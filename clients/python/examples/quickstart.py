"""End-to-end example against a running ate-env-api (default localhost:7777).

Port-forward the service first:

    kubectl port-forward -n ate-env svc/ate-env-api 7777:7777
"""

import asyncio

from ate_env import Client


async def main():
    # One Client can be shared across tasks/agents; close it on shutdown.
    client = Client("localhost:7777")
    try:
        env = await client.create("demo-env")

        result = await env.shell("echo hello && uname -a")
        print(result.exit_code, result.stdout)

        await env.write_file("/workspace/notes.txt", b"hi\n", mode=0o644)
        print(await env.read_file_bytes("/workspace/notes.txt"))

        pid = await env.start_process(
            ["sh", "-c", "for i in 1 2 3; do echo $i; sleep 1; done"]
        )
        async for chunk in env.stream_outputs(pid, follow=True):
            print(chunk.source.name, chunk.data.decode())

        await env.delete()
    finally:
        await client.close()


if __name__ == "__main__":
    asyncio.run(main())
