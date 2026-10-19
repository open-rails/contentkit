-- Target sync reads every offer of a target, withdrawn ones too; the unique
-- index covers only active offers.
CREATE INDEX content_offers_target_idx ON content_offers (tenant_id, target_kind, target_id);
