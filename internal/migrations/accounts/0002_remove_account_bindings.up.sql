ALTER TABLE content_account_avatar_placement
    DROP CONSTRAINT content_account_avatar_placement_tenant_id_fkey;

ALTER TABLE content_account_erasure_intents
    DROP CONSTRAINT content_account_erasure_intents_tenant_id_fkey;

ALTER TABLE content_account_erasure_receipts
    DROP CONSTRAINT content_account_erasure_receipts_tenant_id_fkey;

DROP TABLE content_account_bindings;
