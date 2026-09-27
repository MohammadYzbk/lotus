---
status: superseded
---

> **Superseded 2026-09-15.** The bundled corpus, local compiler server, and
> protobuf contract this ADR depended on were removed along with the native
> macOS implementation. Lotus now invokes a `tectonic` binary on `PATH`.

# Ship a curated offline TeX corpus

The native MVP ships a digest-pinned corpus reproducibly derived from TeX
Live's `scheme-small`, its complete format-language registry, and narrow Lotus
fixture/runtime additions instead of the multi-gigabyte full TTB or a warmed
cache. This preserves deterministic offline compilation within a 160 MiB
installed engine budget, avoids the five unresolved packages in the full
distribution, and makes omitted packages an explicit unavailable-offline
outcome that can grow only through a reviewed signed app release.
