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

## Device- and environment-specific code

Code that exists only because of a specific device, architecture, firmware or environment must be marked on the line above it:

```
DEVICE-FIX(<scope>): <what breaks without this>
```

- `<scope>` is one of: `arm64`, `x86_64`, `uno-q`, `jetson`, `msm`, `nvidia`, `intel`, `amd`, `no-tpm`, `vm`, `u-boot`, `uefi`. A new scope is added to this list in the same pull request that first uses it.
- The text states what fails on that device without the code, not what the code does.
- Use the comment syntax of the file's language.
- This covers driver names, hardware probes, architecture constants, firmware quirks, boot-chain differences and anything gated on a specific environment.
- A pull request that adds or changes such code without the marker is not merged.
- `git grep -n "DEVICE-FIX("` lists every marker in the repository.
