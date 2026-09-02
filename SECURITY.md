# Security Policy

## Reporting a vulnerability

Please do not open a public issue for a security vulnerability.

Report it privately through GitHub's [Security Advisories](https://github.com/mya-ai/agentkit/security/advisories/new), or by
email to **security@heymya.ai**. We will acknowledge within a few business days.

Please include what an attacker can do, the affected version or commit, and a reproduction if you
have one.

## Scope

agentkit is a library. It has no network listeners and no credential handling of its own — your
provider adapter and your `ActorStore` own those. Relevant to this repo:

- A way to bypass the tier gate so an irreversible tool call executes inside a loop instead of being
  captured as a proposal.
- A way to make capability verification accept a call the agent never declared.
- A way to make the single-writer guarantee admit two concurrent live instances for one unit, given a
  correct `ActorStore`.

Issues in *your* provider adapter, datastore, or prompt content are yours to fix, though we are glad
to be told if the kit's interfaces made the mistake easy to write.
