# Doc images

One row per visual. The SVG in this folder is the source and carries every label as `<text>`; `make diagrams` checks the labels that name code against the source the row cites.

| id | subject | doc § section | checked labels | source |
|---|---|---|---|---|
| wardyn-walls | One outer wall around a datacentre and people's devices; each sandbox behind its own inner wall | `docs/README.md` opener | none | `wardyn-walls.webp` only |
| wardyn-places | The same walls as a plan: each place runs the sandboxes that fit it | `docs/README.md` opener, under the first figure | none | `wardyn-places.webp` only |
| wardyn-run | One run from console to destinations: control plane, barrier, egress proxy, allow, hold or deny | `README.md` § Architecture at a glance, above the diagram | none | `wardyn-run.webp` only |
| wardyn-laptop | One enrolled laptop: normal path, sandboxes out through their egress proxy only, audit to the org control plane | `docs/DESKTOP.md` § Topology | none | `wardyn-laptop.webp` only |
| wardyn-barriers | Fence (CC1), Wall (CC2, default) and Vault (CC3, experimental) compared | `threatmodel/THREAT-MODEL.md` § 7, above the diagram | none | `wardyn-barriers.webp` only |
