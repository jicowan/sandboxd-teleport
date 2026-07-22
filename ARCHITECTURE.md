# Architecture

This file is retired. It previously described the **agent-sandbox** front door
(`broker_mcp.py` + `SandboxClaim`s + agent-sandbox's `sandbox-router`), which is no
longer used — those files are parked in [`delete_me/`](./delete_me/README.md).

The current architecture lives under **[docs/sandboxd/](./docs/sandboxd/README.md)**:

- **[architecture-sandboxd.md](./docs/sandboxd/architecture-sandboxd.md)** — the
  sandboxd control plane itself (router, operator, Valkey, workers, S3; teleport;
  pools; CRDs). This is the standalone product.
- **[architecture-broker.md](./docs/sandboxd/architecture-broker.md)** — the optional
  reference front door (agentgateway + broker + Keycloak) that fronts sandboxd with
  authenticated, per‑user MCP.
- **[overview-and-vs-substrate.md](./docs/sandboxd/overview-and-vs-substrate.md)** —
  what sandboxd is and how it compares to Agent Substrate.

See the root **[README.md](./README.md)** for the top‑level picture.
