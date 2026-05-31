# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Real `actions/runner --jitconfig` runtime executing in-VM.

### Changed

- CI image workflow only publishes on tag or manual dispatch (not on push to main).

## [v0.1.0] — 2026-05-30

### Added

- Initial skeleton: Go module, CLI, runner package boundaries.
- Real REST integration — `Register` mints a token, `Run` pools ephemeral microVMs.
- OCI runtime image for the in-VM `actions/runner`.
- CI: build + test on push/PR across linux amd64+arm64 matrix.
- REST shim unit tests backed by `httptest` server.
