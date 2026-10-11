# Contributing

## Layout

See the repository layout in the [README](README.md) and the file map in [`daemon/doc.go`](daemon/doc.go). The design is in [docs/architecture.md](docs/architecture.md).

## Build and test

```sh
cd daemon
gofmt -l .                 # must print nothing
go vet ./... && GOOS=freebsd go vet ./...
go test -race ./...        # unit tests: pairing protocol, state, sessions, features
```

Package: [docs/building.md](docs/building.md). GUI and script parts have no unit tests; CI lints PHP, XML, JavaScript and shell, and every change to them must be tried on a lab node (see [lab/README.md](lab/README.md)).

## Rules that keep it safe

- **Never trust the peer or the GUI input.** Validate names, sizes and formats where data enters (Go handlers, PHP controllers). No shell strings: use `exec.Command` with arguments and configd actions with positional parameters.
- **State changes must be crash-safe**: write through the durable helpers (temp file, fsync, rename), and fail safe (standby, silent) when in doubt.
- **A standby must never talk with the shared IP/MAC.** Anything that brings interfaces up belongs in the promote path only.
- **Anything that changes the failover path needs a lab measurement** (planned and power-cut) and a line in [docs/validation.md](docs/validation.md).
- Keep dependencies at zero (standard library only).

## Commits and releases

Small, focused commits with a message that says why. Releases are tags `vX.Y.Z`; CI builds the package, checksums and the GitHub release. Update [CHANGELOG.md](CHANGELOG.md) with every user-visible change.
