<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# terraform-provider-vyos

A native OpenTofu/Terraform provider for **VyOS** routers/firewalls via the
**VyOS HTTP API** (HTTPS, API-key auth).

VyOS is **config-path based**, not REST CRUD. The HTTP API POSTs operations
(`set` / `delete` / `showConfig` / …) carrying a *path* (a list of config-tree
segments) — `/configure` applies **and commits** the change as a single
transaction. This provider models that directly: a config node is addressed by
its `path`, and the managed config subtree is declared as `config`.

## Why generic

The VyOS config tree is vast and stable (`interfaces`, `firewall`, `nat`,
`service`, `system`, `protocols`, `vpn`, …). Rather than hand-code a resource
per feature (and chase release additions forever), this provider is **generic
over the API** — one resource and one data source address *any* config path.
That is **100% feature coverage** by construction.

## Resources

### `vyos_config` (resource)

CRUD + `ImportState` for any VyOS config node.

```hcl
resource "vyos_config" "eth1" {
  path   = ["interfaces", "ethernet", "eth1"]
  config = jsonencode({
    address     = "192.168.1.1/24"
    description = "lan"
  })
}

resource "vyos_config" "hostname" {
  path   = ["system", "host-name"]
  config = jsonencode("router1")   # single-value leaf
}
```

The `config` value mirrors the shape `/retrieve showConfig` returns for the
node: a nested object for sub-nodes, a string for a single-value leaf, an array
of strings for a multi-value leaf, and `{}` for a valueless (tag-present) node.
On create/update the subtree is **flattened into `set` commands** (and removed
keys into `delete` commands), all POSTed to `/configure` in one atomic commit.

**Manage-declared-only / 0-diff imports.** `config` declares *only* the keys you
manage. State holds the full device subtree (from `showConfig`); a plan modifier
suppresses the diff when every declared key already matches the device, so:

- importing an existing node (`tofu import` / `import {}` block) lands at
  **0-diff** with no apply against the router, and
- the provider never clobbers config you didn't declare.

| Attribute | | Meaning |
|-----------|---|---------|
| `path` | required, ForceNew | config path as a list of segments, e.g. `["interfaces","ethernet","eth1"]` |
| `config` | required | JSON subtree of the keys you manage (showConfig shape) |
| `id` | computed | the `path` segments joined by `/` |

Import id is the path joined by `/`:

```sh
tofu import vyos_config.eth1 interfaces/ethernet/eth1
```

### `vyos_config` (data source)

```hcl
data "vyos_config" "eth0" { path = ["interfaces", "ethernet", "eth0"] }
# .response is the subtree as compact JSON; path = [] returns the whole config
```

## Provider configuration

```hcl
terraform {
  required_providers {
    vyos = { source = "registry.terraform.io/jamesonrgrieve/vyos" }
  }
}

provider "vyos" {
  host     = "192.168.7.x"   # no scheme; the API is HTTPS
  key      = var.vyos_api_key # sensitive
  insecure = true            # VyOS self-signed cert (default true)
}
```

The API key is configured on the router under
`service https api keys id <name> key <key>`.

## Local build / dev install

```sh
make build          # -> terraform-provider-vyos
make install        # installs to $DEV_BIN_DIR for a dev_overrides .tfrc
make check          # tidy + fmt + vet + test + build (pre-commit / CI gate)
```

For runners without registry access, install into a filesystem mirror:
`<plugins>/registry.terraform.io/JamesonRGrieve/tofu-vyos/<ver>/<os>_<arch>/terraform-provider-vyos`
and point a `.terraformrc` `provider_installation { filesystem_mirror {...} }` at it.

## License

AGPL-3.0-or-later.
