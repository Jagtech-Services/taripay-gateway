// Package ootlepay is taripay-gateway's Ootle L2 client wrapper (task brief "Ootle L2
// stablecoin/native-resource payment support in taripay-gateway", section 3): it drives
// the Snipa22/tari-ootle-invoice template's Invoice component (commit 5f5493f on that
// repo's main, cloned read-only to verify the exact ABI cited throughout this file —
// see src/lib.rs's `new_with_address`/`pay`/`sweep`/accessor methods) through
// github.com/tari-project/ootle-go v0.2.0 (module paths .../ootle and .../transport),
// instantiating one Invoice component per Ootle-chain invoice, polling it for
// payment/trapdoor state, and sweeping trapdoor-held funds on operator request.
//
// # Verified ootle-go API surface
//
// Every ootle-go/transport call this package makes was read directly out of the
// vendored module (github.com/tari-project/ootle-go@v0.2.0, go.sum-pinned; also
// diffed against that repo's unreleased main, commit bbc556f, which changes nothing
// relevant here) — not guessed at, per the task brief's explicit "do not fabricate
// the Go client API" instruction:
//
//   - (*ootle.Client).SendInstructions(ctx, ootle.GenericTransactionIntent,
//     ootle.PublicTransferKeys) (ootle.FinalizedResult, error) — ootle-go's
//     ootle/driver_generic.go:22. Builds the generic intent, resolves inputs
//     (bounded two-phase loop), seals, submits, and waits for finality — the
//     production path examples/template_invoke/main.go and
//     examples/counter_deploy/main.go both use for a real CallFunction/CallMethod.
//   - ootle.NewTransaction().PayFeeFromAccount(component, feeMicroTari).
//     CallMethod(component, method, args...).Intent() — ootle/transaction_builder.go;
//     confirmed against counter_deploy's own two-transaction (`new` then `increase`)
//     pattern.
//   - ootle.ArgAddress(string)/ootle.ArgAmount(uint64) — ootle/instructions.go's typed
//     Arg* DSL.
//   - ootle.FinalizedResult.IsCommit()/RejectReason()/Pending() — ootle/result.go;
//     confirmed against examples/internal/common.MustCommit's own usage.
//   - (*transport.Client).FetchSubstate(ctx, id) (*transport.FetchedSubstate, error) —
//     transport/transport.go:269; confirmed against counter_deploy's own substate
//     fetch after calling increase().
//   - ootle.DecodeSubstate(substateValue json.RawMessage) (ootle.DecodedSubstate,
//     error), and its Kind=="component" (ComponentValue{TemplateAddress, EntityID,
//     VaultIDs}) / Kind=="vault" (VaultValue{ResourceAddress, RevealedBalance,
//     ConfidentialCommitmentCount, VaultKind}) shapes — ootle/substate.go; confirmed
//     against counter_deploy's own DecodeSubstate call on a fetched component.
//
// # Confirmed SDK gap #1: InstantiateInvoice cannot currently commit a real transaction
//
// Invoice::new_with_address's first parameter is a `ComponentAddressAllocation`
// (tari-ootle-invoice/src/lib.rs), obtained ONLY via a preceding "pre-allocate the
// address" instruction bound to a workspace label — confirmed directly against that
// template's own integration test harness
// (tari-ootle-invoice/tests/test.rs's `setup()`: `test.transaction()
// .allocate_component_address("invoice_addr").call_function(invoice_template,
// "new_with_address", args![Workspace("invoice_addr"), ...])`).
//
// ootle-go v0.2.0's public Go API has no equivalent builder: ootle/instructions.go's
// InstructionSpec type has exactly five externally-tagged variants (CallFunction,
// CallMethod, CreateAccount, PublishTemplate, PutLastInstructionOutputOnWorkspace),
// all of InstructionSpec's fields are unexported, and there is no exported
// constructor for a sixth "AllocateAddress" variant — confirmed by reading every
// exported symbol in that file and in transaction_builder.go. The underlying native
// core library DOES support this (an `AllocateAddress { substate_type: ... }`
// engine instruction and an `AddressAllocationInvokeArg` arg type are both present as
// literal strings in the vendored
// internal/cffi/lib/linux_amd64/libootle_sdk_ffi_c.a, confirmed via `strings`), but
// ootle-go has not yet wrapped either. transport.Client.Submit only accepts an
// already-sealed base64 envelope (transport/transport.go's own doc comment: "does I/O
// and JSON marshalling and nothing else... no encoding... [that] lives in the Rust
// core"), and sealing happens exclusively inside ootle.Client.SendInstructions via the
// unexported internal/cffi package — so there is also no raw-REST/JSON fallback
// available here (unlike the task brief's anticipated fallback #2, which assumed a
// typed-builder gap could be worked around at the REST layer; that path is closed too,
// because building/sealing an envelope is not something transport.Client, or anything
// reachable from outside ootle-go, can do without the missing instruction builder).
//
// InstantiateInvoice below is therefore wired to the exact DI seam
// internal/invoice.InstantiateOotleInvoiceFunc expects, and returns
// ErrAddressAllocationUnsupported (see that var's own doc comment) rather than
// fabricating a call to a method that doesn't exist. This is this dispatch's #1 open
// question for the project owner (Alex): either ootle-go needs to add the missing
// builder, or a different, already-supported instantiation path needs to be designed
// (e.g. a template constructor that doesn't require pre-allocation).
//
// # Confirmed SDK gap #2: PollInvoice cannot detect the happy-path "paid" transition
//
// is_paid()/held_resources()/held_balance() are ordinary `&self` accessor methods —
// calling them requires a CallMethod transaction (a dry run works, since they mutate
// nothing), but ootle.FinalizedResult (result.go) exposes NO method-return-value
// decoding at all: only Submit (id + commit/reject outcome), FeeReceipt, DiffSummary
// (created/destroyed substate ids+versions, not their contents), Events, and Logs.
// Confirmed by reading every field on FinalizedResult/DiffSummary and by noting that
// examples/template_invoke's own total_supply() call never reads back its result
// either — it only confirms the call committed. Grepping this package's own vendored
// source for "return_value"/"ReturnValue"/"InstructionResult" turns up nothing.
//
// Substate-level decoding doesn't help either: ootle.DecodedSubstate's Kind=="component"
// shape (ComponentValue) exposes only {TemplateAddress, EntityID, VaultIDs} — a
// template-agnostic view with no access to a specific template's own scalar state
// fields (Invoice's `paid: bool`). And the happy path's own funds movement
// (`ComponentManager::get(self.merchant_account).invoke("deposit", ...)`, per
// tari-ootle-invoice/src/lib.rs's `pay()`) touches the MERCHANT account's substate, not
// the Invoice component's own — so even watching the Invoice component's own vaults
// (which this package DOES do, for the trapdoor signal — see PollInvoice below) gives
// no signal on the happy path, and the merchant account is shared across every invoice
// for this single-tenant gateway, so a balance delta there can't be attributed to one
// specific invoice either. The template also never calls `Runtime::emit_event`
// anywhere (confirmed by reading the whole of tari-ootle-invoice/src/lib.rs), so
// ootle.WatchEvents' SSE stream has nothing template-specific to watch for on this
// component either.
//
// PollInvoice below is therefore honest about this: it real-detects the trapdoor
// signal (held_resources(), via substate fetch+decode of the Invoice component's own
// vault ids — see PollInvoice's doc comment for exactly which real calls this uses),
// and always reports paid=false (documented, not a bug) since that value genuinely
// cannot be determined from ootle-go v0.2.0's public surface today. This is this
// dispatch's #2 open question for Alex: PollOotleInvoices (internal/eventwatcher) will
// only ever flag trapdoors, never auto-confirm an Ootle invoice, until either ootle-go
// adds method-return-value decoding or the template is changed to emit a domain event
// on payment.
package ootlepay

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tari-project/ootle-go/ootle"
	"github.com/tari-project/ootle-go/transport"
)

// SendInstructionsFunc matches (*ootle.Client).SendInstructions's exact signature —
// confirmed against ootle-go@v0.2.0/ootle/driver_generic.go:22 (see this package's doc
// comment). Tests inject a fake instead of the real method so they never touch a real
// indexer or the ootle-go native (cffi) library.
type SendInstructionsFunc func(ctx context.Context, intent ootle.GenericTransactionIntent, keys ootle.PublicTransferKeys) (ootle.FinalizedResult, error)

// FetchSubstateFunc matches (*transport.Client).FetchSubstate's exact signature —
// confirmed against ootle-go@v0.2.0/transport/transport.go:269 (see this package's
// doc comment). Tests inject a fake instead of the real method so they never touch a
// real indexer.
type FetchSubstateFunc func(ctx context.Context, id string) (*transport.FetchedSubstate, error)

// DecodeSubstateFunc matches ootle.DecodeSubstate's exact signature — confirmed
// against ootle-go@v0.2.0/ootle/substate.go:69 (see this package's doc comment). It is
// a local/native call (no network I/O — it calls into the vendored ootle-go native
// library, not the indexer), but is still injected here so this package's own tests
// never need to link that native library either.
type DecodeSubstateFunc func(substateValue json.RawMessage) (ootle.DecodedSubstate, error)

// ErrAddressAllocationUnsupported is returned by InstantiateInvoice: see this
// package's doc comment's "Confirmed SDK gap #1" section for the full, cited
// explanation of exactly what ootle-go v0.2.0 is missing and how that was verified
// (not guessed).
var ErrAddressAllocationUnsupported = errors.New("ootlepay: github.com/tari-project/ootle-go v0.2.0 exposes no address-pre-allocation instruction builder, so Invoice::new_with_address cannot currently be called from Go — see this package's doc comment (\"Confirmed SDK gap #1\") for the full citation trail; flagged as an open question for the project owner")

// DefaultFeeMicroTari is the flat µTari fee this package pays on every submitted
// transaction when NewClient is given feeMicroTari == 0. PLACEHOLDER, UNCONFIRMED
// value — same "reasonable-sounding guess, not project-owner-approved" caveat this
// repo's other placeholder tunables carry (see e.g. internal/config's
// DefaultConfirmationDepth doc comment). Matches the ballpark ootle-go's own examples
// use for a simple CallMethod (examples/counter_deploy and
// examples/template_invoke use 3000-5000 µTari per instruction).
const DefaultFeeMicroTari uint64 = 3000

// Client wraps this package's three Ootle operations (InstantiateInvoice, PollInvoice,
// Sweep) over injected func-types, so tests never touch a real indexer or the ootle-go
// native library (per the task brief's explicit DI requirement). Concrete production
// wiring — a real *ootle.Client + *transport.Client bound to the merchant's configured
// indexer — happens in cmd/gateway/main.go, mirroring exactly how
// walletGRPC.StreamTransactionEvents/GetPaymentIdAddress are wired into
// internal/eventwatcher/internal/invoice today.
type Client struct {
	sendInstructions SendInstructionsFunc
	fetchSubstate    FetchSubstateFunc
	decodeSubstate   DecodeSubstateFunc

	// operatorFeeAccount is the on-ledger account component (component_<hex>) that
	// pays the µTari fee for every operator-signed transaction this package submits
	// (Sweep; InstantiateInvoice, once ErrAddressAllocationUnsupported is resolved
	// upstream) — config-driven (internal/config.Config's Ootle fields), per
	// AGENTS.md's no-hardcoded-assumptions rule.
	operatorFeeAccount string
	// operatorSecretHex is the operator's account secret key (lowercase hex), used to
	// sign every operator-initiated transaction (ootle.PublicTransferKeys.AccountSecret)
	// — config-driven, sourced the same way internal/config already sources other
	// secrets (env var / TOML file, never a CLI flag — see internal/config.Flags' own
	// doc comment on PostgresDSN/WebhookHMACSecret for why).
	operatorSecretHex string
	// feeMicroTari is the flat fee (µTari) this Client pays on every submitted
	// transaction. See DefaultFeeMicroTari's doc comment.
	feeMicroTari uint64
}

// NewClient builds a Client. feeMicroTari == 0 uses DefaultFeeMicroTari.
func NewClient(sendInstructions SendInstructionsFunc, fetchSubstate FetchSubstateFunc, decodeSubstate DecodeSubstateFunc, operatorFeeAccount, operatorSecretHex string, feeMicroTari uint64) *Client {
	if feeMicroTari == 0 {
		feeMicroTari = DefaultFeeMicroTari
	}
	return &Client{
		sendInstructions:   sendInstructions,
		fetchSubstate:      fetchSubstate,
		decodeSubstate:     decodeSubstate,
		operatorFeeAccount: operatorFeeAccount,
		operatorSecretHex:  operatorSecretHex,
		feeMicroTari:       feeMicroTari,
	}
}

// InstantiateInvoice is internal/invoice.InstantiateOotleInvoiceFunc's real
// implementation — BLOCKED, see this package's doc comment ("Confirmed SDK gap #1").
// paymentID/resourceAddress/expectedAmount vary per invoice; merchantAccount and
// operatorPublicKeyHex are this single-tenant gateway's constant config
// (internal/config.Config.OotleMerchantAccount/OotleOperatorPublicKey), matching
// Invoice::new_with_address's exact parameter order (merchant_account,
// expected_resource, expected_amount, payment_id, operator).
//
// A real implementation, once ootle-go exposes the missing address-allocation
// builder (see ErrAddressAllocationUnsupported's doc comment), would look like:
//
//	operatorPubKeyBytes, err := hex.DecodeString(operatorPublicKeyHex)
//	intent := ootle.NewTransaction().
//		PayFeeFromAccount(c.operatorFeeAccount, c.feeMicroTari).
//		<AllocateAddress instruction not yet exposed by ootle-go>("invoice_addr").
//		CallFunction(invoiceTemplateAddress, "new_with_address",
//			ootle.ArgWorkspace("invoice_addr"),
//			ootle.ArgAddress(merchantAccount),
//			ootle.ArgAddress(resourceAddress),
//			ootle.ArgAmount(expectedAmount),
//			ootle.ArgString(paymentID),
//			ootle.ArgBytes(operatorPubKeyBytes),
//		).
//		Intent()
//	res, err := c.sendInstructions(ctx, intent, ootle.PublicTransferKeys{AccountSecret: c.operatorSecretHex})
//	component, ok := res.DiffSummary.NewComponent()
//
// This function validates what it can (operatorPublicKeyHex must actually be valid
// hex) and otherwise returns ErrAddressAllocationUnsupported rather than fabricating
// a call to a builder that doesn't exist.
func (c *Client) InstantiateInvoice(ctx context.Context, paymentID, merchantAccount, resourceAddress string, expectedAmount uint64, operatorPublicKeyHex string) (string, error) {
	if _, err := hex.DecodeString(operatorPublicKeyHex); err != nil {
		return "", fmt.Errorf("ootlepay: instantiate invoice for payment_id %s: operator public key %q is not valid hex: %w", paymentID, operatorPublicKeyHex, err)
	}
	if merchantAccount == "" {
		return "", fmt.Errorf("ootlepay: instantiate invoice for payment_id %s: merchant account address is required", paymentID)
	}
	return "", fmt.Errorf("ootlepay: instantiate invoice for payment_id %s: %w", paymentID, ErrAddressAllocationUnsupported)
}

// PollInvoice queries the indexer for componentAddress's current state (task brief
// section 3): fetch+decode the Invoice component's own substate directly, per the
// task brief's explicit "your call, document which" allowance — chosen over calling
// held_resources() as a template method because ootle.FinalizedResult exposes no
// method-return-value decoding at all (see this package's doc comment, "Confirmed SDK
// gap #2"), so a substate-level read is not just cheaper, it's the only real,
// verified option available at all for this part.
//
// heldResources is computed for real: fetch the Invoice component's own substate
// (c.fetchSubstate), decode it (c.decodeSubstate) to get its ComponentValue.VaultIDs
// (the trapdoor's own vault ids, in state order — see tari-ootle-invoice/src/lib.rs's
// `vaults: Vec<(ResourceAddress, Vault)>` field), then fetch+decode each vault
// substate to get its VaultValue{ResourceAddress, RevealedBalance}; any vault with a
// non-zero RevealedBalance is "held" (mirrors held_resources()'s own
// `vault.balance() > Amount::zero()` filter). A component with zero vaults (nothing
// ever retained) yields an empty, non-nil-checked heldResources with no error.
//
// paid is ALWAYS false from this real implementation — see this package's doc
// comment, "Confirmed SDK gap #2", for exactly why that value cannot currently be
// determined from ootle-go's public API. This is not a bug: it is the honest,
// documented consequence of a confirmed SDK limitation, not a silently wrong guess.
func (c *Client) PollInvoice(ctx context.Context, componentAddress string) (paid bool, heldResources []string, err error) {
	component, err := c.fetchSubstate(ctx, componentAddress)
	if err != nil {
		return false, nil, fmt.Errorf("ootlepay: poll invoice: fetch component %s: %w", componentAddress, err)
	}
	if component == nil {
		return false, nil, fmt.Errorf("ootlepay: poll invoice: component %s not found", componentAddress)
	}

	decoded, err := c.decodeSubstate(component.SubstateValue)
	if err != nil {
		return false, nil, fmt.Errorf("ootlepay: poll invoice: decode component %s: %w", componentAddress, err)
	}
	if decoded.Kind != "component" {
		return false, nil, fmt.Errorf("ootlepay: poll invoice: substate %s is kind %q, want %q", componentAddress, decoded.Kind, "component")
	}
	var comp ootle.ComponentValue
	if err := json.Unmarshal(decoded.Value, &comp); err != nil {
		return false, nil, fmt.Errorf("ootlepay: poll invoice: unmarshal component value for %s: %w", componentAddress, err)
	}

	held := make([]string, 0, len(comp.VaultIDs))
	for _, vaultID := range comp.VaultIDs {
		vaultSubstate, err := c.fetchSubstate(ctx, vaultID)
		if err != nil {
			return false, nil, fmt.Errorf("ootlepay: poll invoice: fetch vault %s (component %s): %w", vaultID, componentAddress, err)
		}
		if vaultSubstate == nil {
			// A vault id the component itself listed but the indexer can't find is
			// an inconsistent read (e.g. a race with a not-yet-indexed commit) —
			// surfaced as an error rather than silently treated as "not held", so a
			// caller doesn't wrongly skip a trapdoor flag on a transient blip.
			return false, nil, fmt.Errorf("ootlepay: poll invoice: vault %s (component %s) not found", vaultID, componentAddress)
		}
		vaultDecoded, err := c.decodeSubstate(vaultSubstate.SubstateValue)
		if err != nil {
			return false, nil, fmt.Errorf("ootlepay: poll invoice: decode vault %s (component %s): %w", vaultID, componentAddress, err)
		}
		if vaultDecoded.Kind != "vault" {
			return false, nil, fmt.Errorf("ootlepay: poll invoice: substate %s is kind %q, want %q", vaultID, vaultDecoded.Kind, "vault")
		}
		var vault ootle.VaultValue
		if err := json.Unmarshal(vaultDecoded.Value, &vault); err != nil {
			return false, nil, fmt.Errorf("ootlepay: poll invoice: unmarshal vault value for %s: %w", vaultID, err)
		}
		if vault.RevealedBalance > 0 {
			held = append(held, vault.ResourceAddress)
		}
	}

	// paid is always false here — see this method's doc comment ("Confirmed SDK gap
	// #2"): it genuinely cannot be determined from ootle-go's public API today.
	return false, held, nil
}

// Sweep submits the Invoice component's `sweep(recovery_account)` transaction, signed
// with the operator's key (c.operatorSecretHex — config-driven, not hardcoded, per
// AGENTS.md). This is fully real and verified: a plain CallMethod with no return
// value the caller needs to decode (per this package's doc comment — the SDK gap only
// affects calls whose RESULT matters, not this one, whose success/failure is fully
// visible via FinalizedResult.IsCommit()/RejectReason()).
//
// A non-operator caller is rejected on-chain by the template itself
// (tari-ootle-invoice/src/lib.rs's `sweep()`: `assert_eq!(caller, self.operator, ...)`)
// — this surfaces here as a committed-fee-but-rejected-execution outcome (or a full
// reject, depending on how far execution got), reported as a clean error via
// RejectReason, never a panic. See this package's own tests for that exact scenario
// (mirroring the template's own sweep_attempted_by_non_operator_fails_funds_untouched
// test case, per the task brief).
func (c *Client) Sweep(ctx context.Context, componentAddress, recoveryAccount string) error {
	intent := ootle.NewTransaction().
		PayFeeFromAccount(c.operatorFeeAccount, c.feeMicroTari).
		CallMethod(componentAddress, "sweep", ootle.ArgAddress(recoveryAccount)).
		Intent()

	res, err := c.sendInstructions(ctx, intent, ootle.PublicTransferKeys{AccountSecret: c.operatorSecretHex})
	if err != nil {
		return fmt.Errorf("ootlepay: sweep %s: %w", componentAddress, err)
	}
	if res.Pending() {
		return fmt.Errorf("ootlepay: sweep %s: transaction %s still pending (no outcome)", componentAddress, res.Submit.TransactionID)
	}
	if !res.IsCommit() {
		reason := res.RejectReason()
		if reason != nil {
			return fmt.Errorf("ootlepay: sweep %s: transaction %s not committed: %s: %s", componentAddress, res.Submit.TransactionID, reason.Code, reason.Message)
		}
		return fmt.Errorf("ootlepay: sweep %s: transaction %s not committed (no reject reason given)", componentAddress, res.Submit.TransactionID)
	}
	return nil
}
