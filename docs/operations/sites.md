> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Sites: runs near their data

- A site is an admin's name for a place, such as an office, an edge cluster or a cloud region. It groups organization [runner pools](runner-pools.md) and admin drives, and it can name one of its pools as its default.
- When a run names no pool and its homed drives are all at one site, the run goes to that site's default pool. The run and its `run.create` audit row then name the site.
- A site is a placement label. It grants nothing, isolates nothing and is not a failure domain for the control plane.
- Every reason code named here is in [sdk.md](../sdk.md).

## Turn sites on

- Sites are off until an admin sends `{"features":{"org_sites":{"enabled":true}}}` to `PUT /site-config`. `{"enabled":false}` turns them off again.
- While sites are off, a write that adds a site, puts a pool at one, sets a default pool or homes a drive is refused `403` `org_sites_off`. Taking a pool or a drive off a site and deleting a site still work.
- While sites are off, placement, the run record and the audit row are as they were before sites existed. Sites and their pools are still listed.
- With sites on but no site defined, placement is the same as with sites off.

## Define a site

| Route | Who | What it does |
|---|---|---|
| `GET /org-sites`, `GET /org-sites/{id}` | Anyone signed in | Lists sites, or reads one. A site names only the pools the reader may use. |
| `POST /org-sites` | Admin | Adds a site: `name`, and an optional `storage_label` and `storage_url`. |
| `PUT /org-sites/{id}` | Admin | Replaces the name, the storage note and `default_pool_id`, at the `revision` you read. |
| `DELETE /org-sites/{id}` | Admin | Deletes a site with no pool and no drive at it. A deleted site stays on the runs placed there. |
| `PUT` and `DELETE /runner-pools/{id}/site` | Admin | Puts an organization pool at a site, moving it from any other, or takes it off. |
| `PUT` and `DELETE /drives/{id}/site` | Admin | Homes an admin drive at a site, or takes its home away. |
| `GET /org-sites/drives` | Admin or security admin | Lists the admin drives homed at a site, as `GET /drives` lists drives. |

- A site's name follows the pool name rule: 1 to 64 printable characters, unique among live sites.
- A pool is at one site at most. Only an organization pool can be at a site, and only an admin drive can have a home site; anything else is refused `org_site_pool_scope`.
- A member drive has no home site, because it is made wherever its run goes.
- A site's default pool must be one of its own pools (`org_site_default_pool`). When that pool leaves the site, the site has no default until you set one.
- A site with a pool or a drive at it is not deleted (`org_site_in_use`). Take them off first.
- Moving a pool to another site raises the pool's revision. A run admitted against the pool before the move is refused as stale, so it is never recorded at the old site.

## The storage note

- `storage_label` and `storage_url` tell people where the site's data lives. Wardyn never connects to the link and stores no credential for it.
- The link is an `http` or `https` URL with no user name, password, query or fragment; anything else is refused `org_site_invalid`.
- Audit rows carry the label and `storage_link_set`, never the link.

## Which pool a run goes to

Sites add one step to [which pool a run goes to](runner-pools.md#which-pool-and-which-runner):

1. The pool the run names.
2. While sites are on: the default pool of the one site where every homed drive of the run is.
3. The person's own default pool.
4. The organization's default pool.

- A run held to one runner skips the site step: a drive bound to a runner, a kept machine, bound workspace folders or `runner_id`. The audit row says `site_preference: pinned`.
- A site default that is gone, at a deleted site, or not a pool the person may use is passed over (`unusable`). Drives homed at two sites give no preference (`ambiguous`).
- A site default the person may use is chosen like a saved default. When none of its runners can take the run, the run is refused or queued as for any chosen pool. It does not fall through to the person's default.
- So a site's default pool must be able to run what people launch with the site's drives. Otherwise those launches are refused until they name a pool.
- A queued run keeps the pool, selection and site it was admitted with, whatever happens to the site while it waits.

## Proxies and failure domains

- Each run's egress proxy runs beside the run, on the run's runner, so it is already at the run's site. There is no proxy per site.
- The corporate upstream proxy stays install-wide (`upstream_proxy_url` in site config).
- A site does not split the control plane. Every site's runs depend on the same control plane; see [high availability](high-availability.md).

## Audit

- `org_site.create`, `org_site.update` and `org_site.delete` record a site's changes, and `org_site.pool.assign`, `org_site.pool.unassign`, `org_site.drive.set` and `org_site.drive.clear` record pools and drives joining and leaving it.
- A write that changes nothing writes no row. Each row's data is in [the audit actions](../AUDIT-ACTIONS.md).
- A run placed in a pool at a site carries `site_id`, and its `run.create` row carries `site_id`, `site_name` and `site_preference`.
