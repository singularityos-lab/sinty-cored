# Contributing to sinty-cored

## Quick Start

```bash
git clone https://github.com/singularityos-lab/sinty-cored
cd sinty-cored
go build ./...
go test ./...
```

## Guidelines

- Keep changes small and tested. Anything that touches pairing, the session
  keys or the trusted list needs a test that tries to get around it: a wrong
  code, a revoked screen, a replayed or edited frame.
- Never log or return a pairing code or key material.
- Match the surrounding style.
- By submitting a contribution you agree to the [CLA](CLA.md).

## License

GPL-3.0-only.
