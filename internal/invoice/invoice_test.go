package invoice

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Snipa22/taripay-gateway/internal/db"
)

// setupStore connects to TARIPAY_TEST_POSTGRES_DSN, migrates a clean schema, and
// returns a Store wired to a fake resolveAddress (never touches a real wallet gRPC
// connection or global walletGRPC package state, per the brief's explicit
// requirement). Skips the calling test if no live Postgres DSN is configured.
func setupStore(t *testing.T, resolveAddress ResolveAddressFunc) *Store {
	t.Helper()
	dsn := os.Getenv("TARIPAY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SKIP: TARIPAY_TEST_POSTGRES_DSN not set, no live Postgres to test against")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	database := &db.DB{Pool: pool}
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS schema_migrations, webhook_deliveries, invoices CASCADE`); err != nil {
		t.Fatalf("cleanup before test: %v", err)
	}
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	if resolveAddress == nil {
		resolveAddress = func(paymentID string) (string, error) {
			return "fake-address-for-" + paymentID, nil
		}
	}
	return NewStore(pool, resolveAddress)
}

func TestCreate_InsertsRowWithResolvedAddress(t *testing.T) {
	s := setupStore(t, nil)
	ctx := context.Background()

	inv, err := s.Create(ctx, "order-123", 5_000_000, 30*time.Minute)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if inv.OrderRef != "order-123" {
		t.Errorf("OrderRef = %q, want %q", inv.OrderRef, "order-123")
	}
	if inv.AmountUTari != 5_000_000 {
		t.Errorf("AmountUTari = %d, want 5000000", inv.AmountUTari)
	}
	if inv.PaymentID != inv.ID.String() {
		t.Errorf("PaymentID = %q, want it to equal ID.String() (%q)", inv.PaymentID, inv.ID.String())
	}
	if inv.Address != "fake-address-for-"+inv.PaymentID {
		t.Errorf("Address = %q, want the resolved fake address", inv.Address)
	}
	if inv.Status != StatusPending {
		t.Errorf("Status = %q, want %q", inv.Status, StatusPending)
	}
	if !inv.ExpiresAt.After(inv.CreatedAt) {
		t.Errorf("ExpiresAt (%v) should be after CreatedAt (%v)", inv.ExpiresAt, inv.CreatedAt)
	}

	// Round-trip through the DB confirms the row was actually inserted.
	got, err := s.GetByID(ctx, inv.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got.PaymentID != inv.PaymentID {
		t.Errorf("GetByID().PaymentID = %q, want %q", got.PaymentID, inv.PaymentID)
	}
}

func TestCreate_WalletResolutionFailureInsertsNoRow(t *testing.T) {
	wantErr := errors.New("wallet: connection refused")
	s := setupStore(t, func(paymentID string) (string, error) {
		return "", wantErr
	})
	ctx := context.Background()

	_, err := s.Create(ctx, "order-456", 1000, time.Minute)
	if err == nil {
		t.Fatal("Create() error = nil, want an error when address resolution fails")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Create() error = %v, want it to wrap %v", err, wantErr)
	}

	// No orphan row should exist for order-456.
	var count int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM invoices WHERE order_ref = $1`, "order-456").Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Errorf("invoices with order_ref=order-456 = %d, want 0 (no orphan row on wallet failure)", count)
	}
}

// TestCreate_IdempotentWhileNonTerminal covers the idempotent-invoice-creation fix
// (task brief part 1, I5/AI-16): two Create calls with the same order_ref while the
// first invoice is still non-terminal (pending, here) must return the SAME invoice
// (same ID), not create a second row for the same order_ref.
func TestCreate_IdempotentWhileNonTerminal(t *testing.T) {
	s := setupStore(t, nil)
	ctx := context.Background()

	first, err := s.Create(ctx, "order-idempotent", 1000, time.Hour)
	if err != nil {
		t.Fatalf("Create() #1 error = %v", err)
	}
	if first.Status != StatusPending {
		t.Fatalf("Create() #1 Status = %q, want %q", first.Status, StatusPending)
	}

	second, err := s.Create(ctx, "order-idempotent", 9999, time.Hour)
	if err != nil {
		t.Fatalf("Create() #2 error = %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("Create() #2 ID = %v, want it to equal Create() #1's ID %v (idempotent retry must return the same invoice)", second.ID, first.ID)
	}
	// The second call's differing amount must be ignored — the existing
	// invoice's own amount/address are returned unchanged, not overwritten.
	if second.AmountUTari != first.AmountUTari {
		t.Errorf("Create() #2 AmountUTari = %d, want %d (unchanged from the original invoice)", second.AmountUTari, first.AmountUTari)
	}
	if second.Address != first.Address {
		t.Errorf("Create() #2 Address = %q, want %q (unchanged from the original invoice)", second.Address, first.Address)
	}

	var count int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM invoices WHERE order_ref = $1`, "order-idempotent").Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("invoices with order_ref=order-idempotent = %d, want 1 (no second row created)", count)
	}

	// A third call after the invoice transitions underpaid (still
	// non-terminal) must still be idempotent.
	if err := s.UpdateStatus(ctx, first.ID, StatusUnderpaid, nil); err != nil {
		t.Fatalf("UpdateStatus(underpaid): %v", err)
	}
	third, err := s.Create(ctx, "order-idempotent", 1000, time.Hour)
	if err != nil {
		t.Fatalf("Create() #3 error = %v", err)
	}
	if third.ID != first.ID {
		t.Errorf("Create() #3 ID = %v, want it to equal the original ID %v (still non-terminal)", third.ID, first.ID)
	}
}

// TestCreate_TerminalOrderRefAllowsNewInvoice covers the other half of the
// idempotent-invoice-creation fix's scoping: a Create call with an order_ref
// matching an invoice that's already terminal (confirmed/rejected/expired/
// cancelled) must create a genuinely NEW invoice, proving old completed orders
// don't block reuse of the reference (e.g. a recycled WooCommerce order number).
func TestCreate_TerminalOrderRefAllowsNewInvoice(t *testing.T) {
	for _, terminalStatus := range []string{StatusConfirmed, StatusRejected, StatusExpired, StatusCancelled} {
		t.Run(terminalStatus, func(t *testing.T) {
			s := setupStore(t, nil)
			ctx := context.Background()

			orderRef := "order-terminal-" + terminalStatus
			first, err := s.Create(ctx, orderRef, 1000, time.Hour)
			if err != nil {
				t.Fatalf("Create() #1 error = %v", err)
			}
			if err := s.UpdateStatus(ctx, first.ID, terminalStatus, nil); err != nil {
				t.Fatalf("UpdateStatus(%s): %v", terminalStatus, err)
			}

			second, err := s.Create(ctx, orderRef, 2000, time.Hour)
			if err != nil {
				t.Fatalf("Create() #2 error = %v", err)
			}
			if second.ID == first.ID {
				t.Errorf("Create() #2 ID = %v, want a NEW id distinct from the terminal invoice's %v", second.ID, first.ID)
			}
			if second.AmountUTari != 2000 {
				t.Errorf("Create() #2 AmountUTari = %d, want 2000 (a genuinely new invoice)", second.AmountUTari)
			}

			var count int
			if err := s.db.QueryRow(ctx, `SELECT count(*) FROM invoices WHERE order_ref = $1`, orderRef).Scan(&count); err != nil {
				t.Fatalf("count query: %v", err)
			}
			if count != 2 {
				t.Errorf("invoices with order_ref=%s = %d, want 2 (old terminal + new)", orderRef, count)
			}
		})
	}
}

// TestGetActiveByOrderRef covers GetActiveByOrderRef directly: not-found for an
// unused order_ref, found for a non-terminal one, and not-found again once that
// invoice becomes terminal.
func TestGetActiveByOrderRef(t *testing.T) {
	s := setupStore(t, nil)
	ctx := context.Background()

	if _, err := s.GetActiveByOrderRef(ctx, "order-never-used"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetActiveByOrderRef(never used) error = %v, want ErrNotFound", err)
	}

	inv, err := s.Create(ctx, "order-active-lookup", 500, time.Hour)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := s.GetActiveByOrderRef(ctx, "order-active-lookup")
	if err != nil {
		t.Fatalf("GetActiveByOrderRef() error = %v", err)
	}
	if got.ID != inv.ID {
		t.Errorf("GetActiveByOrderRef().ID = %v, want %v", got.ID, inv.ID)
	}

	if err := s.UpdateStatus(ctx, inv.ID, StatusConfirmed, nil); err != nil {
		t.Fatalf("UpdateStatus(confirmed): %v", err)
	}
	if _, err := s.GetActiveByOrderRef(ctx, "order-active-lookup"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetActiveByOrderRef(now terminal) error = %v, want ErrNotFound", err)
	}
}

func TestGetByID_NotFound(t *testing.T) {
	s := setupStore(t, nil)
	ctx := context.Background()

	_, err := s.GetByID(ctx, uuid.New())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID() error = %v, want ErrNotFound", err)
	}
}

func TestGetByPaymentID_RoundTrip(t *testing.T) {
	s := setupStore(t, nil)
	ctx := context.Background()

	inv, err := s.Create(ctx, "order-789", 42, time.Hour)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := s.GetByPaymentID(ctx, inv.PaymentID)
	if err != nil {
		t.Fatalf("GetByPaymentID() error = %v", err)
	}
	if got.ID != inv.ID {
		t.Errorf("GetByPaymentID().ID = %v, want %v", got.ID, inv.ID)
	}

	_, err = s.GetByPaymentID(ctx, "does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByPaymentID(unknown) error = %v, want ErrNotFound", err)
	}
}

func TestUpdateStatus(t *testing.T) {
	s := setupStore(t, nil)
	ctx := context.Background()

	inv, err := s.Create(ctx, "order-status", 100, time.Hour)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	confirmedAt := time.Now().UTC().Truncate(time.Second)
	if err := s.UpdateStatus(ctx, inv.ID, StatusConfirmed, &confirmedAt); err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}

	got, err := s.GetByID(ctx, inv.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got.Status != StatusConfirmed {
		t.Errorf("Status = %q, want %q", got.Status, StatusConfirmed)
	}
	if got.ConfirmedAt == nil || !got.ConfirmedAt.Equal(confirmedAt) {
		t.Errorf("ConfirmedAt = %v, want %v", got.ConfirmedAt, confirmedAt)
	}

	if err := s.UpdateStatus(ctx, uuid.New(), StatusCancelled, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateStatus(unknown id) error = %v, want ErrNotFound", err)
	}
}

// TestAddReceivedAmount covers the C2-fix addition (task brief "fix C2 and add
// auth", part 1): AddReceivedAmount must accumulate correctly across multiple calls
// (simulating multiple transaction events for the same invoice) and return the new
// running total each time, against real Postgres — verifying the atomic
// UPDATE ... RETURNING form actually persists and reflects accumulation, not just a
// mocked/in-memory counter.
func TestAddReceivedAmount(t *testing.T) {
	s := setupStore(t, nil)
	ctx := context.Background()

	inv, err := s.Create(ctx, "order-received", 5000, time.Hour)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if inv.AmountReceivedUTari != 0 {
		t.Errorf("AmountReceivedUTari on creation = %d, want 0", inv.AmountReceivedUTari)
	}

	total, err := s.AddReceivedAmount(ctx, inv.ID, 3000)
	if err != nil {
		t.Fatalf("AddReceivedAmount(3000) error = %v", err)
	}
	if total != 3000 {
		t.Errorf("AddReceivedAmount(3000) returned total = %d, want 3000", total)
	}

	total, err = s.AddReceivedAmount(ctx, inv.ID, 2500)
	if err != nil {
		t.Fatalf("AddReceivedAmount(2500) error = %v", err)
	}
	if total != 5500 {
		t.Errorf("AddReceivedAmount(2500) returned total = %d, want 5500 (cumulative)", total)
	}

	// Round-trip through the DB confirms the accumulated total was actually
	// persisted, not just returned in-memory.
	got, err := s.GetByID(ctx, inv.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got.AmountReceivedUTari != 5500 {
		t.Errorf("GetByID().AmountReceivedUTari = %d, want 5500", got.AmountReceivedUTari)
	}

	if _, err := s.AddReceivedAmount(ctx, uuid.New(), 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddReceivedAmount(unknown id) error = %v, want ErrNotFound", err)
	}
}

// TestListNonTerminal covers the reconciliation-after-downtime fix's prerequisite
// (task brief part 1, S2/I7): seeds an invoice for every status value this package
// defines and confirms ListNonTerminal returns exactly the non-terminal ones
// (pending, seen, underpaid), excluding every status in TerminalStatuses (confirmed,
// rejected, expired, cancelled).
func TestListNonTerminal(t *testing.T) {
	s := setupStore(t, nil)
	ctx := context.Background()

	seed := func(orderRef, status string) *Invoice {
		inv, err := s.Create(ctx, orderRef, 100, time.Hour)
		if err != nil {
			t.Fatalf("Create(%s) error = %v", orderRef, err)
		}
		if status != StatusPending {
			if err := s.UpdateStatus(ctx, inv.ID, status, nil); err != nil {
				t.Fatalf("UpdateStatus(%s, %s) error = %v", orderRef, status, err)
			}
		}
		return inv
	}

	pending := seed("order-nonterm-pending", StatusPending)
	seen := seed("order-nonterm-seen", StatusSeen)
	underpaid := seed("order-nonterm-underpaid", StatusUnderpaid)
	confirmed := seed("order-nonterm-confirmed", StatusConfirmed)
	rejected := seed("order-nonterm-rejected", StatusRejected)
	expired := seed("order-nonterm-expired", StatusExpired)
	cancelled := seed("order-nonterm-cancelled", StatusCancelled)

	got, err := s.ListNonTerminal(ctx)
	if err != nil {
		t.Fatalf("ListNonTerminal() error = %v", err)
	}

	gotIDs := map[uuid.UUID]bool{}
	for _, inv := range got {
		gotIDs[inv.ID] = true
	}

	for _, tc := range []struct {
		name   string
		id     uuid.UUID
		wantIn bool
	}{
		{"pending", pending.ID, true},
		{"seen", seen.ID, true},
		{"underpaid", underpaid.ID, true},
		{"confirmed", confirmed.ID, false},
		{"rejected", rejected.ID, false},
		{"expired", expired.ID, false},
		{"cancelled", cancelled.ID, false},
	} {
		if gotIDs[tc.id] != tc.wantIn {
			t.Errorf("%s: present in ListNonTerminal() = %v, want %v", tc.name, gotIDs[tc.id], tc.wantIn)
		}
	}
}

func TestExpireStale(t *testing.T) {
	s := setupStore(t, nil)
	ctx := context.Background()

	// A pending invoice already past its expires_at.
	stale, err := s.Create(ctx, "order-stale", 10, time.Hour)
	if err != nil {
		t.Fatalf("Create(stale) error = %v", err)
	}
	if _, err := s.db.Exec(ctx, `UPDATE invoices SET expires_at = now() - interval '1 minute' WHERE id = $1`, stale.ID); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}

	// A seen invoice, also stale.
	seenStale, err := s.Create(ctx, "order-seen-stale", 10, time.Hour)
	if err != nil {
		t.Fatalf("Create(seenStale) error = %v", err)
	}
	if err := s.UpdateStatus(ctx, seenStale.ID, StatusSeen, nil); err != nil {
		t.Fatalf("UpdateStatus(seenStale): %v", err)
	}
	if _, err := s.db.Exec(ctx, `UPDATE invoices SET expires_at = now() - interval '1 minute' WHERE id = $1`, seenStale.ID); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}

	// A fresh pending invoice, not stale — must not be touched.
	fresh, err := s.Create(ctx, "order-fresh", 10, time.Hour)
	if err != nil {
		t.Fatalf("Create(fresh) error = %v", err)
	}

	// A confirmed invoice past expires_at — must not be touched (only
	// pending/seen are eligible).
	confirmedPast, err := s.Create(ctx, "order-confirmed-past", 10, time.Hour)
	if err != nil {
		t.Fatalf("Create(confirmedPast) error = %v", err)
	}
	now := time.Now().UTC()
	if err := s.UpdateStatus(ctx, confirmedPast.ID, StatusConfirmed, &now); err != nil {
		t.Fatalf("UpdateStatus(confirmedPast): %v", err)
	}
	if _, err := s.db.Exec(ctx, `UPDATE invoices SET expires_at = now() - interval '1 minute' WHERE id = $1`, confirmedPast.ID); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}

	count, err := s.ExpireStale(ctx)
	if err != nil {
		t.Fatalf("ExpireStale() error = %v", err)
	}
	if count != 2 {
		t.Errorf("ExpireStale() count = %d, want 2", count)
	}

	for _, tc := range []struct {
		name       string
		id         uuid.UUID
		wantStatus string
	}{
		{"stale pending -> expired", stale.ID, StatusExpired},
		{"stale seen -> expired", seenStale.ID, StatusExpired},
		{"fresh pending untouched", fresh.ID, StatusPending},
		{"confirmed past untouched", confirmedPast.ID, StatusConfirmed},
	} {
		got, err := s.GetByID(ctx, tc.id)
		if err != nil {
			t.Fatalf("%s: GetByID() error = %v", tc.name, err)
		}
		if got.Status != tc.wantStatus {
			t.Errorf("%s: Status = %q, want %q", tc.name, got.Status, tc.wantStatus)
		}
	}
}

// ---- Ootle L2 support tests (task brief "Ootle L2 stablecoin/native-resource
// payment support in taripay-gateway") ----

// TestCreateOotle_InsertsRowWithInstantiatedComponent covers CreateOotle's happy path
// and the new columns' round-trip through the DB (task brief section 6: "new column
// scan round-trip"): Chain/OotleComponentAddress/OotleResourceAddress/
// OotleTrapdoorFlagged must all come back exactly as inserted via GetByID (which goes
// through the same shared scanInvoice/invoiceColumns every other Store method uses).
func TestCreateOotle_InsertsRowWithInstantiatedComponent(t *testing.T) {
	s := setupStore(t, nil)
	ctx := context.Background()

	wantComponent := "component_deadbeef00000000000000000000000000000000000000000000000000000000"
	wantResource := "resource_0102030000000000000000000000000000000000000000000000000000000000"
	var gotPaymentID, gotResourceAddress string
	var gotAmount uint64
	instantiate := func(ctx context.Context, paymentID string, resourceAddress string, expectedAmount uint64) (string, error) {
		gotPaymentID = paymentID
		gotResourceAddress = resourceAddress
		gotAmount = expectedAmount
		return wantComponent, nil
	}

	inv, err := s.CreateOotle(ctx, "order-ootle-1", wantResource, 750_000, 30*time.Minute, instantiate)
	if err != nil {
		t.Fatalf("CreateOotle() error = %v", err)
	}

	if gotPaymentID != inv.PaymentID {
		t.Errorf("instantiate called with paymentID = %q, want %q", gotPaymentID, inv.PaymentID)
	}
	if gotResourceAddress != wantResource {
		t.Errorf("instantiate called with resourceAddress = %q, want %q", gotResourceAddress, wantResource)
	}
	if gotAmount != 750_000 {
		t.Errorf("instantiate called with expectedAmount = %d, want 750000", gotAmount)
	}

	if inv.Chain != ChainOotle {
		t.Errorf("Chain = %q, want %q", inv.Chain, ChainOotle)
	}
	if inv.OotleComponentAddress == nil || *inv.OotleComponentAddress != wantComponent {
		t.Errorf("OotleComponentAddress = %v, want %q", inv.OotleComponentAddress, wantComponent)
	}
	if inv.OotleResourceAddress == nil || *inv.OotleResourceAddress != wantResource {
		t.Errorf("OotleResourceAddress = %v, want %q", inv.OotleResourceAddress, wantResource)
	}
	if inv.OotleTrapdoorFlagged {
		t.Errorf("OotleTrapdoorFlagged = true on creation, want false")
	}
	if inv.Status != StatusPending {
		t.Errorf("Status = %q, want %q", inv.Status, StatusPending)
	}

	// Round-trip through the DB via GetByID confirms the new columns were
	// actually persisted (not just returned in-memory from CreateOotle itself).
	got, err := s.GetByID(ctx, inv.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got.Chain != ChainOotle {
		t.Errorf("GetByID().Chain = %q, want %q", got.Chain, ChainOotle)
	}
	if got.OotleComponentAddress == nil || *got.OotleComponentAddress != wantComponent {
		t.Errorf("GetByID().OotleComponentAddress = %v, want %q", got.OotleComponentAddress, wantComponent)
	}
	if got.OotleResourceAddress == nil || *got.OotleResourceAddress != wantResource {
		t.Errorf("GetByID().OotleResourceAddress = %v, want %q", got.OotleResourceAddress, wantResource)
	}
	if got.OotleTrapdoorFlagged {
		t.Errorf("GetByID().OotleTrapdoorFlagged = true, want false")
	}
}

// TestCreateOotle_InstantiationFailureInsertsNoRow mirrors
// TestCreate_WalletResolutionFailureInsertsNoRow's L1 precedent: if the injected
// instantiate func fails, no row is inserted at all — no orphan Ootle invoices with
// no on-chain component.
func TestCreateOotle_InstantiationFailureInsertsNoRow(t *testing.T) {
	s := setupStore(t, nil)
	ctx := context.Background()

	wantErr := errors.New("ootlepay: indexer unreachable")
	instantiate := func(ctx context.Context, paymentID string, resourceAddress string, expectedAmount uint64) (string, error) {
		return "", wantErr
	}

	_, err := s.CreateOotle(ctx, "order-ootle-fail", "resource_00", 1000, time.Minute, instantiate)
	if err == nil {
		t.Fatal("CreateOotle() error = nil, want an error when instantiation fails")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("CreateOotle() error = %v, want it to wrap %v", err, wantErr)
	}

	var count int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM invoices WHERE order_ref = $1`, "order-ootle-fail").Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Errorf("invoices with order_ref=order-ootle-fail = %d, want 0 (no orphan row on instantiation failure)", count)
	}
}

// TestCreateOotle_IdempotentWhileNonTerminal mirrors
// TestCreate_IdempotentWhileNonTerminal's L1 precedent for CreateOotle: two calls with
// the same order_ref while the first invoice is still non-terminal must return the
// SAME invoice, without calling instantiate a second time (no second on-chain
// component minted for the same order).
func TestCreateOotle_IdempotentWhileNonTerminal(t *testing.T) {
	s := setupStore(t, nil)
	ctx := context.Background()

	calls := 0
	instantiate := func(ctx context.Context, paymentID string, resourceAddress string, expectedAmount uint64) (string, error) {
		calls++
		return "component_" + paymentID, nil
	}

	first, err := s.CreateOotle(ctx, "order-ootle-idempotent", "resource_00", 1000, time.Hour, instantiate)
	if err != nil {
		t.Fatalf("CreateOotle() #1 error = %v", err)
	}

	second, err := s.CreateOotle(ctx, "order-ootle-idempotent", "resource_11", 9999, time.Hour, instantiate)
	if err != nil {
		t.Fatalf("CreateOotle() #2 error = %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("CreateOotle() #2 ID = %v, want it to equal CreateOotle() #1's ID %v (idempotent retry must return the same invoice)", second.ID, first.ID)
	}
	if calls != 1 {
		t.Errorf("instantiate called %d time(s), want 1 (idempotent retry must not instantiate a second on-chain component)", calls)
	}

	var count int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM invoices WHERE order_ref = $1`, "order-ootle-idempotent").Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("invoices with order_ref=order-ootle-idempotent = %d, want 1 (no second row created)", count)
	}
}

// TestFlagTrapdoor_And_ClearTrapdoor covers both new methods (task brief section 6):
// FlagTrapdoor sets the column true (and is idempotent), ClearTrapdoor sets it back
// false, and both return ErrNotFound for an unknown id.
func TestFlagTrapdoor_And_ClearTrapdoor(t *testing.T) {
	s := setupStore(t, nil)
	ctx := context.Background()

	instantiate := func(ctx context.Context, paymentID string, resourceAddress string, expectedAmount uint64) (string, error) {
		return "component_" + paymentID, nil
	}
	inv, err := s.CreateOotle(ctx, "order-trapdoor", "resource_00", 1000, time.Hour, instantiate)
	if err != nil {
		t.Fatalf("CreateOotle() error = %v", err)
	}
	if inv.OotleTrapdoorFlagged {
		t.Fatalf("OotleTrapdoorFlagged on creation = true, want false")
	}

	if err := s.FlagTrapdoor(ctx, inv.ID); err != nil {
		t.Fatalf("FlagTrapdoor() error = %v", err)
	}
	got, err := s.GetByID(ctx, inv.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if !got.OotleTrapdoorFlagged {
		t.Errorf("OotleTrapdoorFlagged after FlagTrapdoor() = false, want true")
	}

	// Idempotent: flagging an already-flagged invoice again is a harmless no-op.
	if err := s.FlagTrapdoor(ctx, inv.ID); err != nil {
		t.Fatalf("FlagTrapdoor() (second call) error = %v", err)
	}

	if err := s.ClearTrapdoor(ctx, inv.ID); err != nil {
		t.Fatalf("ClearTrapdoor() error = %v", err)
	}
	got, err = s.GetByID(ctx, inv.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got.OotleTrapdoorFlagged {
		t.Errorf("OotleTrapdoorFlagged after ClearTrapdoor() = true, want false")
	}

	if err := s.FlagTrapdoor(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("FlagTrapdoor(unknown id) error = %v, want ErrNotFound", err)
	}
	if err := s.ClearTrapdoor(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("ClearTrapdoor(unknown id) error = %v, want ErrNotFound", err)
	}
}

// TestListNonTerminalOotle covers the Ootle-scoped ListNonTerminalOotle: it must
// return only chain='ootle' non-terminal invoices, excluding both L1 invoices AND
// terminal Ootle invoices.
func TestListNonTerminalOotle(t *testing.T) {
	s := setupStore(t, nil)
	ctx := context.Background()

	instantiate := func(ctx context.Context, paymentID string, resourceAddress string, expectedAmount uint64) (string, error) {
		return "component_" + paymentID, nil
	}

	ootlePending, err := s.CreateOotle(ctx, "order-ootle-list-pending", "resource_00", 100, time.Hour, instantiate)
	if err != nil {
		t.Fatalf("CreateOotle(pending) error = %v", err)
	}
	ootleConfirmed, err := s.CreateOotle(ctx, "order-ootle-list-confirmed", "resource_00", 100, time.Hour, instantiate)
	if err != nil {
		t.Fatalf("CreateOotle(confirmed) error = %v", err)
	}
	if err := s.UpdateStatus(ctx, ootleConfirmed.ID, StatusConfirmed, nil); err != nil {
		t.Fatalf("UpdateStatus(confirmed): %v", err)
	}
	l1Pending, err := s.Create(ctx, "order-l1-list-pending", 100, time.Hour)
	if err != nil {
		t.Fatalf("Create(l1 pending) error = %v", err)
	}

	got, err := s.ListNonTerminalOotle(ctx)
	if err != nil {
		t.Fatalf("ListNonTerminalOotle() error = %v", err)
	}

	gotIDs := map[uuid.UUID]bool{}
	for _, inv := range got {
		if inv.Chain != ChainOotle {
			t.Errorf("ListNonTerminalOotle() returned a non-ootle invoice %s (chain=%q)", inv.ID, inv.Chain)
		}
		gotIDs[inv.ID] = true
	}

	if !gotIDs[ootlePending.ID] {
		t.Errorf("ListNonTerminalOotle() missing non-terminal ootle invoice %s", ootlePending.ID)
	}
	if gotIDs[ootleConfirmed.ID] {
		t.Errorf("ListNonTerminalOotle() should not include terminal ootle invoice %s", ootleConfirmed.ID)
	}
	if gotIDs[l1Pending.ID] {
		t.Errorf("ListNonTerminalOotle() should not include L1 invoice %s", l1Pending.ID)
	}
}
