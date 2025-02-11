// Package relay implements a module for private bundlers to send batches to the EntryPoint through regular
// EOA transactions.
package relay

import (
	"math/big"
	"time"

	badger "github.com/dgraph-io/badger/v3"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"github.com/stackup-wallet/stackup-bundler/internal/config"
	"github.com/stackup-wallet/stackup-bundler/internal/logger"
	"github.com/stackup-wallet/stackup-bundler/pkg/entrypoint/transaction"
	"github.com/stackup-wallet/stackup-bundler/pkg/modules"
	"github.com/stackup-wallet/stackup-bundler/pkg/signer"
)

// Relayer provides a module that can relay batches with a regular EOA. Relaying batches to the EntryPoint
// through a regular transaction comes with several important notes:
//
//   - The bundler will NOT be operating as a block builder.
//   - This opens the bundler up to frontrunning.
//
// This module only works in the case of a private mempool and will not work in the P2P case where ops are
// propagated through the network and it is impossible to prevent collisions from multiple bundlers trying to
// relay the same ops.
type Relayer struct {
	sender      *transaction.Sender
	eoa         *signer.EOA
	eth         *ethclient.Client
	chainID     *big.Int
	beneficiary common.Address
	logger      logr.Logger
	waitTimeout time.Duration
}

// New initializes a new EOA relayer for sending batches to the EntryPoint.
func New(
	db *badger.DB,
	eoa *signer.EOA,
	eth *ethclient.Client,
	chainID *big.Int,
	beneficiary common.Address,
	l logr.Logger,
) (*Relayer, error) {
	l = l.WithName("relayer")
	sender, err := transaction.NewSender(eth, db, l)
	if err != nil {
		return nil, err
	}

	return &Relayer{
		sender:      sender,
		eoa:         eoa,
		eth:         eth,
		chainID:     chainID,
		beneficiary: beneficiary,
		logger:      l,
		waitTimeout: DefaultWaitTimeout,
	}, nil
}

// SetWaitTimeout sets the total time to wait for a transaction to be included. When a timeout is reached, the
// BatchHandler will throw an error if the transaction has not been included or has been included but with a
// failed status.
//
// The default value is 30 seconds. Setting the value to 0 will skip waiting for a transaction to be included.
func (r *Relayer) SetWaitTimeout(timeout time.Duration) {
	r.waitTimeout = timeout
}

// SendUserOperation returns a BatchHandler that is used by the Bundler to send batches in a regular EOA
// transaction.
func (r *Relayer) SendUserOperation() modules.BatchHandlerFunc {
	return func(ctx *modules.BatchHandlerCtx) error {
		opts := transaction.Opts{
			EOA:         r.eoa,
			Eth:         r.eth,
			ChainID:     ctx.ChainID,
			EntryPoint:  ctx.EntryPoint,
			Batch:       ctx.Batch,
			Beneficiary: r.beneficiary,
			BaseFee:     ctx.BaseFee,
			Tip:         ctx.Tip,
			GasPrice:    ctx.GasPrice,
			GasLimit:    0,
			WaitTimeout: r.waitTimeout,
		}
		// Estimate gas for handleOps() and drop all userOps that cause unexpected reverts.
		estRev := []string{}
		for len(ctx.Batch) > 0 {
			est, revert, err := transaction.EstimateHandleOpsGas(&opts)

			if err != nil {
				return errors.WithMessage(err, "failed to estimate `HandleOps` gas")
			} else if revert != nil {
				op := ctx.MarkOpIndexForRemoval(revert.OpIndex)
				estRev = append(estRev, revert.Reason)

				// Exclude the removed user operation from consideration for next estimation loop.
				opts.Batch = ctx.Batch

				logger.Shared().WithValues("userop", op, "revert", revert).
					Info("userop added to pending removal due to `HandleOps` estimation revert")
			} else {
				opts.GasLimit = est
				break
			}
		}
		ctx.Data["relayer_est_revert_reasons"] = estRev

		if len(ctx.Batch) == 0 {
			return nil
		}

		finalEst, err := transaction.EstimateBundleTxnGas(&opts)
		if err != nil {
			return err
		}

		opts.GasLimit = finalEst

		// Calculate the total gas limit of all user operations.
		totalUserOpGasLimit := big.NewInt(0)
		for _, op := range ctx.Batch {
			totalUserOpGasLimit.Add(totalUserOpGasLimit, op.GetMaxGasAvailable())
		}

		// The estimated gas limit exceeds the total gas limits of all user operations, which is not
		// good since bundler might lose money.
		if totalUserOpGasLimit.Uint64() < opts.GasLimit {
			r.logger.WithValues("totalUserOpGasLimit", totalUserOpGasLimit.Uint64()).
				WithValues("estimatedTxnGasLimit", opts.GasLimit).
				Info("Estimated bundle gas limit exceeds the total gas limit of all user operations")
		}

		// Call handleOps() with gas estimate. Any userOps that cause a revert at this stage will be
		// caught and dropped in the next iteration.
		var txn *types.Transaction
		if config.Shared().LegacySending {
			txn, err = transaction.HandleOps(&opts)
		} else {
			txn, err = r.sender.HandleOps(&opts)
		}

		if err != nil {
			return errors.WithMessage(err, "failed to send `HandleOps` transaction")
		}

		ctx.Data["txn_hash"] = txn.Hash().String()
		return nil
	}
}
