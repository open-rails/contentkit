-- Withdrawals remain withdrawn: rolling back code cannot safely restore a sale.
ALTER TABLE content_offers DROP COLUMN content_id;
