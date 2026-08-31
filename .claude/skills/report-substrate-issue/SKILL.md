---
name: report-substrate-issue
description: File a GitHub issue for a problem with env/substrate on the local kind setup (Claude Code on Substrate environments). Gathers cluster state, versions, and logs automatically, drafts the issue, and files it with gh. Use whenever the user hits a bug, error, or rough edge while using or setting up the substrate-env stack — even for small annoyances; the barrier to report should be near zero.
---

# Report a substrate/env issue

Turn whatever just went wrong into a well-formed GitHub issue with minimal
effort from the user. The user only needs to say what happened (even one
sentence, even just "file an issue for this"); everything else is collected
automatically. If the problem is visible in the current conversation (a
failed command, an error message), use that as the symptom without asking.

## 1. Collect diagnostics (best effort, never block on failures)

Run these and keep the outputs; skip anything that errors and note it as
unavailable:

```bash
# Versions / commits
git -C <env-checkout> log --oneline -1 && git -C <env-checkout> status --short | head -5
git -C <substrate-checkout> log --oneline -1
go version && kind version 2>/dev/null; claude --version 2>/dev/null

# Cluster state
kubectl -n ate-system get pods 2>&1
kubectl -n ate-env get pods,actortemplates,workerpools 2>&1
kubectl get actors -A 2>&1 | head -15

# Recent logs from the two components that fail most often
kubectl -n ate-env logs deploy/ate-env-api --tail=30 2>&1
kubectl -n ate-system logs deploy/atenet-router -c envoy --tail=20 2>&1
```

The env and substrate checkouts normally sit side by side (look for
`env/` and `substrate/` under the workspace root, e.g.
`~/github/agent-substrate/`).

## 2. Choose the repository

- `agent-substrate/env` — default. Anything involving ate-env-api,
  ate-env-guest, the MCP endpoint/tools, the `ate-env` CLI, idle
  suspension, manifests, clients, or the kind demo guide.
- `agent-substrate/substrate` — only when the evidence clearly points at
  core Substrate: atenet-router/envoy failures, actor scheduling or
  checkpoint/restore failures, atelet/ateapi crashes, kind install
  scripts. When unsure, file against `env`; maintainers will move it.

## 3. Draft the issue

Title: one line, symptom first, e.g.
`shell tool hangs after suspend/resume on kind`.

Body (omit sections with nothing to say — short beats complete):

```markdown
### What happened
<1-3 sentences; include the exact error text or tool output>

### How to reproduce
<numbered steps, or the single command; mention docs/demo-kind.md phase if applicable>

### Expected
<one sentence>

### Environment
- env: <commit> (<dirty/clean>), substrate: <commit>
- platform: <macOS/arch>, kind cluster per docs/demo-kind.md
- claude code: <version if MCP-related>

### Diagnostics
<details><summary>pods / logs</summary>

<only the relevant excerpts of the collected output — trim aggressively>

</details>
```

## 4. File it

Show the user the title and a one-line summary of the draft (not the whole
body), then:

```bash
gh issue create -R agent-substrate/<repo> --title "<title>" --body-file <tmpfile> --label kind-demo
```

If the label doesn't exist, retry without `--label`. If `gh` is missing or
unauthenticated (`gh auth status` fails), save the draft to
`/tmp/substrate-issue-draft.md`, print the path plus a
`gh auth login` hint, and give the user the manual URL:
`https://github.com/agent-substrate/<repo>/issues/new`.

Always end by printing the created issue URL.
