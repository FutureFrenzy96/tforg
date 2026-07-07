# tforg — fast Terraform formatter + file organizer

`tforg` does two things to a Terraform codebase, very fast:

1. **Formats** every `.tf` file with output byte-identical to `terraform fmt`
   (it uses HashiCorp's own `hclwrite` library — the same code that powers
   `terraform fmt` — so there is no external binary dependency and no drift).
2. **Organizes** top-level blocks into their conventional files within each
   directory, creating the files when they don't exist:

   | Block type           | File           |
   | -------------------- | -------------- |
   | `resource`, `module` | `main.tf`      |
   | `data`               | `data.tf`      |
   | `variable`           | `variables.tf` |
   | `output`             | `outputs.tf`   |
   | `locals`             | `locals.tf`    |
   | `provider`           | `providers.tf` |
   | `terraform`          | `versions.tf`  |
   | `moved`              | `moved.tf`     |
   | `import`             | `imports.tf`   |
   | `removed`            | `removed.tf`   |
   | `check`              | `checks.tf`    |
   | `ephemeral`          | `ephemeral.tf` |

Blocks are moved as verbatim text (leading comments travel with their block),
then formatted. Blocks only ever move between files in the **same directory**,
since a directory is the module boundary. Source files left empty by the moves
are deleted. On a benchmark of 1,000 files across 100 module directories,
a full run takes ~50ms and a no-op verification ~25ms.

## Install

```sh
go build -ldflags="-s -w" -o tforg .   # local build
# or
go install .                            # puts tforg on your GOPATH/bin
```

## Usage

```sh
tforg                    # format + organize the current directory, recursively
tforg path/to/repo       # ... a specific directory
tforg modules/vpc/x.tf   # ... a single file (blocks move to siblings in its dir)
tforg -staged            # ... the .tf files currently staged in git
tforg -check .           # report what would change, write nothing (CI-friendly)
tforg -diff .            # unified diff of pending changes (implies -check)
tforg -sort .            # also alphabetize variable/output blocks
tforg -fmt-only .        # formatting only, no block moves
tforg -quiet .           # errors only
tforg -no-color .        # plain output (NO_COLOR / CLICOLOR_FORCE also honored)
tforg -map terraform=terraform.tf,module=modules.tf .   # override destinations
tforg -version           # print version
tforg install-hook       # write .git/hooks/pre-commit for the current repo
```

Output is grouped per directory and color-coded — each conventional file has
its own color (green `main.tf`, yellow `variables.tf`, blue `data.tf`, ...),
`+`/`-`/`~` mark created, deleted, and reformatted files, and a summary line
shows the total and elapsed time:

```
example
  everything.tf → versions.tf   terraform
  everything.tf → variables.tf  variable "region"
  + variables.tf  created
  - everything.tf  deleted (empty)

✓ fixed 12 files in 2 directories · 6ms
```

Colors turn off automatically when output is piped (CI logs stay clean) and
under `NO_COLOR`; set `CLICOLOR_FORCE=1` to keep them when a hook runner
captures output.

Exit codes: `0` nothing to do · `1` changes were made (or are needed, with
`-check`) · `2` error (e.g. a file that does not parse).

## Configuration: `.tforg.hcl`

Some blocks legitimately belong outside the default convention — say a module
that only loads data sources, which you want in `data.tf` rather than
`main.tf`. Rather than annotating your Terraform with comments, declare the
exception once in a `.tforg.hcl` at the repo root:

```hcl
place "module" "network_data" {
  file = "data.tf"
}
```

Rules are checked before the type mapping and the first match wins; anything
unmatched follows the normal convention. The second label is the block's name
(labels joined with `.` for two-label blocks, so `aws_instance.web` addresses
a specific resource) and may be a glob (`data_*`) if you later adopt a naming
convention. Two other forms are supported:

```hcl
place "module" "legacy" {
  keep = true            # leave it wherever it currently lives
}

map {
  terraform = "terraform.tf"   # change a type's default destination
}
```

The nearest `.tforg.hcl` — in the target directory or any parent — applies,
so one file at the repo root covers every nested module. `-config path`
points at an explicit file, `-no-config` ignores config files entirely, and
CLI flags always win (`-map 'module:network_data=data.tf'` is the one-off
equivalent of a `place` rule). Placement stays fully deterministic and
idempotent: it depends only on your code and config, never on run history.

## Git pre-commit hook

From inside any repo, run:

```sh
tforg install-hook
```

This writes a `.git/hooks/pre-commit` (respecting `core.hooksPath`; add
`-force` to overwrite an existing hook) that runs `tforg -staged`: only the
staged `.tf` files are checked, and if anything is rewritten the commit is
aborted so you can review and re-stage. The same script lives at
[hooks/pre-commit](hooks/pre-commit) if you prefer to copy it manually.

Note for partial staging (`git add -p`): like any formatter hook, `tforg`
rewrites the working-tree file, so staged and unstaged hunks of the same file
are formatted together.

If you use the [pre-commit](https://pre-commit.com) framework, this repo ships
a [.pre-commit-hooks.yaml](.pre-commit-hooks.yaml), so once the repo is pushed
somewhere you can reference it:

```yaml
repos:
  - repo: https://your.git.host/you/tforg
    rev: v0.1.0
    hooks:
      - id: tforg
```

## Behavior details

- **Duplicate detection**: two blocks with the same address in one module
  (`variable "region"` defined twice, identical resource addresses, ...) are
  reported as errors (exit `2`) and nothing is moved — catching at commit time
  what Terraform would only surface at plan time. Provider aliases, repeated
  `terraform`/`locals` blocks, and override files are exempt, as Terraform
  allows those.
- **Sorting** (`-sort`, opt-in): variable and output blocks are alphabetized
  within their files. A file is skipped when reordering would be unsafe (mixed
  block types, or standalone comments between blocks). Resources are never
  reordered.
- **Atomic writes**: files are written via temp-file-and-rename, so an
  interrupted run can never leave a truncated `.tf` behind.
- **Comments**: comment lines directly above a block move with it; comments
  separated from a block by a blank line stay where they are. A comment on the
  closing-brace line travels with the block.
- **Heredocs and strings** are never confused for block boundaries — the real
  HCL lexer decides what is code and what is a comment.
- **Override files** (`override.tf`, `*_override.tf`) are formatted but their
  blocks are never moved, preserving Terraform's override merge semantics.
- **Unknown block types** stay where they are.
- **Files that fail to parse** are reported (exit `2`) and left untouched, and
  no blocks are moved into an unparseable destination.
- **`.tf.json`**, `.terraform/`, `.git/`, and hidden directories are skipped.
- **Idempotent**: running twice always yields "nothing to do".
