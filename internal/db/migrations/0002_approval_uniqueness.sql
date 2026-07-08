-- At most ONE open (PENDING) credential approval per grant. Closes the
-- SELECT-then-INSERT race in the broker's ensureApproval: without this,
-- concurrent in-sandbox mint attempts could create duplicate PENDING rows,
-- and approving the older duplicate while a newer PENDING exists would wedge
-- the mint (the mint join reads the newest approval).
CREATE UNIQUE INDEX IF NOT EXISTS approvals_pending_credential_uniq
    ON approvals (grant_id)
    WHERE kind = 'credential' AND state = 'PENDING';
