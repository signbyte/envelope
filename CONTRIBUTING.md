# Contributing

Thank you for considering a contribution. Bug reports, fixes and improvements are welcome. For
anything that could be exploited, use the private route in [SECURITY.md](SECURITY.md) — never a
public issue.

For anything larger than a small fix, please open an issue first and describe what you want to
change and why. This service is the durable record of who must sign what and in what order, so a
change that fights its design is better redirected before it is written than after.

## Building and testing

You need the Go toolchain at the version named in [go.mod](go.mod). Every dependency is public, so
nothing needs credentials, a `GOPRIVATE` setting or a vendor directory. The gate a change must pass
is the same one CI runs:

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...
```

Three more checks run in CI and are worth running before you push:

- **Lint** — `golangci-lint run`, at the version pinned in
  [.github/workflows/ci.yml](.github/workflows/ci.yml); the repo's [.golangci.yml](.golangci.yml)
  carries the configuration.
- **Vulnerabilities** — `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`.
- **The image** — CI builds it, generates an SBOM, fails on HIGH/CRITICAL findings from a
  vulnerability scan, and signs it. Build the [Dockerfile](Dockerfile) locally before you push if
  you touched it, because that job is slow to fail.

The committed tree must already be tidy: CI runs `go mod tidy -diff` and fails if it would change
anything, so run `go mod tidy` after touching dependencies. All Go code is `gofmt`-formatted, and
`.gitattributes` pins Go files to LF line endings — leave that alone, it keeps the tidy-diff gate
stable across platforms.

The tests need no database and no collaborator service: there is an in-memory store and the
document and signing surfaces are behind interfaces. If a change makes a test need a live system,
that is a design signal worth raising in the issue rather than solving with a fixture.

## What a change to this service needs

Read the **Security invariants** and **The state machine** sections of the [README](README.md)
first. Each invariant is the reason a specific class of defect cannot happen, and a change that
weakens one is the change, not a side effect.

The four that carry the most weight:

- **Every transition goes through the compare-and-set.** A state change applies only if the
  envelope version still matches what the caller read, then bumps it. A write that skips it can
  lose a decline or double a completion under two concurrent actors — and the owner and the signing
  callback genuinely are concurrent. New transitions use the same path; they do not get their own.
- **Order is checked twice, on purpose.** A sequential slot is eligible only once every
  lower-ordered slot is signed, at the eligibility precondition *and* again as slots sign. Keep both.
  A single check is a race, not a simplification.
- **The callback is idempotent.** A replayed slot-signed callback is a no-op success so the signing
  service can retry safely. Anything that makes a replay do work needs to explain why it cannot
  double a signature.
- **Owner and participant filtering is not optional, and 404 is the answer.** Reads and writes are
  scoped to the owner or to an invited signer matched on the authenticated identity claim — never a
  header. A miss returns 404 so absence and no-access are indistinguishable; a more helpful error is
  an enumeration oracle.

**The signer-slot limit is a deliberate edition boundary, not a tunable.** It is a named constant,
enforced in the service *and* in the data layer, and it is package-private and indirected only so
this package's own tests can exercise the boundary from both sides. Do not move it into
configuration, do not derive the count from an order index, and do not add a code path that creates
a slot without the check. If you change what it means, the tests that assert the refusal must fail
when the guard is neutered — a boundary a self-hoster can raise with an environment variable is not
a boundary.

Also load-bearing:

- **References only — no bytes, no keys, no evidence.** A document is a document id plus a pinned
  content hash; a signature is a reference. The pinning is what makes the same-document invariant
  hold, so do not loosen it.
- **On-behalf-of has no fallback.** Document references are validated with the user's delegated
  token; a call without a subject token fails closed rather than using this service's identity.
- **Audit and events are non-fatal and PII-free** — pseudonymous internal identity references only,
  no contact data in lifecycle events, and never a failure that rolls back a user's action.
- **New state or a new transition means the state machine diagram in the README changes too**, in
  the same pull request. It is the map everyone reads before touching this service.

## Proposing a change

- Work on a branch and open a pull request against `develop`. `develop` is merged into `main` and
  tagged there when a release goes out, so `main` is never committed to directly.
- **Sign off every commit.** This project uses the
  [Developer Certificate of Origin](https://developercertificate.org/): by adding a
  `Signed-off-by: Your Name <you@example.org>` line you certify that you wrote the change or
  otherwise have the right to submit it under this project's licence. `git commit -s` adds the line
  for you; the name and address must match the commit author. A pull request whose commits lack it
  fails the DCO check and cannot be merged.
- Keep the change focused: one concern per pull request.
- A change in behaviour comes with a test that fails without it.
- Match the style around you — naming, error handling, comment density. Comments explain what and
  why in plain domain terms.
- A change an operator or an integrator can feel — a new or changed endpoint, field, error code,
  event, configuration knob or default — belongs in [CHANGELOG.md](CHANGELOG.md) in the same pull
  request.
- Pull requests also run a dependency review. A new dependency needs a reason the standard library
  or the existing ones cannot cover.

## Licence

This project is licensed under the **GNU Affero General Public License, version 3 only** (see
[LICENSE](LICENSE)). By submitting a contribution you agree that it is provided under the same
licence.

Worth knowing what AGPL means here, because this is a service rather than a library: if you run a
modified version and let others interact with it over a network, the licence requires you to offer
those users the corresponding source of your modified version. Using it unmodified, or modifying it
for purely internal use with no network users, does not trigger that.
