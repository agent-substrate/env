# Demo: Claude Code on Substrate environments

A five-beat script showing an unmodified Claude Code driving shell and file
operations inside Substrate environments on a local kind cluster — with
environments that suspend themselves when idle and resume without the
harness ever noticing.

Prerequisite: the stack from [setup-kind.md](setup-kind.md), through
Phase 4 — cluster up, `demo1` created, the `ate-env-endpoint` relay
serving `localhost:7777`, and the `substrate-env` MCP server registered
with Claude Code. The beats assume the demo-friendly `--idle-ttl 90s` from
the setup guide.

Keep a second terminal open for the `kubectl` / `ate-env` checkpoints.

## Opening

Start `claude` and set the stage:

> Use only the substrate-env MCP tools for all shell and file operations —
> never local tools. First, show me `uname -a` and `cat /etc/os-release` so
> we know where you are.

Checkpoint: `Linux actor 4.19.0-gvisor ... aarch64` on Alpine — the
sandbox, not the laptop. (For a locked-down run where local execution is
impossible, launch with `claude --disallowedTools Bash`.)

## Beat 1 — write & run

Chunked `write_file` plus process execution. The guest image is minimal
Alpine — stick to `sh`, the busybox toolset, and files:

> Write /tmp/fib.sh that prints the first 20 Fibonacci numbers, make it
> executable, run it, and show the output.

## Beat 2 — a long command

The atenet router bounds every request at ~10s, yet this runs for 75s and
loses nothing — the data plane reconnects its output streams by byte offset
behind the scenes:

> Run `for i in 1 2 3 4 5; do echo tick $i; sleep 15; done; echo DONE` with
> the shell tool and show the full output.

## Beat 3 — auto-suspend economics

Stop prompting for ~2 minutes, then in the second terminal:

```bash
/tmp/ate-env get demo1
kubectl -n ate-env logs deploy/ate-env-api | grep "idle reaper"
```

Expect `demo1  default  suspended` — nobody asked for it — and a log line
like `idle reaper: suspended default/demo1 after 1m30s of inactivity`.

Now ask Claude to run anything — the call transparently resumes the actor
(~1s) and `get demo1` shows `running` again. The harness never knew.

## Beat 4 — suspend mid-job, lose nothing

> Start `for i in $(seq 1 45); do echo step $i; sleep 2; done; echo BG-DONE`
> with start_process and tell me the process id. Then wait for my go-ahead.

Wait ~2 minutes: the reaper suspends the environment *while the job runs* —
the checkpoint freezes the process mid-loop. Verify in the second terminal
with `/tmp/ate-env get demo1`, then:

> Now follow that process with stream_process_logs until it finishes.

The environment resumes, the frozen process continues where it stopped, and
every line from `step 1` through `BG-DONE` arrives — no gap, no error.

## Beat 5 — the economics (optional)

```bash
kubectl -n ate-env get pods
kubectl get actors -A
```

Two pre-warmed workers serve any number of environments (that's the whole
pod list), and the actor listing shows each environment and its state;
suspended ones cost only their snapshot in storage.

**Epilogue (optional):** quit Claude Code, relaunch, and ask it to check the
Beat 4 process id with `get_process` — connections and sessions are
disposable; the process handle and its output are durable.

If anything misbehaves, the setup guide's
[troubleshooting section](setup-kind.md#troubleshooting) is the first stop —
and please file what you hit: `/report-substrate-issue <one sentence>`.
