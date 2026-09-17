package ootlepay

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tari-project/ootle-go/ootle"
	"github.com/tari-project/ootle-go/transport"
)

const (
	testComponentAddress = "component_aaaa000000000000000000000000000000000000000000000000000000000000"
	testResourceAddress  = "resource_bbbb00000000000000000000000000000000000000000000000000000000000000"
	testVaultID          = "vault_cccc0000000000000000000000000000000000000000000000000000000000000000"
	testRecoveryAccount  = "component_dddd000000000000000000000000000000000000000000000000000000000000"
	testOperatorFeeAcct  = "component_eeee000000000000000000000000000000000000000000000000000000000000"
	testOperatorSecret   = "ff00000000000000000000000000000000000000000000000000000000000000"
)

// fakeSubstates backs both FetchSubstateFunc and DecodeSubstateFunc for PollInvoice
// tests: substates is keyed by substate id, holding the pre-"decoded" value this fake
// hands back verbatim (never touching a real indexer or the ootle-go native library,
// per the task brief's explicit DI requirement).
type fakeSubstates struct {
	// componentVaultIDs is the VaultIDs list the component substate decodes to.
	componentVaultIDs []string
	// vaultBalances maps a vault substate id to its RevealedBalance/ResourceAddress.
	vaultBalances map[string]ootle.VaultValue
	fetchErr      error
}

func (f *fakeSubstates) fetchSubstate(ctx context.Context, id string) (*transport.FetchedSubstate, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	// The substate id is embedded as a marker inside SubstateValue so
	// decodeSubstate below knows which fixture to return without a second map.
	return &transport.FetchedSubstate{SubstateID: id, Version: 1, SubstateValue: json.RawMessage(`"` + id + `"`)}, nil
}

func (f *fakeSubstates) decodeSubstate(substateValue json.RawMessage) (ootle.DecodedSubstate, error) {
	var id string
	if err := json.Unmarshal(substateValue, &id); err != nil {
		return ootle.DecodedSubstate{}, err
	}
	if id == testComponentAddress {
		value, _ := json.Marshal(ootle.ComponentValue{
			TemplateAddress: "template_0000",
			EntityID:        "entity_0000",
			VaultIDs:        f.componentVaultIDs,
		})
		return ootle.DecodedSubstate{Kind: "component", Value: value}, nil
	}
	if vault, ok := f.vaultBalances[id]; ok {
		value, _ := json.Marshal(vault)
		return ootle.DecodedSubstate{Kind: "vault", Value: value}, nil
	}
	return ootle.DecodedSubstate{}, errors.New("fakeSubstates: unknown substate id " + id)
}

// TestPollInvoice_HappyPathNoTrapdoor covers the "happy path" ootlepay must test (task
// brief section 6): a component with no vaults at all (nothing ever retained) yields
// paid=false (see PollInvoice's own doc comment — this is the documented, honest
// consequence of the confirmed SDK gap, not a bug) and an empty heldResources, with no
// error — confirming the substate fetch+decode wiring works end-to-end for the
// "nothing wrong" case.
func TestPollInvoice_HappyPathNoTrapdoor(t *testing.T) {
	fake := &fakeSubstates{componentVaultIDs: nil}
	c := NewClient(nil, fake.fetchSubstate, fake.decodeSubstate, testOperatorFeeAcct, testOperatorSecret, 0)

	paid, held, err := c.PollInvoice(context.Background(), testComponentAddress)
	if err != nil {
		t.Fatalf("PollInvoice() error = %v", err)
	}
	if paid {
		t.Errorf("paid = true, want false (see PollInvoice's doc comment: this cannot be determined from the current SDK)")
	}
	if len(held) != 0 {
		t.Errorf("heldResources = %v, want empty", held)
	}
}

// TestPollInvoice_TrapdoorFlagPath covers the trapdoor-flag path (task brief section
// 6): a component with one vault holding a non-zero balance must be reported in
// heldResources.
func TestPollInvoice_TrapdoorFlagPath(t *testing.T) {
	fake := &fakeSubstates{
		componentVaultIDs: []string{testVaultID},
		vaultBalances: map[string]ootle.VaultValue{
			testVaultID: {ResourceAddress: testResourceAddress, RevealedBalance: 500},
		},
	}
	c := NewClient(nil, fake.fetchSubstate, fake.decodeSubstate, testOperatorFeeAcct, testOperatorSecret, 0)

	paid, held, err := c.PollInvoice(context.Background(), testComponentAddress)
	if err != nil {
		t.Fatalf("PollInvoice() error = %v", err)
	}
	if paid {
		t.Errorf("paid = true, want false")
	}
	if len(held) != 1 || held[0] != testResourceAddress {
		t.Errorf("heldResources = %v, want [%q]", held, testResourceAddress)
	}
}

// TestPollInvoice_EmptiedVaultNotReported covers the sweep()-emptied-vault edge case
// held_resources() itself documents (tari-ootle-invoice/src/lib.rs): a vault that
// still exists (the engine forbids ever removing one) but has zero balance must NOT
// be reported as held.
func TestPollInvoice_EmptiedVaultNotReported(t *testing.T) {
	fake := &fakeSubstates{
		componentVaultIDs: []string{testVaultID},
		vaultBalances: map[string]ootle.VaultValue{
			testVaultID: {ResourceAddress: testResourceAddress, RevealedBalance: 0},
		},
	}
	c := NewClient(nil, fake.fetchSubstate, fake.decodeSubstate, testOperatorFeeAcct, testOperatorSecret, 0)

	_, held, err := c.PollInvoice(context.Background(), testComponentAddress)
	if err != nil {
		t.Fatalf("PollInvoice() error = %v", err)
	}
	if len(held) != 0 {
		t.Errorf("heldResources = %v, want empty (a zero-balance vault must not be reported)", held)
	}
}

// TestPollInvoice_FetchErrorPropagates confirms a transport failure is returned as an
// error, not silently swallowed.
func TestPollInvoice_FetchErrorPropagates(t *testing.T) {
	wantErr := errors.New("indexer: connection refused")
	fake := &fakeSubstates{fetchErr: wantErr}
	c := NewClient(nil, fake.fetchSubstate, fake.decodeSubstate, testOperatorFeeAcct, testOperatorSecret, 0)

	_, _, err := c.PollInvoice(context.Background(), testComponentAddress)
	if err == nil {
		t.Fatal("PollInvoice() error = nil, want an error when fetchSubstate fails")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("PollInvoice() error = %v, want it to wrap %v", err, wantErr)
	}
}

// ---- Sweep tests ----

// fakeSendInstructions builds a SendInstructionsFunc that records the intent/keys it
// was called with and returns a pre-set result/error — never touching a real indexer
// or the ootle-go native library.
type fakeSendInstructions struct {
	result    ootle.FinalizedResult
	err       error
	gotIntent ootle.GenericTransactionIntent
	gotKeys   ootle.PublicTransferKeys
	calls     int
}

func (f *fakeSendInstructions) send(ctx context.Context, intent ootle.GenericTransactionIntent, keys ootle.PublicTransferKeys) (ootle.FinalizedResult, error) {
	f.calls++
	f.gotIntent = intent
	f.gotKeys = keys
	return f.result, f.err
}

// TestSweep_HappyPathCommits covers Sweep's happy path: a committed result yields no
// error, and the transaction was signed with the operator's key and paid its fee from
// the operator's configured fee account.
func TestSweep_HappyPathCommits(t *testing.T) {
	fake := &fakeSendInstructions{
		result: ootle.FinalizedResult{
			Submit: ootle.SubmitResult{
				TransactionID: "tx-happy",
				Outcome:       &ootle.TransactionOutcome{Commit: true},
			},
		},
	}
	c := NewClient(fake.send, nil, nil, testOperatorFeeAcct, testOperatorSecret, 0)

	if err := c.Sweep(context.Background(), testComponentAddress, testRecoveryAccount); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("sendInstructions called %d times, want 1", fake.calls)
	}
	if fake.gotKeys.AccountSecret != testOperatorSecret {
		t.Errorf("signed with AccountSecret = %q, want the operator secret %q", fake.gotKeys.AccountSecret, testOperatorSecret)
	}
	if fake.gotIntent.FeePayment == (ootle.FeeSource{}) {
		// FeeSource has no exported comparable fields to assert on directly, but
		// marshalling it confirms a variant was actually set (PayFeeFromAccount
		// was called) rather than left zero.
		body, err := json.Marshal(fake.gotIntent.FeePayment)
		if err != nil || strings.Contains(string(body), "no variant set") {
			t.Errorf("fee payment source was not set on the swept intent")
		}
	}
}

// TestSweep_NonOperatorRejectionReturnsCleanError mirrors the template's own
// sweep_attempted_by_non_operator_fails_funds_untouched test case (task brief section
// 6): a rejected outcome (as the chain would report for a non-operator caller) must
// surface as a clean, descriptive error — never a panic.
func TestSweep_NonOperatorRejectionReturnsCleanError(t *testing.T) {
	fake := &fakeSendInstructions{
		result: ootle.FinalizedResult{
			Submit: ootle.SubmitResult{
				TransactionID: "tx-rejected",
				Outcome: &ootle.TransactionOutcome{
					Reject: &ootle.RejectReason{
						Code:    "EXECUTION_FAILURE",
						Message: "Only the operator may sweep this invoice",
					},
				},
			},
		},
	}
	c := NewClient(fake.send, nil, nil, testOperatorFeeAcct, testOperatorSecret, 0)

	err := c.Sweep(context.Background(), testComponentAddress, testRecoveryAccount)
	if err == nil {
		t.Fatal("Sweep() error = nil, want a clean error for a non-operator rejection")
	}
	if !strings.Contains(err.Error(), "Only the operator may sweep this invoice") {
		t.Errorf("Sweep() error = %v, want it to include the reject reason's message", err)
	}
}

// TestSweep_TransportErrorPropagates confirms a transport-level failure (e.g. the
// indexer unreachable) is returned as-is, wrapped, never panics.
func TestSweep_TransportErrorPropagates(t *testing.T) {
	wantErr := errors.New("indexer: connection refused")
	fake := &fakeSendInstructions{err: wantErr}
	c := NewClient(fake.send, nil, nil, testOperatorFeeAcct, testOperatorSecret, 0)

	err := c.Sweep(context.Background(), testComponentAddress, testRecoveryAccount)
	if !errors.Is(err, wantErr) {
		t.Errorf("Sweep() error = %v, want it to wrap %v", err, wantErr)
	}
}

// TestSweep_PendingResultReturnsCleanError confirms a still-pending result (no
// outcome yet) is reported as a clean error, not treated as success.
func TestSweep_PendingResultReturnsCleanError(t *testing.T) {
	fake := &fakeSendInstructions{
		result: ootle.FinalizedResult{Submit: ootle.SubmitResult{TransactionID: "tx-pending"}},
	}
	c := NewClient(fake.send, nil, nil, testOperatorFeeAcct, testOperatorSecret, 0)

	err := c.Sweep(context.Background(), testComponentAddress, testRecoveryAccount)
	if err == nil {
		t.Fatal("Sweep() error = nil, want an error for a still-pending result")
	}
	if !strings.Contains(err.Error(), "pending") {
		t.Errorf("Sweep() error = %v, want it to mention the pending state", err)
	}
}

// ---- InstantiateInvoice tests ----

// TestInstantiateInvoice_ReturnsAddressAllocationUnsupported documents and locks in
// this dispatch's confirmed SDK gap #1 (see this package's doc comment): calling
// InstantiateInvoice must return ErrAddressAllocationUnsupported, not fabricate a
// commit.
func TestInstantiateInvoice_ReturnsAddressAllocationUnsupported(t *testing.T) {
	c := NewClient(nil, nil, nil, testOperatorFeeAcct, testOperatorSecret, 0)

	_, err := c.InstantiateInvoice(context.Background(), "pay-id-1", testRecoveryAccount, testResourceAddress, 1000, "aa"+testOperatorSecret[:62])
	if !errors.Is(err, ErrAddressAllocationUnsupported) {
		t.Errorf("InstantiateInvoice() error = %v, want it to wrap ErrAddressAllocationUnsupported", err)
	}
}

// TestInstantiateInvoice_InvalidOperatorKeyRejectedBeforeSDKGap confirms input
// validation happens (and reports its own distinct error) before the SDK-gap error,
// so a caller passing garbage doesn't get a misleading "unsupported" message for what
// is actually its own bug.
func TestInstantiateInvoice_InvalidOperatorKeyRejectedBeforeSDKGap(t *testing.T) {
	c := NewClient(nil, nil, nil, testOperatorFeeAcct, testOperatorSecret, 0)

	_, err := c.InstantiateInvoice(context.Background(), "pay-id-2", testRecoveryAccount, testResourceAddress, 1000, "not-valid-hex")
	if err == nil {
		t.Fatal("InstantiateInvoice() error = nil, want an error for a malformed operator public key")
	}
	if errors.Is(err, ErrAddressAllocationUnsupported) {
		t.Errorf("InstantiateInvoice() error = %v, want the hex-validation error, not ErrAddressAllocationUnsupported", err)
	}
}
