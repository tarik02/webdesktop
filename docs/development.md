# Development

## Development shell

Enter the Nix environment:

```bash
nix develop
```

The frontend must exist before a direct Go build because `web/assets.go`
embeds `web/dist`:

```bash
cd web
pnpm install --frozen-lockfile
pnpm format:check
pnpm lint
pnpm typecheck
pnpm build
cd ..
go build ./...
```

Run the repository checks with:

```bash
golangci-lint run
nix flake check path:. --print-build-logs
git diff --check
```

The flake builds the frontend and service, runs Go vet, validates the embedded
assets, and checks the systemd unit and desktop entry.

## Releases

Validate the GoReleaser configuration and build a local snapshot:

```bash
goreleaser check
goreleaser release --snapshot --clean --skip=publish
```

Pull request titles must follow Conventional Commits. Release Please uses those
titles to create or update the stable release pull request. `fix` produces a
patch release, `feat` produces a minor release, and a breaking change produces a
major release.

Merging the release pull request creates the stable tag and GitHub release.
GoReleaser then builds the frontend and Linux binary and uploads the archive and
checksum file.

Every other push to `master` publishes a prerelease with a tag in this format:

```text
v0.0.0-nightly.<run-number>.<commit-sha>
```

Add the `build-binaries` label to a pull request to generate snapshot artifacts
without publishing a release.
