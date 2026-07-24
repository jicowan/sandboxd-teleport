# Examples

Worked examples of the two shapes of workload sandboxd runs.

| Example | Shape | What it shows |
|---------|-------|---------------|
| **on-demand / multi-app** (in [`../checkpoint-restore/controlplane/deploy/aio/`](../checkpoint-restore/controlplane/deploy/aio/)) | request→response | A user sends a request through an MCP client; a session is created/resumed on a generic pool, the request is processed, a response returns. Two MCP apps share one pool, fronted by the reference broker + agentgateway. |
| [**forkset** (RL)](forkset/) | fan-out | A `ForkSet` fans out N isolated environments from a common source — either **cold-start** (independent trials) or a **golden snapshot** (branch-from-a-known-state) — for reinforcement-learning parallel rollouts. Driven directly against the router (no broker). |

For the concepts behind these, see [docs/sandboxd/](../docs/sandboxd/) — especially
[architecture-sandboxd.md](../docs/sandboxd/architecture-sandboxd.md) and, for the
ForkSet design, [docs/PRD-snapshot-fork.md](../docs/PRD-snapshot-fork.md).
