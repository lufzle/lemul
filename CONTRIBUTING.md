# Contributing

lemul is **alpha**. The tree changes quickly. Read the root [README](README.md)
before sending a change that touches tenancy, the relay split, or credentials.

## License

Contributions are accepted under the [GNU Affero General Public License v3.0](LICENSE).
By opening a pull request you license your work under AGPL-3.0.

## Development

You need Go (see `go.mod`), Docker (for store / management-API / e2e tests),
and Make.

```bash
make test          # go test -race -p 1
make lint          # golangci-lint
make check-oss-tree
```

The console lives in `console/` (Bun). Identity for local runs is `auth-stack/`.
Vendor deploy is `deploy/`; the customer AWS module is `terraform/`.

## Pull requests

- Keep the change scoped. Do not mix refactors with behaviour.
- Tests that exercise the shipped path beat comments that describe it.
- Do not commit credentials, `.state/`, or `.env` files.
- Run `make test` and `make lint` when you can; CI will run them on the PR.

Open an [issue](https://github.com/lufzle/lemul/issues) for discussion if the
change is architectural.

## Security

See [SECURITY.md](SECURITY.md). Do not report vulnerabilities in public issues.
