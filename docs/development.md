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
goreleaser release --snapshot --clean
```

Pushing a `v*` tag runs the GitHub release workflow. GoReleaser builds the
frontend, compiles the Linux binary, creates the archive and checksum file, and
publishes a GitHub release.
