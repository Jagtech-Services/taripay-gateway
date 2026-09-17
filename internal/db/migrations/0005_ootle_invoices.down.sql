-- 0005_ootle_invoices down: revert every column/index added by
-- 0005_ootle_invoices.up.sql.

DROP INDEX IF EXISTS idx_invoices_ootle_nonterminal;
ALTER TABLE invoices DROP COLUMN IF EXISTS ootle_trapdoor_flagged;
ALTER TABLE invoices DROP COLUMN IF EXISTS ootle_resource_address;
ALTER TABLE invoices DROP COLUMN IF EXISTS ootle_component_address;
ALTER TABLE invoices DROP COLUMN IF EXISTS chain;
