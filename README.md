# LGTM

[![CI](https://github.com/Temikus/lgtm/actions/workflows/ci.yml/badge.svg)](https://github.com/Temikus/lgtm/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Score pull requests with System One decision models.

LGTM asks a decision model such as TypeSafe's Jev (`typesafe/jev-1.13`) a set of typed questions about a PR: is the diff aligned with its description, scoped, safe, tested? Each answer is a 0–1 value. LGTM combines them into one verdict, **approve**, **deny**, or **needs-human**, which you can gate a merge on. A typical PR costs a fraction of a cent.

LGTM doesn't write review comments, approve PRs, or merge. It's a mistake detector, not a security boundary: run it alongside required human reviewers, never instead of them.

> **Status:** pre-release, under active development. See [design/PRD.md](design/PRD.md).

## Install

Requires an [OpenRouter](https://openrouter.ai) API key. Create a dedicated key with a spend limit.

```sh
go install github.com/Temikus/lgtm/cmd/lgtm@latest
```

## Usage

```sh
lgtm version
```

More commands (`score`, `action`, `init`, `replay`, `config validate`) are coming. See the PRD.

## Data

LGTM sends PR content (title, description, linked issue titles, and diffs) to the model provider. Paths matched by `exclude` are never sent. On OpenRouter, turn on the data-policy setting that refuses providers which train on inputs.

## Development

Uses [mise](https://mise.jdx.dev) for tools and [just](https://just.systems) for tasks.

```sh
mise install
just check                 # lint + unit tests
just test integration      # live Jev API; needs OPENROUTER_API_KEY
```

## License

[Apache-2.0](LICENSE)
