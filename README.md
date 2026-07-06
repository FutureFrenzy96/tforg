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
tforg -check .           # report what would change, write nothing (CI-friendly)
tforg -fmt-only .        # formatting only, no block moves
tforg -quiet .           # errors only
tforg -map terraform=terraform.tf,module=modules.tf .   # override destinations
```

Exit codes: `0` nothing to do · `1` changes were made (or are needed, with
`-check`) · `2` error (e.g. a file that does not parse).

## Git pre-commit hook

Copy [hooks/pre-commit](hooks/pre-commit) into your repo's `.git/hooks/`
directory (and `chmod +x` it), or point `core.hooksPath` at a shared hooks
directory. The hook runs `tforg` on the staged `.tf` files only; if anything
was rewritten it aborts the commit so you can review and re-stage:

```sh
cp hooks/pre-commit /path/to/your/repo/.git/hooks/pre-commit
chmod +x /path/to/your/repo/.git/hooks/pre-commit
```

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
