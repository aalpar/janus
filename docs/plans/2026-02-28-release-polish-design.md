# Release Polish Design

**Date:** 2026-02-28
**Status:** Approved

## Goal

Prepare Janus for public announcement: lower the barrier from "clone and
build" to "helm install" or "kubectl apply".

## Deliverables

### 1. VERSION file

Single source of truth for the release version. Read by the Makefile,
GoReleaser, and CI workflows. Current value: `v0.0.1`.

### 2. .goreleaser.yml

Builds the `janus` CLI binary for four platforms (darwin/linux ×
amd64/arm64). Archives include LICENSE and README.md. Uses
GitHub-native changelog.

### 3. Release workflow

`.github/workflows/release.yml` — triggered on `v*` tag push:

1. CI gate (`make cd`)
2. GoReleaser → CLI binaries attached to GitHub Release
3. Docker buildx → multi-arch image pushed to `ghcr.io/aalpar/janus`
4. Helm chart packaged and attached to GitHub Release

### 4. Makefile targets

| Target | Purpose |
|--------|---------|
| `ci` | lint + build + test |
| `cd` | build + test (pre-release) |
| `tag` | annotated tag from VERSION |
| `bump-major/minor/patch` | bump VERSION |
| `release-check` | validate .goreleaser.yml |
| `release-snapshot` | local GoReleaser dry run |
| `release` | full GoReleaser release |
| `helm-lint` | lint Helm chart |
| `helm-package` | package chart to dist/ |
| `helm-template` | dry-run template rendering |

### 5. Helm chart

Minimal chart in `chart/`:

- CRDs via `crds/` directory
- Deployment, ServiceAccount, ClusterRole, ClusterRoleBinding
- Webhooks + cert-manager Certificate (togglable, default on)
- Configurable: image, replicas, resources, namespace

### 6. CONTRIBUTING.md

Prerequisites, dev setup, build/test commands, commit conventions, PR
process. Adapted from Wile's contributor guide.

### 7. GitHub templates

- Bug report (`.github/ISSUE_TEMPLATE/bug_report.yml`)
- Feature request (`.github/ISSUE_TEMPLATE/feature_request.yml`)
- Template config (`.github/ISSUE_TEMPLATE/config.yml`)
- PR template (`.github/pull_request_template.md`)

### 8. README update

Replace manual docker-build install instructions with Helm install
(primary) and pre-built ghcr.io image reference.

### 9. CI workflow tightening

Restrict existing lint/test/e2e triggers to master branch pushes and
PRs (currently fires on all branches).

### 10. Utility scripts

`tools/sh/bump-version.sh` for semantic version bumping.
