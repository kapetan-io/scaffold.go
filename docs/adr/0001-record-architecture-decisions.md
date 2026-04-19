# 1. Record architecture decisions

Date: 2026-04-19

## Status

Accepted

## Context

Scaffold is a framework with a deliberately opinionated design. Decisions about lifecycle, dependency injection, port ownership, middleware scoping, and code-generation boundaries compound over time; later contributors need to understand not only what the code does but why it is shaped that way. Without a durable record, reasoning behind decisions is lost to commit history, discussion threads, and individual memory. Future contributors then face two equally bad options: reverse-engineer intent from the code, or re-open settled decisions because the context is no longer available.

## Decision

We will use Architecture Decision Records, as described by Michael Nygard in [Documenting Architecture Decisions](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions), stored in `docs/adr/`.

Each ADR is a short markdown file capturing one decision in four sections: Status, Context, Decision, Consequences. Files are numbered sequentially (`NNNN-title.md`). ADRs are immutable once accepted; when a decision changes, a new ADR is written and the old one is marked `Superseded by ADR-NNNN`.

## Consequences

Contributors gain a searchable log of the architectural reasoning behind the codebase, which reduces re-litigation of settled choices and accelerates onboarding. The practice adds a small, recurring cost: decisions worth recording must be recognized and written down in the moment, rather than discovered later.

ADRs are narrow by design — they record decisions, not requirements, designs, or postmortems. Other documents (tech specs, runbooks, design notes) still have their place.
