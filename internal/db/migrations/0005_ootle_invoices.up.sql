-- 0005_ootle_invoices: adds Ootle L2 invoice support to `invoices` (task brief "Ootle
-- L2 stablecoin/native-resource payment support in taripay-gateway", section 1).
--
-- Every new column is NULL-safe/defaulted so every existing (and every future L1) row
-- is completely unaffected: `chain` defaults to 'l1', matching current behavior
-- exactly, and the three Ootle-specific columns are only ever populated by
-- internal/invoice.Store.CreateOotle for a genuinely new chain='ootle' invoice.
--
-- chain distinguishes which network + payment-flow an invoice uses:
--   'l1'    -- the existing per-invoice wallet-address + StreamTransactionEvents flow
--             (internal/eventwatcher.Watcher.Run/Reconcile), completely unchanged by
--             this migration.
--   'ootle' -- a per-invoice instantiation of the Snipa22/tari-ootle-invoice template's
--             Invoice component, polled by internal/eventwatcher.Watcher.PollOotleInvoices
--             (see that method's doc comment) via internal/ootlepay.
ALTER TABLE invoices ADD COLUMN chain TEXT NOT NULL DEFAULT 'l1';

-- ootle_component_address is the instantiated per-invoice Invoice component's on-chain
-- address (component_<hex>) once the instantiation transaction commits; NULL until then
-- (and always NULL for chain='l1' rows).
ALTER TABLE invoices ADD COLUMN ootle_component_address TEXT;

-- ootle_resource_address is the resource this invoice expects to be paid in (the
-- Invoice template's own `expected_resource` — native tXTR's resource address today,
-- or later a TariStableCoin's resource address), caller-supplied at invoice-creation
-- time; NULL for chain='l1' rows.
ALTER TABLE invoices ADD COLUMN ootle_resource_address TEXT;

-- ootle_trapdoor_flagged is set true by the watcher poll (PollOotleInvoices) when it
-- observes the Invoice component's held_resources() non-empty while is_paid() is still
-- false — i.e. money landed on-chain but could not be auto-forwarded to the merchant
-- (wrong resource, wrong amount, or a repeat payment attempt). This is what the admin
-- panel's sweep button/list surfaces (task brief section 5) — it is a distinct,
-- admin-visible signal, deliberately NOT folded into the `status` column (money stuck
-- is not the same thing as an invoice status transition; see PollOotleInvoices' own
-- doc comment).
ALTER TABLE invoices ADD COLUMN ootle_trapdoor_flagged BOOLEAN NOT NULL DEFAULT false;

-- Supports internal/invoice.Store.ListNonTerminalOotle's poll-loop query (task brief
-- section 4): every non-terminal chain='ootle' invoice, without scanning the whole
-- table. Mirrors 0004_order_ref_active_index's own partial-index precedent/status list
-- exactly (see that migration's doc comment for why a partial index over the
-- non-terminal statuses is preferred to indexing everything).
CREATE INDEX idx_invoices_ootle_nonterminal ON invoices (chain)
    WHERE chain = 'ootle' AND status NOT IN ('confirmed', 'rejected', 'expired', 'cancelled');
