package order

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/agora/client/account"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/input"
)

const (
	// deriveKeyTimeout is the number of seconds we allow the wallet to take
	// to derive a key.
	deriveKeyTimeout = 10 * time.Second
)

var (
	// ErrMismatchErr is the wrapped error that is returned if the batch
	// verification fails.
	ErrMismatchErr = errors.New("batch verification result mismatch")
)

// MismatchErr is an error type that is returned if the batch verification on
// the client does not come up with the same result as the server.
type MismatchErr struct {
	msg   string
	cause error
}

// Unwrap returns the underlying error cause. This is always ErrMismatchErr so
// we can compare any error returned by the batch verifier with errors.Is() but
// still retain the context what exactly went wrong.
func (m *MismatchErr) Unwrap() error {
	return ErrMismatchErr
}

// Error returns the underlying error message.
//
// NOTE: This method is part of the error interface.
func (m *MismatchErr) Error() string {
	if m.cause == nil {
		return m.msg
	}
	return fmt.Sprintf("%s: %v", m.msg, m.cause)
}

// newMismatchErr return a new MismatchErr from the cause and the error message.
func newMismatchErr(cause error, msg string, args ...interface{}) error {
	return &MismatchErr{
		msg:   fmt.Sprintf(msg, args...),
		cause: cause,
	}
}

// batchVerifier is a type that implements BatchVerifier and can verify a batch
// from the point of view of the trader.
type batchVerifier struct {
	orderStore    Store
	getAccount    func(*btcec.PublicKey) (*account.Account, error)
	wallet        lndclient.WalletKitClient
	ourNodePubkey [33]byte
}

// Verify makes sure the batch prepared by the server is correct and can be
// accepted by the trader.
//
// NOTE: This method is part of the BatchVerifier interface.
func (v *batchVerifier) Verify(batch *Batch) error {
	// First of all, make sure we're using the same batch validation version
	// as the server. Otherwise we bail out of the batch. This should
	// already be handled when the client connects/authenticates. But
	// doesn't hurt to check again.
	if batch.Version != CurrentVersion {
		return ErrVersionMismatch
	}

	// First go through all orders that were matched for us. We'll make sure
	// we know of the order and that the numbers check out on a high level.
	tallies := make(map[[33]byte]*AccountTally)
	accounts := make(map[[33]byte]*account.Account)
	for nonce, theirOrders := range batch.MatchedOrders {
		// Find our order in the database.
		ourOrder, err := v.orderStore.GetOrder(nonce)
		if err != nil {
			return fmt.Errorf("order %v not found: %v", nonce, err)
		}

		// We'll index our account tallies by the serialized form of
		// the account key so some copying is necessary first.
		var (
			acctKey    = ourOrder.Details().AcctKey
			acctKeyRaw [33]byte
		)
		if acctKey == nil {
			return fmt.Errorf("account for order %v invalid", nonce)
		}
		copy(acctKeyRaw[:], acctKey.SerializeCompressed())

		// Find the account the order spends from, if it isn't already
		// in the cache because another order spends from it.
		tally, ok := tallies[acctKeyRaw]
		if !ok {
			acct, err := v.getAccount(acctKey)
			if err != nil {
				return fmt.Errorf("account %x not found: %v",
					acctKeyRaw, err)
			}
			tally = &AccountTally{
				EndingBalance: acct.Value,
			}
			tallies[acctKeyRaw] = tally
			accounts[acctKeyRaw] = acct
		}

		// Now that we know which of our orders were involved in the
		// match, we can start validating the match and tally up the
		// account balance, executed units and fee diffs.
		unitsFilled := SupplyUnit(0)
		for _, theirOrder := range theirOrders {
			// Verify order compatibility and fee structure.
			err = v.validateMatchedOrder(
				tally, ourOrder, theirOrder, batch.ExecutionFee,
				batch.ClearingPrice,
			)
			if err != nil {
				return newMismatchErr(
					err, "error matching against order %v",
					theirOrder.Order.Nonce(),
				)
			}

			// Make sure there is a channel output included in the
			// batch transaction that has the multisig script we
			// expect.
			err = v.validateChannelOutput(
				batch.BatchTX, ourOrder, theirOrder,
			)
			if err != nil {
				return newMismatchErr(
					err, "error finding channel output "+
						"for matched order %v",
					theirOrder.Order.Nonce(),
				)
			}

			// The match looks good, one channel output more to pay
			// chain fees for.
			tally.NumChansCreated++
			unitsFilled += theirOrder.UnitsFilled
		}

		// Last check is to make sure our order has not been over
		// filled somehow.
		if unitsFilled > ourOrder.Details().UnitsUnfulfilled {
			return &MismatchErr{
				msg: fmt.Sprintf("invalid units to be filled "+
					"for order %v. currently unfulfilled "+
					"%d, matched with %d in total",
					ourOrder.Nonce(),
					ourOrder.Details().UnitsUnfulfilled,
					unitsFilled,
				),
			}
		}
	}

	// Now that we know all the accounts that were involved in the batch,
	// we can make sure we got a diff for each of them.
	for _, diff := range batch.AccountDiffs {
		// We only should get diffs for accounts that have orders in the
		// batch. If not, something's messed up.
		tally, ok := tallies[diff.AccountKeyRaw]
		if !ok {
			return &MismatchErr{
				msg: fmt.Sprintf("got diff for uninvolved "+
					"account %x", diff.AccountKeyRaw),
			}
		}
		acct := accounts[diff.AccountKeyRaw]

		// Now that we know how many channels were created from the
		// given account, let's also account for the chain fees.
		tally.ChainFees(batch.BatchTxFeeRate)

		// Even if the account output is dust, we should arrive at the
		// same number with our tally as the server.
		if diff.EndingBalance != tally.EndingBalance {
			return &MismatchErr{
				msg: fmt.Sprintf("server sent unexpected "+
					"ending balance. got %d expected %d",
					diff.EndingBalance, tally.EndingBalance),
			}
		}

		// Make sure the ending state of the account is correct.
		err := diff.validateEndingState(batch.BatchTX, acct)
		if err != nil {
			return newMismatchErr(
				err, "account %x diff is incorrect",
				diff.AccountKeyRaw,
			)
		}

		// The expiry should be the same as it was before if the account
		// output has been recreated (which means the expiry has been
		// reset as the CSV starts counting again in the new output).
		if diff.EndingState == clmrpc.AccountDiff_OUTPUT_RECREATED &&
			diff.Expiry != acct.Expiry {

			return &MismatchErr{
				msg: fmt.Sprintf("account %x has invalid "+
					"expiry: %d", diff.AccountKeyRaw,
					diff.Expiry),
			}
		}
	}

	// From what we can tell, the batch looks good. At least our part checks
	// out at this point.
	return nil
}

// validateMatchedOrder validates our order against another trader's order and
// tallies up our order's account balance.
func (v *batchVerifier) validateMatchedOrder(tally *AccountTally,
	ourOrder Order, otherOrder *MatchedOrder, executionFee FeeSchedule,
	clearingPrice FixedRatePremium) error {

	// Order type must be opposite.
	if otherOrder.Order.Type() == ourOrder.Type() {
		return fmt.Errorf("order %v matched same type "+
			"orders", ourOrder.Nonce())
	}

	// Make sure we weren't matched to our own order.
	if otherOrder.NodeKey == v.ourNodePubkey {
		return fmt.Errorf("other order is an order from our node")
	}

	// Verify that the durations overlap. Then tally up all the fees and
	// units that were paid/accrued in this matched order pair. We can
	// safely cast orders here because we made sure we have the right types
	// in the previous step.
	switch ours := ourOrder.(type) {
	case *Ask:
		other := otherOrder.Order.(*Bid)
		if other.MinDuration > ours.MaxDuration {
			return fmt.Errorf("order duration not overlapping " +
				"for our ask")
		}

		// The ask's price cannot be higher than the bid's price.
		if ours.FixedRate > other.FixedRate {
			return fmt.Errorf("ask price greater than bid price")
		}

		// This match checks out, deduct it from the account's balance.
		tally.CalcMakerDelta(
			executionFee, clearingPrice,
			otherOrder.UnitsFilled.ToSatoshis(), other.MinDuration,
		)

	case *Bid:
		other := otherOrder.Order.(*Ask)
		if other.MaxDuration < ours.MinDuration {
			return fmt.Errorf("order duration not overlapping " +
				"for our bid")
		}

		// The ask's price cannot be higher than the bid's price.
		if other.FixedRate > ours.FixedRate {
			return fmt.Errorf("ask price greater than bid price")
		}

		// This match checks out, deduct it from the account's balance.
		tally.CalcTakerDelta(
			executionFee, clearingPrice,
			otherOrder.UnitsFilled.ToSatoshis(), ours.MinDuration,
		)
	}

	// Everything checks out so far.
	return nil
}

// validateChannelOutput makes sure there is a channel output in the batch TX
// that spends the correct amount for the matched units to the correct multisig
// script that can be used by us to open the channel.
func (v *batchVerifier) validateChannelOutput(batchTx *wire.MsgTx,
	ourOrder Order, otherOrder *MatchedOrder) error {

	// Re-derive our multisig key first.
	ctxt, cancel := context.WithTimeout(
		context.Background(), deriveKeyTimeout,
	)
	defer cancel()
	ourKey, err := v.wallet.DeriveKey(
		ctxt, &ourOrder.Details().MultiSigKeyLocator,
	)
	if err != nil {
		return fmt.Errorf("could not derive our multisig key: %v", err)
	}

	// Gather the information we expect to find in the batch TX.
	expectedOutputSize := otherOrder.UnitsFilled.ToSatoshis()
	_, expectedOut, err := input.GenFundingPkScript(
		ourKey.PubKey.SerializeCompressed(), otherOrder.MultiSigKey[:],
		int64(expectedOutputSize),
	)
	if err != nil {
		return fmt.Errorf("could not create multisig script: %v", err)
	}

	// Locate the channel output now that we know what to look for.
	for _, out := range batchTx.TxOut {
		if out.Value == expectedOut.Value &&
			bytes.Equal(out.PkScript, expectedOut.PkScript) {

			// Bingo, this is what we want.
			return nil
		}
	}

	return fmt.Errorf("no channel output found in batch tx for matched "+
		"order %v", otherOrder.Order.Nonce())
}

// A compile-time constraint to ensure batchVerifier implements BatchVerifier.
var _ BatchVerifier = (*batchVerifier)(nil)