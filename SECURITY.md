# Security policy

This service is the durable record of **who must sign what, in what order, and where each slot
stands**. It runs the envelope state machine, enforces signing order, decides who may see or act on
an envelope, and applies every transition atomically under a compare-and-set on the envelope
version. It holds no document bytes, no keys and no cryptographic evidence — only references.

So its failures are failures of **authority and order**: a person acting on an envelope that is not
theirs, a slot signed before its turn, a transition lost or applied twice. Each of those ends in a
signing that happened outside the workflow it was supposed to follow.

Please report security problems privately. Do not open a public issue, pull request or discussion
for anything that could be exploited before a fix exists.

## How to report

Use **[private vulnerability reporting](https://github.com/signbyte/envelope/security/advisories/new)**
on this repository. The report stays visible only to you and the maintainers until an advisory is
published, and it gives us one place to discuss and co-ordinate a fix with you.

Please include, as far as you can establish it:

- what the problem is, and what an attacker gains from it;
- the smallest set of steps that reproduces it, and against which version or commit;
- whether it needs a particular envelope state, ordering mode or participant role;
- whether you have told anyone else, and whether a disclosure date already binds you.

**Please do not send us real tokens, identity codes or personal data.** Recipient and signer
identities on an envelope *are* personal data; a redacted trace explains almost any finding here.

## What happens next

- We acknowledge a report within **five working days**.
- We tell you whether we can reproduce it, and what we think its severity is, as soon as we know.
- We keep you updated while a fix is prepared, and we agree a disclosure date with you. Our default
  is to publish an advisory once a fix is available, and in any case within **90 days** of the
  report — earlier if the problem is already public or being exploited.
- We credit you in the advisory unless you would rather stay anonymous.

There is no bug-bounty programme. We are grateful anyway, and we say so publicly.

## What we consider most serious

**Reaching an envelope that is not yours.** Reads and writes are scoped to the owner, or to an
invited signer matched on their authenticated identity code taken from a trusted token claim —
never from a header a caller controls. Serious findings: any path that skips that filter; a match
made on something spoofable; and anything that lets a caller tell an envelope that does not exist
apart from one they may not see, since the difference is itself a disclosure. The signer inbox is
keyed on identity rather than ownership, which makes it the most interesting surface to probe.

**Signing out of turn.** A sequential slot becomes eligible only once every lower-ordered slot is
signed, checked at the eligibility precondition *and* re-checked as slots sign. Anything that opens
a slot early, or that lets the order be reordered after sending, breaks the guarantee the whole
workflow exists to provide.

**A transition lost or doubled.** Every state change applies only if the envelope version still
matches the one the caller read. A path that writes without the compare-and-set, or that retries in
a way that applies a transition twice, can lose a decline or double a completion — and the
slot-signed callback is deliberately idempotent so the signing service can retry safely. A callback
that stops being idempotent is a real finding.

**The signer-slot limit being exceeded.** The number of signer slots an envelope may carry is a
named constant in the service, not a configuration value, and it is enforced in the data layer as
well. Anything that admits an extra signer — a count derived from an order index rather than
counted, a path that adds a slot without the check, or a way to set the limit from outside the
package — defeats a boundary that is deliberately not a knob.

**Acting as the wrong subject.** Document references are validated with the user's delegated token,
never this service's own identity, and a call without a subject token must fail closed. Any
fallback there lets one person's envelope reference another person's document.

**The same-document invariant.** A document's content hash is pinned when it is attached. A path
that lets the referenced document change after pinning means a signer signs something other than
what the envelope showed.

**Personal data where it should not be.** Access records carry pseudonymous internal identity
references only, and lifecycle events carry no contact data at all — a notification consumer
downstream resolves that itself. An event or audit record that starts carrying a name, an email
address or a raw identity code is a privacy failure with a wide blast radius, because those events
fan out.

Denial of service and findings that need an already-compromised host or an already-authenticated
administrator are in scope but lower priority. Reports about outdated dependencies are welcome
where you can show the vulnerable path is actually reachable.

## What is deliberately not a finding

- **This service holds no document bytes, no keys and no cryptographic evidence** — only references.
  It does not sign, does not validate a signature, and does not write the signing-evidence chain.
- **Audit and event publication are non-fatal by design**: a failure to record or publish never
  fails the user's action. A report that one of them *silently changed the recorded outcome* is a
  finding; a report that a failure did not block the action is not.
- **The in-memory store is a development backend** and is not durable. Running production without a
  store connection is a deployment mistake, not a defect — though a configuration in which it is
  selected *silently* would be.
- **Expiry is modelled but not swept here.** No endpoint or scheduler in this service drives an
  envelope to expired yet; that is a documented gap, not a vulnerability.

A report that an API *implies* one of the guarantees above, or that a caller is likely to read it
that way, is a real finding.

## Scope

This policy covers the code in this repository. It does not cover the document, signing or
authentication services, the database, the broker, a notification consumer, or any deployment
operated by someone other than us — report those to the parties that run them. How a deployment
configures this service is the operator's responsibility, but a report that a **default** is unsafe
is very much in scope.

## Supported versions

Security fixes land on the most recent release. Older tags are not patched; if you are pinned to
one, the fix is to move forward.
