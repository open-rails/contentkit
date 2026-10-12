-- parent: 1 sha256:0d2be33bce1f2bcb481f90c5fa16d1ab296ed2b9e169eb88471de94081d0ba7f
-- Coordinated dev cutover: stop signal writers and require empty legacy
-- projections. Raw events and the erasure ledger are unchanged. Hosts rebuild
-- each tenant before starting readers/writers on the new library version.

SELECT throwIf(count() > 0 AND
    (SELECT type FROM system.columns WHERE database = currentDatabase()
        AND table = 'subject_content_state' AND name = 'version') != 'UInt256',
    'projection generation cutover requires empty legacy subject_content_state')
FROM subject_content_state;

SELECT throwIf(count() > 0 AND
    (SELECT type FROM system.columns WHERE database = currentDatabase()
        AND table = 'subject_content_daily' AND name = 'version') != 'UInt256',
    'projection generation cutover requires empty legacy subject_content_daily')
FROM subject_content_daily;

ALTER TABLE subject_content_state {{ON_CLUSTER}} MODIFY COLUMN version UInt256;
ALTER TABLE subject_content_daily {{ON_CLUSTER}} MODIFY COLUMN version UInt256;
