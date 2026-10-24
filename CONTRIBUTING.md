# Contributing

Tempest is a Go project. Build it from source and run the full check before you
open a pull request. Contributions are accepted under the MIT license.

## Build

```sh
CGO_ENABLED=0 go build -o bin/tempest ./cmd/harness
```

## Checks

```sh
make check   # build, examples, vet, tests, race detector
make smoke   # end-to-end run against the faux provider
```

The default tests need no credentials or network access. Integration and
contract tests run only with their build tags.

## Pull requests

- Keep the change focused and add a test for behavior it adds or fixes.
- Run `gofmt` on changed files and match the surrounding style.
- Update the [guide](guide/README.md) when behavior or configuration changes.
- Say what changed and how you checked it.

## Reporting bugs

Use the issue templates and include the Tempest commit, your Go version, and
the command that failed.
