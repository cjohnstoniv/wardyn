# Doc images

One row per visual. An SVG in this folder is its own source and carries every label as `<text>`; `make diagrams` checks the labels that name code against the source the row cites. A WebP is the shown file, with its labels in the image and described by its alt text.

| id | subject | doc § section | checked labels | source |
|---|---|---|---|---|
| wardyn-walls | One outer wall around a datacentre and people's devices; each sandbox behind its own inner wall | `README.md` and `docs/README.md` openers | none | `wardyn-walls.webp` only |
| wardyn-places | The same walls as a plan: each place runs the sandboxes that fit it | `README.md` and `docs/README.md` openers, directly under the walls figure | none | `wardyn-places.webp` only |
| wardyn-run | One run from console to destinations: control plane, barrier, egress proxy, allow, hold or deny | `README.md` § Architecture at a glance, above the diagram | none | `wardyn-run.webp` only |
| wardyn-laptop | One enrolled laptop: normal path, sandboxes out through their egress proxy only, audit to the org control plane | `docs/DESKTOP.md` § Topology | none | `wardyn-laptop.webp` only |
| wardyn-remote | One cluster: internal services on the normal path, sandboxes out through their egress proxy only, audit to the org control plane | `ARCHITECTURE.md` § Deployment surface | none | `wardyn-remote.webp` only |
| wardyn-barriers | Fence (CC1), Wall (CC2, default) and Vault (CC3, experimental) compared | `threatmodel/THREAT-MODEL.md` § 7, above the diagram | none | `wardyn-barriers.webp` only |
