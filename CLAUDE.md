# vyos — Agent Operating Guide

Native OpenTofu/Terraform provider for **VyOS** via the **VyOS HTTP API**.
Sibling of `../tofu-aruba-aos` and `../openwrt-ubus` (same generic-over-the-API
philosophy, same toolchain). The workspace-root `../CLAUDE.md` applies; this
adds specifics.

## What this is / isn't

- **Is:** a provider for VyOS routers/firewalls, driven entirely through the
  documented HTTP API (`/configure`, `/retrieve`, `/config-file`, …),
  API-key authenticated.
- **Isn't:** a config-file editor or an SSH/CLI scraper. Every change goes
  through the HTTP API, where `/configure` applies **and commits** atomically.

## Design tenets

General Go/provider standards: see `/home/jameson/source/ai-prompts/go.md`.

- **The generic resources here are `vyos_config` (+ data source)** — they
  address any config path. Resist adding typed resources until there's a real
  ergonomics need.
- **Config-path model.** A node is addressed by `path` (a list of segments,
  ForceNew). `config` is the managed subtree, in the `showConfig` JSON shape.
- **Flatten, don't replace.** Create/Update flatten the declared subtree into
  `set` path-arrays (`setCommands`); Update also emits `delete` for leaves
  dropped vs. the prior declared subtree (`pruneCommands`). VyOS encodes a
  value as the *final path segment*, not a separate field.
- **The subset plan modifier is `subsetMatches`** (recursive value-subset);
  `config` is the keys we manage. State holds the full device subtree, so a
  subset declaration imports/refreshes to 0-diff and never clobbers unmanaged
  config.
- **No 404.** VyOS has no HTTP 404; `vyos.NotFound` classifies the
  "empty/invalid/not-exist" error messages `showConfig` returns for an absent
  path so Read can drop the resource from state.

## Toolchain

- Go 1.26.4 (`/home/jameson/.local/go`), `terraform-plugin-framework` v1.19.0.
  Reuse `../tofu-aruba-aos`'s vetted dependency versions — do not add/bump deps.
- Provider address: `registry.terraform.io/JamesonRGrieve/tofu-vyos`.
- Gate: `make check` (tidy + fmt + vet + test + build); pre-commit re-runs it.

## Hard rules

- **No secrets in the repo.** Creds come from the provider config (OpenBao →
  `TF_VAR_*` via Semaphore). The `key` provider attribute is the API key.
- **Live changes via Semaphore / 0-diff first.** Never hand-apply against a
  production router. Validate against a LAB VyOS only.
