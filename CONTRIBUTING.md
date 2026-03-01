# Contributing to Janus

Thank you for your interest in contributing to Janus! This document provides guidelines for contributing to the project.

## Getting Started

### Prerequisites

- Go 1.25 or later
- Git
- Docker (for building container images)
- A Kubernetes cluster (Kind works well for development)
- kubectl v1.30+

### Finding Work

1. Browse [open issues](https://github.com/aalpar/janus/issues)
2. Look for issues labeled:
   - `good-first-issue` — Great for newcomers
   - `help-wanted` — High-priority items needing contributors
3. Read the issue description and linked design docs (in `docs/plans/`) for context
4. Comment on the issue to claim it or ask questions

### Development Setup

```bash
# Clone the repository
git clone https://github.com/aalpar/janus.git
cd janus

# Build CLI and controller
make build

# Install CRDs into a cluster
make install

# Run tests
make test

# Run the CLI
./bin/janus --help
```

## Development Workflow

### 1. Create a Feature Branch

**Never commit directly to `master`.** All changes must go through feature branches and pull requests.

```bash
git checkout -b feature/descriptive-name

# Examples:
#   feature/dry-run-support
#   fix/rollback-conflict-detection
#   docs/helm-chart-guide
```

### 2. Make Your Changes

**Critical Rules:**
- **Run `make lint` before committing** — must pass with 0 issues
- **All tests must pass** (`make test`)
- **Use project error types** — `ResourceOpError`, `ErrConflictDetected`, etc. No `fmt.Errorf` in production code.
- **Wrap errors with context at boundaries** — `&ResourceOpError{Op: "fetching", Ref: ref.String(), Err: err}`

**Key Patterns:**
- Read code before editing — understand existing patterns
- Use `errors.Is`/`errors.As` for error comparisons (never `==`)
- Prefer standard library over new dependencies

### 3. Write Tests

- Controller tests use Ginkgo v2 + envtest (`internal/controller/transaction_controller_test.go`)
- E2E tests run against Kind clusters (`test/e2e/`)
- Run tests with: `make test`
- Run e2e tests with: `make test-e2e`

### 4. Code Quality

Before committing:

```bash
# Format and lint (REQUIRED)
make lint

# Run all tests (REQUIRED)
make test

# Full CI validation (optional but recommended)
make ci
```

### 5. Commit Your Changes

```bash
# Stage only related files
git add <specific-files>

# Commit with conventional commit message
git commit -m "feat: short description

Longer explanation of what changed and why."
```

**Commit prefixes:**
- `feat:` — New feature
- `fix:` — Bug fix
- `refactor:` — Code restructuring without behavior change
- `docs:` — Documentation only
- `test:` — Adding or fixing tests
- `chore:` — Maintenance tasks

### 6. Push and Create a PR

```bash
git push -u origin feature/descriptive-name

gh pr create --title "feat: short description" --body "
## Summary
What this PR does.

## Changes
- Change 1
- Change 2

## Test Plan
- [ ] Tests pass
- [ ] Manual verification
"
```

### 7. Wait for CI

**Do not merge until all CI checks pass.** The PR will automatically run lint, unit tests, and e2e tests.

## Architecture Overview

Janus is a Kubernetes operator implementing the Saga pattern for atomic
multi-resource changes. Two CRDs: Transaction (orchestrator) and
ResourceChange (individual mutation).

### Key Packages

| Package | Purpose |
|---------|---------|
| `api/v1alpha1/` | CRD definitions + webhook validators |
| `internal/controller/` | State machine, reconciliation |
| `internal/lock/` | Advisory locking via Leases |
| `internal/rollback/` | Rollback ConfigMap schema |
| `internal/impersonate/` | SA impersonation client factory |
| `cmd/controller/` | Controller manager entry point |
| `cmd/janus/` | CLI: create, add, seal, recover |

### Essential Reading

- **[User Guide](docs/USER_GUIDE.md)** — Full walkthrough
- **[Design](docs/DESIGN.md)** — Architecture and state machine
- **[Invariants](docs/INVARIANTS.md)** — Safety and liveness guarantees
- **`docs/plans/`** — Design documents for features

## Contribution Guidelines

### What We're Looking For

- **Bug fixes** — Especially edge cases in rollback or conflict detection
- **Test coverage** — New scenarios for the controller test suite
- **Documentation** — Examples, guides, improved CRD field descriptions
- **Observability** — Metrics, events, logging improvements
- **CLI improvements** — New commands, better output formatting

### What to Avoid

- Large refactorings without prior discussion (open an issue first)
- New dependencies without justification (prefer standard library)
- Changes to the CRD API without a design doc

### Code Review Process

1. Maintainer reviews your PR (typically within a few days)
2. Address feedback by pushing new commits (don't force-push)
3. Once approved and CI passes, maintainer merges
4. Your branch is automatically deleted after merge

## Getting Help

- **Questions about an issue?** Comment on the issue
- **Found a bug?** Open an issue with reproduction steps
- **Want to propose a feature?** Open an issue for discussion first

## License

By contributing to Janus, you agree that your contributions will be licensed under the Apache License 2.0 (see LICENSE file).
