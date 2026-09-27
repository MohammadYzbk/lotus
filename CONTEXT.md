# Lotus Product Language

Canonical vocabulary for Lotus's product references and for naming the delivery
stages that lead to release. Lotus is one cross-platform application, so this
vocabulary is platform-neutral.

## Product references

**Desktop concept**:
A named reference scenario that expresses product intent. It may represent one
state in a larger flow and does not imply one view or code type.
_Avoid_: Screen, mockup

**Acceptance case**:
A scenario derived from one or more Desktop concepts that states a user-visible
outcome and required behavior. Concepts and acceptance cases may map one-to-many
or many-to-one and never imply code structure.
_Avoid_: Screen test, view specification

**Product operation**:
A user-invoked action that changes project or app data, navigation,
presentation mode, or window or session state. Text entry and ordinary
selection are editing interactions rather than product operations.
_Avoid_: Primary operation, user action

**Owner sign-off**:
A recorded product-owner decision accepting a scoped deviation from an accepted
contract, including its reason and affected acceptance cases.
_Avoid_: Verbal approval, implicit exception

**Conformance gap**:
An unapproved mismatch between an accepted contract and the current
documentation or implementation.
_Avoid_: Existing behavior, accepted exception

## Compiler outcomes

**Document failure**:
The compiler completed normally, but the current project source did not produce
a new PDF. Source diagnostics explain the failure.
_Avoid_: Compiler failure, compile error

**Compiler failure**:
The compiler could not complete a valid attempt because its engine or process
failed rather than because of project source.
_Avoid_: Document failure, source error

**Stale compiled output**:
The last successfully compiled PDF retained while a newer attempt is pending or
after that attempt fails. It remains usable but does not represent current source.
_Avoid_: Current output, failed PDF

## Delivery stages

**Client Completion**:
Every approved compiler-independent behavior and the acceptance cases covering
every Desktop concept.
_Avoid_: UI complete, frontend complete

**Compiler Integration**:
The accepted engine invocation, diagnostics mapping, process lifecycle, and live
compile behavior.
_Avoid_: Compiler UI

**Engineering Hardening**:
The architecture, quality, accessibility, performance, release-readiness, and
documentation work required to make the product durable.
_Avoid_: Polish, cleanup
