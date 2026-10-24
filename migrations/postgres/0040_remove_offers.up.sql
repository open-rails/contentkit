-- OpenRails owns products, prices and grants. Never discard a sale intent that
-- has not reached that authority. Stop old ContentKit writers and drain their
-- offer jobs before applying this migration.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM content_offers WHERE synced_revision < revision) THEN
        RAISE EXCEPTION 'ContentKit offer removal: unsynchronized offers remain. Run the previous ContentKit version to settle them in OpenRails, stop its workers, then retry this migration.';
    END IF;
    IF EXISTS (
        SELECT 1 FROM content_offers
        WHERE state = 'active' AND (
            nullif(product_id, '') IS NULL
            OR (coalesce((terms->>'amount')::numeric, 0) > 0 AND nullif(price_id, '') IS NULL)
        )
    ) THEN
        RAISE EXCEPTION 'ContentKit offer removal: an active offer has no OpenRails product or price. Verify and settle its billing identity before retrying this migration.';
    END IF;
END
$$;

-- These are ContentKit-only tables/columns. Product IDs, immutable prices,
-- subscriptions, purchases and granted entitlements in OpenRails are untouched.
DROP TABLE content_offers;
ALTER TABLE content_items DROP COLUMN ever_sold;
ALTER TABLE content_entities DROP COLUMN ever_sold;
