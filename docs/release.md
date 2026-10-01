# Releases and published images

A release of this repo is a git tag `vX.Y.Z` on `main`. Pushing the tag runs
[`.github/workflows/release.yaml`](../.github/workflows/release.yaml), which
publishes everything a user needs from this repo:

| artifact | where | how to reference it |
|---|---|---|
| `ate-env-api` image | `ghcr.io/agent-substrate/env/ate-env-api` | by digest, from the release notes or `images_<tag>.txt` |
| `ate-env-guest` image | `ghcr.io/agent-substrate/env/ate-env-guest` | by digest, same places |
| `ate-env` CLI | release assets `ate-env_<tag>_<os>_<arch>` for linux and darwin, amd64 and arm64 | download, `chmod +x` |
| checksums | `SHA256SUMS_<tag>.txt` | `sha256sum -c` |

Images are multi-platform (linux/amd64, linux/arm64), tagged with the release
tag and `latest`, and signed with cosign keyless signing bound to this
repository's GitHub Actions identity.

## What a release binary knows

The `ate-env` binaries attached to a release are built after the images, with
the two digests baked in. `ate-env manifest` defaults `--api-image` to the
release's `ate-env-api`, and `ate-env manifest template` defaults
`--guest-image` to the release's `ate-env-guest`. `ate-env --version` prints the
tag. A build from source (`go install ...@latest`, `go build`) has no defaults
and asks for both flags. `make build-cli` reproduces a release-style binary
locally from `ATE_ENV_API_IMAGE` and `ATE_ENV_GUEST_IMAGE`.

The worker image (`--worker-image`) is not published here: it is
`ateom-gvisor` from the [Substrate repo](https://github.com/agent-substrate/substrate)
and must match the Substrate version deployed on the cluster. Until Substrate
publishes images, build it from that repo with `ko` at the matching version.

## Pin by digest

Always deploy images by digest, never by tag. Substrate requires it for
ActorTemplates because a changed image invalidates snapshots, and the `latest`
tag moves with every release. The release notes and `images_<tag>.txt` carry the
digests; the CLI defaults are digests.

## Verify a signature

```bash
cosign verify \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/agent-substrate/env/' \
  ghcr.io/agent-substrate/env/ate-env-guest@sha256:<digest>
```

A valid signature proves the image was built by this repository's release
workflow from the tagged commit.

## Cutting a release

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The workflow creates the GitHub release with generated notes, builds and pushes
the images, signs them, builds the CLI for four platforms, uploads the assets,
and appends an `## Images` section with the digests and the verify command to
the notes. To republish an existing tag (a failed run, a workflow fix), start
the workflow by hand from the Actions tab with the tag as input; it replaces the
assets and the images section rather than duplicating them. A republish
produces new digests, since the images are rebuilt.

## First-time setup for the organization

GitHub Container Registry packages are created on the first push by the
workflow's `GITHUB_TOKEN`, under the repository. Once:

1. Allow GitHub Actions to create packages for the organization (organization
   settings, Packages).
2. After the first release, open each package (`ate-env-api`, `ate-env-guest`)
   and confirm it is public and linked to this repository, so anonymous pulls
   and the repository's permissions apply.

## Reproducibility notes

- The guest's base image is pinned by digest in [`.ko.yaml`](../.ko.yaml). It
  is a `bash` image so that templates whose container is the guest itself have
  a shell for `shell()`; for task-image templates only the static binary is
  used. Bump the pin deliberately and mention it in the release notes.
- The API image uses ko's default distroless base.
- `make images` builds the same two images for a registry of your choice
  (`ATE_ENV_IMAGE_REPO`), for development against a private cluster; the
  released images are the ones to point customers at.
