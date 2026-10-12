---
sidebar_position: 6
---

# Moving from `github setup` to `repos install`

`fullsend github setup <owner/repo>` configures one GitHub repository
from command-line flags. On a rerun it edits the existing
`.fullsend/config.yaml` in place. `fullsend repos install` works
differently. It converges every selected repository to a declarative
`repos.yaml` manifest, which is the source of truth for both the managed
`.fullsend/config.yaml` and the optional `.fullsend/config.base.yaml`
preset ([ADR 0122](../../ADRs/0122-declarative-repo-configuration.md)).

So a setup script cannot be translated flag for flag. Some values become
manifest edits, some stay install-time inputs, and a few setup flags
have no install equivalent at all. This page maps every
configuration-related `github setup` input to its `repos install`
destination. It then walks through moving a repository in each of the
four starting states.

This page covers where each value is declared. Two related topics are
tracked separately and are not covered here: deriving the `FULLSEND_*`
repository variables and secrets from layered configuration
([#8220](https://github.com/fullsend-ai/fullsend/issues/8220)), and the
general precedence and validation rules between CLI flags and layered
configuration ([#7301](https://github.com/fullsend-ai/fullsend/issues/7301)).

## What changes

| | `github setup` | `repos install` |
|---|---|---|
| Source of configuration | Command-line flags | `repos.yaml` |
| `.fullsend/config.yaml` | Generated once, then edited key by key by later runs | Managed: rendered from `repos.yaml` with an ownership marker and rewritten as a whole file on every convergence |
| `.fullsend/config.base.yaml` | Written from `--config` | Written from `config_base.source`; with none declared, an existing file blocks install |
| Runtime selection | Interactive prompt on a terminal, or `--runtime` | `config.runtime` in `repos.yaml`; no prompt and no flag |
| Inference method | Implied by the flags passed | Required `inference.auth` selection in `repos.yaml` |
| Rerun | Keeps the existing overlay unless a config flag is passed | Converges to `repos.yaml`; direct edits to the managed file are drift and get reverted |

## Conversion table

**Destination** names where the value lives with `repos install`:

- **Managed config**: a key in a `config` section, either `defaults.config`, `github.config`, or the repository entry's `config` (see [Choosing the scope](#choosing-the-scope)). It is written to `.fullsend/config.yaml`.
- **Preset**: `config_base.source` / `config_base.sha256`, under `defaults` or the repository entry. It is written to `.fullsend/config.base.yaml`.
- **Manifest field**: a `repos.yaml` field outside `config`.
- **Install-time input**: a flag used during the run and never written to `repos.yaml`.

| `github setup` input | Destination with `repos install` | `repos install` flag | Notes |
|---|---|---|---|
| `<owner/repo>` | Manifest field: repository entry under `github.repos` | Positional argument (`--forge github` when the manifest cannot imply the forge) | Adding by argument writes the entry for you; you can also add it by hand. |
| `--runtime`, or the interactive runtime prompt | Managed config: `runtime` | None | `repos install --runtime` was removed, and the old sibling `runtime:` keys under `defaults` or an entry are rejected with the replacement path. Omit the key to keep the `claude` default. |
| `--agents` | Managed config: `roles` | `--roles` (renamed) | An explicit `--roles` is recorded as the entry's `config.roles` only while the repository has no `.fullsend/config.yaml` or its installation is pending. Otherwise edit `config.roles`. |
| `--mint-url` | Manifest field: `mint_url` under `github` or the entry, which sets the `FULLSEND_MINT_URL` variable. Optionally also managed config: `mint_url` | `--mint-url` | Setup wrote both destinations, but install keeps them separate. Without a manifest `mint_url`, install converges `FULLSEND_MINT_URL` to the public mint (`github.mint_mode: public`). The flag is recorded only on entries the run adds or copies from a glob. |
| `--inference-provider` | Managed config: `inference.provider` | None | Only `vertex` is valid. |
| `--inference-project` | Install-time input: writes the `FULLSEND_GCP_PROJECT_ID` secret. Optionally also managed config: `inference.project` | `--vertex-project` (renamed) | An existing secret is reused when the flag is omitted. |
| `--inference-region` | Install-time input: writes the `FULLSEND_GCP_REGION` variable. Optionally also managed config: `inference.region` | `--vertex-region` (renamed) | Defaults to `global` when `--vertex-project` is passed. |
| `--inference-wif-provider` | Install-time input: writes the `FULLSEND_GCP_WIF_PROVIDER` secret. Optionally also managed config: `inference.wif_provider` | `--vertex-wif-provider` (renamed) | Without it, install derives the per-repo provider from the project. |
| *(implied by the flags)* | Manifest field: `inference.auth` (`vertex-wif`, `openai-api-key` or `openai-wif`) | `--inference-auth` | Required for every repository; there is no default. Use `vertex-wif` for a Vertex setup, `openai-wif` when the three OpenAI flags were used, and `openai-api-key` when the repository uses `FULLSEND_OPENAI_API_KEY`. |
| `--openai-audience`, `--openai-identity-provider-id`, `--openai-service-account-id` | Managed config: `inference.openai.audience`, `inference.openai.identity_provider_id`, `inference.openai.service_account_id` | None | Use with `inference.auth: openai-wif`. Repository variables `FULLSEND_OPENAI_*` still win when set; see [OpenAI Workload Identity](../infrastructure/openai-workload-identity.md). |
| — | Install-time input: writes `FULLSEND_OPENAI_API_KEY` | `--openai-api-key` | Setup has no such flag (setups used `fullsend github set`). The value is never written to `repos.yaml`. |
| `--inference-gateway-url`, `--inference-gateway-audience`, `--inference-gateway-auth` | Managed config: `inference.gateway.url`, `.audience`, `.auth` | None | To clear the block, remove the keys from `repos.yaml` instead of passing empty flags. In `api-key` mode the `FULLSEND_INFERENCE_GATEWAY_API_KEY` secret remains yours to set (`fullsend github set`). |
| `--inference-gateway-model id=api` (repeatable) | Managed config: `inference.gateway.models` (`<id>: {api: <api>}`) | None | |
| `--inference-gateway-models-file <local file>` | Managed config: `inference.gateway.models_file` (a path in the repository) | None | Install does not commit the file. Commit it to the repository yourself, for example as `.fullsend/inference-gateway.json`, or list the models inline instead. |
| `--config` | Preset: `config_base.source` | None | Valid under `defaults` or on the entry, not in the `github` section. A local path is resolved relative to `repos.yaml`. |
| `--config-hash` | Preset: `config_base.sha256` | None | Optional. `none` on an entry skips an inherited digest. |
| `--app-set` | Manifest field: `app_set` under `github` or the entry, which sets the `FULLSEND_APP_SET` variable | `--app-set` | Recorded on every selected GitHub entry, including already tracked ones. |
| `--fullsend-ref` | Manifest field: `fullsend_ref` under `github` or the entry | `--fullsend-ref` | Recorded only on entries the run adds or copies from a glob. Downgrading an installed ref also needs `--force`. |
| `--vendor`, `--fullsend-binary`, `--fullsend-source` | Manifest field: `vendor` under `defaults` or the entry. Binary and source are install-time inputs | `--vendor`, `--fullsend-binary`, `--fullsend-source` | `--vendor` is recorded on entries the run adds; on tracked entries it overrides the manifest for that run only. |
| `--signoff` | Manifest field: `signoff` under `defaults`, `github` or the entry | `--signoff` | The flag overrides the manifest for that run and is never written. |
| `--direct`, `--dry-run` | Install-time input | `--direct`, `--dry-run` | |
| — | Managed config: `allowed_remote_resources` | None | Setup had no flag. `repos install --allowed-remote-resources` was removed, and the old sibling key is rejected. |

`repos install` has no aliases for its removed `--runtime` and
`--allowed-remote-resources` flags, and no automatic migration for the
old `runtime` / `allowed_remote_resources` sibling keys. Move those
values under a `config` section as described in
[`fullsend repos`](../../cli/repos.md#runtime-and-allowed_remote_resources-moved-into-config-breaking).

## Choosing the scope

Each managed-config value can be declared in one of three sections:

| Section | Applies to |
|---|---|
| `defaults.config` | Every repository in the manifest, GitHub and GitLab |
| `github.config` | Every GitHub repository |
| A repository entry's `config` | That repository (or every repository a glob entry matches) |

For each field the effective value resolves, from lowest to highest
priority: code defaults, the `config_base` preset, `defaults.config`,
`github.config`, and then the entry's `config`. Scalars such as
`runtime` are replaced by the more specific layer, and nested maps such
as `inference` merge key by key. `allowed_remote_resources` follows its
own rule; see [Repo Management — Managed configuration](repo-management.md#managed-configuration).

Only declared values are written to `.fullsend/config.yaml`. An omitted
key is never materialized, so it keeps following the preset and the code
defaults. A declared value is always written, even when it equals the
inherited value, and is kept on every later install. Declare a value only
when the repository must keep it if the preset or the defaults change.

### Example: picking a non-default runtime

With `github setup`, choosing a runtime meant answering a prompt or
passing `--runtime pi`. With `repos install`, there is nothing to answer.
Leave `runtime` out to get the `claude` default. Declare it to pick
another runtime:

```yaml
github:
  repos:
    - name: acme/api
      config:
        runtime: pi      # this repository only
```

To move every GitHub repository, put it in `github.config` instead. A
repository entry that declares its own `runtime` still wins over that
value.

## Before you start

- **Inference method.** Decide each repository's `inference.auth` (see
  the table above), and set it under `defaults`, the `github` section, or
  the entry.
- **Mint.** If the repository uses a mint other than the public
  `https://mint.fullsend.sh`, set `github.mint_url` (or the entry's
  `mint_url`) before the first install, so install does not change
  `FULLSEND_MINT_URL`.
- **Workflow ref and vendoring.** If setup ran with `--fullsend-ref` or
  `--vendor`, declare `fullsend_ref` or `vendor: true` so convergence
  keeps them.
- **Credentials.** Existing `FULLSEND_GCP_*` and `FULLSEND_OPENAI_API_KEY`
  secrets are reused when no inputs are passed. Pass `--vertex-*` or
  `--openai-api-key` only to supply missing credentials or replace them.
- **Stop running `github setup` on the repository.** Once the repository
  is in `repos.yaml`, rerunning `github setup` with a config flag
  re-serializes `.fullsend/config.yaml` without its ownership marker, and
  the next `repos install` stops until the file is adopted again.

## Moving a repository

Every scenario uses the same safe order:

1. Edit `repos.yaml`.
2. Run `fullsend repos status --repo <owner/repo>` to read what install
   would change and whether a file needs adoption.
3. Run `fullsend repos install <owner/repo> --dry-run`.
4. Run `fullsend repos install <owner/repo>`.

`repos status` and `--dry-run` never write anything. When an existing
configuration file needs action, install stops before any manifest or
forge write: no variable, secret, scaffold file or PR is created.

### Fresh repository

A repository with no Fullsend files. Write the entry with the values your
setup script passed:

```yaml
version: 1
github:
  inference:
    auth: vertex-wif
  repos:
    - name: acme/api
      config:
        runtime: pi
```

```bash
fullsend repos status --repo acme/api
fullsend repos install acme/api --dry-run \
  --vertex-project "<gcp-project>" --vertex-wif-provider "<wif-provider-url>"
fullsend repos install acme/api \
  --vertex-project "<gcp-project>" --vertex-wif-provider "<wif-provider-url>"
```

`repos install acme/api --forge github --inference-auth vertex-wif`
also creates the entry (and `repos.yaml` itself, if missing). Add any
`config` values before the first install, or in a later edit.

On a first install, install records install-time choices on the entry so
the manifest reproduces the result. With no `config_base` preset and no
`create_issues` declaration, it adds
`config.create_issues.allow_targets.repos: [fullsend-ai/fullsend]`, and
it writes an explicit `--roles` value to `config.roles`. Review and
commit the resulting `repos.yaml` after the run:

```yaml
    - name: acme/api
      config:
        runtime: pi
        create_issues:
          allow_targets:
            repos:
              - fullsend-ai/fullsend
```

A rerun with the same manifest changes neither `repos.yaml` nor the
managed file.

### Repository already listed in `repos.yaml`

The repository already has an entry and a managed `.fullsend/config.yaml`.
Make every configuration change in the manifest, not with flags:

```yaml
    - name: acme/api
      config:
        runtime: codex
```

`repos status` reports the managed file as drift until install rewrites
it. These flags still update an already tracked entry:

| Flag | Effect on an already tracked repository |
|---|---|
| `--inference-auth` | Rewrites the entry's `inference.auth` (a glob-covered repository gets its own entry copied from the glob) |
| `--app-set` | Rewrites the entry's `app_set` |
| `--roles` | Recorded as `config.roles` only while the repository has no `.fullsend/config.yaml` or its installation is pending; otherwise edit `config.roles` |
| `--mint-url`, `--fullsend-ref` | Not recorded on an explicit entry; edit `mint_url` or `fullsend_ref`. A glob-covered repository gets an entry copied from the glob that carries the value |
| `--vendor`, `--signoff` | Override the manifest for this run only |
| `--vertex-*`, `--openai-api-key` | Install-time credentials; never written to `repos.yaml` |
| `--direct`, `--dry-run`, `--force`, `--concurrency` | This run only |

Settings with no install flag (`runtime`, `allowed_remote_resources`, the
OpenAI identifiers, gateway settings and the preset) can only be changed in
`repos.yaml`.

### Existing `.fullsend/config.yaml` without the ownership marker

A repository installed with `github setup` has an overlay without the
`repos-config-v1` marker. Install never overwrites such a file. It stops
with `adoption required` before any write, including a dry run.

1. Add the entry to `repos.yaml` with the manifest fields from
   [Before you start](#before-you-start). Leave `config` empty for now.

2. Read the adoption guidance:

   ```bash
   fullsend repos status --repo acme/api
   ```

   The repository is reported as `managed configuration (adoption required)`.
   The output (table, and the JSON `detail` field with `--json`) contains
   a proposed `repos.yaml` entry that carries every setting the existing
   file declares. It also shows a setting-level difference between the
   existing file and the managed file, and the resulting change in effective
   configuration.

3. Copy the settings you want to keep into the entry's `config`, or into
   `github.config` / `defaults.config` when they apply more widely. Leave
   out a setting only if you accept losing it. An omitted setting falls back
   to the preset and code defaults. For example, if the proposed entry
   shows `runtime: pi` and `roles: [triage, review]` among other keys,
   keeping those two looks like this:

   ```yaml
       - name: acme/api
         config:
           runtime: pi
           roles:
             - triage
             - review
   ```

4. Adopt the file in the repository, in a PR or a direct commit:
   - add the two marker lines at the top of `.fullsend/config.yaml`:

     ```yaml
     # This file is managed by 'fullsend repos': repos-config-v1
     # Do not edit this file directly; changes will be overwritten.
     ```

     Agents keep running with the existing settings until install
     rewrites the file; or
   - delete `.fullsend/config.yaml`. Until install writes the managed
     file, agents run with only the base layer and code defaults.

5. Run `repos status` again. It now reports ordinary drift, or none.
   Then run `repos install acme/api --dry-run` and `repos install acme/api`.
   Install rewrites the file from `repos.yaml`.

Install rejects the rewrite when the new effective configuration would be
less restrictive than the current one and `repos.yaml` does not declare
that relaxation. The compared keys are `kill_switch`, `roles`,
`allowed_remote_resources`, agent `enabled: false` suppressions,
`create_issues.allow_targets`, `inference.gateway.url` and
`inference.gateway.audience`. The output names the affected keys. Copy the
value into the manifest, or declare the less restrictive value explicitly.

### Existing `.fullsend/config.base.yaml` (preset from `github setup --config`)

When no preset is declared for the repository, an existing base file is
drift. `repos status` reports it, and install stops before any write
until you declare a preset or remove the file. Install never deletes it.

Declare the preset that produced the file. Use the source you passed to
`--config` and the digest you passed to `--config-hash`:

```yaml
version: 1
defaults:
  config_base:
    source: https://example.com/presets/org.yaml
    sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
github:
  inference:
    auth: vertex-wif
  repos:
    - name: acme/api
      config:
        runtime: pi        # was --runtime pi alongside --config
```

If the fetched preset matches the installed file byte for byte, install
leaves the file alone. A different preset replaces the whole base file,
after the same safety comparison as above. `github setup --config` also
wrote a `.fullsend/config.yaml` without the marker: either the setup
flags passed alongside `--config`, or a commented stub when there were
none. That file needs adoption as described in the previous section.
Both checks run together, and one `repos status` run reports both.

The `runtime: pi` declaration above resolves above the preset's own
`runtime`. Other keys the preset sets, and the entry does not declare,
stay inherited from the preset.

## Changing values later

All later changes go through `repos.yaml`:

1. Edit the value at the scope you want.
2. Run `fullsend repos status` to see the managed-file drift.
3. Run `fullsend repos install` to rewrite `.fullsend/config.yaml`.

A direct edit to a managed `.fullsend/config.yaml` is reported as drift
and reverted by the next convergence. Repeated installs with an unchanged
manifest change nothing.

## Inspecting the manifest and the managed files

- `repos.yaml`: review `git diff repos.yaml` after each install, since a
  first install can add `config.create_issues` and `config.roles`.
- Planned changes: `fullsend repos install --dry-run` lists what would be
  written. `fullsend repos status --repo <owner/repo> --json` reports
  drift and adoption details per repository.
- Installed files: read `.fullsend/config.yaml` and
  `.fullsend/config.base.yaml` on the default branch, or in the open
  initialization PR. With the `gh` CLI:

  ```bash
  gh api repos/acme/api/contents/.fullsend/config.yaml -H 'Accept: application/vnd.github.raw'
  ```

## See also

- [Repo Management](repo-management.md) — manifest structure, presets, and managed configuration
- [`fullsend repos`](../../cli/repos.md) — command and flag reference
- [`fullsend github`](../../cli/github.md) — `github setup` flag reference
- [Config Reference](../../reference/config-reference.md) — every `config` field
