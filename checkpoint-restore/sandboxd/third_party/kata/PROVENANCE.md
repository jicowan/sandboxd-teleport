# third_party/kata — vendored kata-containers sources

Source copied from [kata-containers](https://github.com/kata-containers/kata-containers)
for sandboxd's microVM runtime (`ch_driver.go` + `kata/`). The upstream `LICENSE`
in this directory covers everything under it.

- **Upstream:** github.com/kata-containers/kata-containers
- **Version:** tag `3.31.0` (matches the kata runtime assets used on the microVM nodes)
- **License:** Apache-2.0 — `./LICENSE` is the upstream license verbatim; the per-file
  copyright headers (HyperHQ, Ant Group, Intel, Databricks) are retained verbatim.
- **Vendored via:** Agent Substrate's `cmd/ateom-microvm/internal/third_party/kata`
  (Apache-2.0, Google LLC), which first extracted and generated these; sandboxd copies
  that vendoring and repoints the Go import path.
- **Ported from substrate commit:** `c538b68b` (2026-08-12) — the ch/, kata/, reaper/,
  and agentpb sources under checkpoint-restore/sandboxd/ were copied verbatim from this
  substrate revision and re-import-pathed. Re-sync from the same upstream when updating.

## agentpb — kata-agent ttrpc protobufs

A copy of the kata-agent protocol-buffer API, used to drive the kata-agent over ttrpc
when the microVM driver boots the guest.

- **Path upstream:** `src/libs/protocols/protos/{agent,oci,types,csi}.proto`
- The `.proto` files are byte-identical to 3.31.0 except `option go_package`, repointed
  to `github.com/jicowan/aio-sandbox/sandboxd/third_party/kata/agentpb` so the generated
  Go lands in-tree. (An embedded `go_package` string remains inside the generated
  file-descriptor bytes in `*.pb.go`; it is cosmetic and does not affect the Go import
  path or runtime behavior.)

### Why a copy instead of a module dependency

kata-containers is a large module; adding it to `go.mod` to use a handful of message
types would pull a heavy, unrelated dependency tree. We vendor just these four `.proto`
files and their generated Go. All required deps (containerd/ttrpc, protobuf,
runtime-spec, go-toml/v2) are already in sandboxd's go.mod.

## To regenerate

Check out kata tag 3.31.0, copy the four `.proto` files, set `option go_package` to the
in-tree path, and run `protoc --go_out` (protoc-gen-go v1.36.x, protoc v4.25.x).
