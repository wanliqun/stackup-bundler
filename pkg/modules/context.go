package modules

import (
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stackup-wallet/stackup-bundler/pkg/entrypoint"
	"github.com/stackup-wallet/stackup-bundler/pkg/userop"
)

// BatchHandlerCtx is the object passed to BatchHandler functions during the Bundler's Run process. It
// also contains a Data field for adding arbitrary key-value pairs to the context. These values will be
// logged by the Bundler at the end of each run.
type BatchHandlerCtx struct {
	Batch          []*userop.UserOperation
	PendingRemoval []*userop.UserOperation
	EntryPoint     common.Address
	ChainID        *big.Int
	BaseFee        *big.Int
	Tip            *big.Int
	GasPrice       *big.Int
	Data           map[string]any
}

// NewBatchHandlerContext creates a new BatchHandlerCtx using a copy of the given batch.
func NewBatchHandlerContext(
	batch []*userop.UserOperation,
	entryPoint common.Address,
	chainID *big.Int,
	baseFee *big.Int,
	tip *big.Int,
	gasPrice *big.Int,
) *BatchHandlerCtx {
	var copy []*userop.UserOperation
	copy = append(copy, batch...)

	return &BatchHandlerCtx{
		Batch:          copy,
		PendingRemoval: []*userop.UserOperation{},
		EntryPoint:     entryPoint,
		ChainID:        chainID,
		BaseFee:        baseFee,
		Tip:            tip,
		GasPrice:       gasPrice,
		Data:           make(map[string]any),
	}
}

// MarkOpIndexForRemoval will remove the op by index from the batch and add it to the pending removal array.
// This should be used for ops that are not to be included on-chain and dropped from the mempool.
func (c *BatchHandlerCtx) MarkOpIndexForRemoval(index int) *userop.UserOperation {
	batch := []*userop.UserOperation{}
	var op *userop.UserOperation
	for i, curr := range c.Batch {
		if i == index {
			op = curr
		} else {
			batch = append(batch, curr)
		}
	}
	if op == nil {
		return nil
	}

	c.Batch = batch
	c.PendingRemoval = append(c.PendingRemoval, op)
	return op
}

// UserOpHandlerCtx is the object passed to UserOpHandler functions during the Client's SendUserOperation
// process.
type UserOpHandlerCtx struct {
	UserOp     *userop.UserOperation
	EntryPoint common.Address
	ChainID    *big.Int
	deposits   sync.Map
	pendingOps []*userop.UserOperation
}

// NewUserOpHandlerContext creates a new UserOpHandlerCtx using a given op.
func NewUserOpHandlerContext(
	op *userop.UserOperation,
	pendingOps []*userop.UserOperation,
	entryPoint common.Address,
	chainID *big.Int,
) *UserOpHandlerCtx {
	return &UserOpHandlerCtx{
		UserOp:     op,
		EntryPoint: entryPoint,
		ChainID:    chainID,
		deposits:   sync.Map{},
		pendingOps: append([]*userop.UserOperation{}, pendingOps...),
	}
}

// AddDepositInfo adds any entity's EntryPoint stake info to the current context.
func (c *UserOpHandlerCtx) AddDepositInfo(entity common.Address, dep *entrypoint.IStakeManagerDepositInfo) {
	c.deposits.Store(entity, dep)
}

// GetDepositInfo retrieves any entity's EntryPoint stake info from the current context if it was previously
// added. Otherwise returns nil
func (c *UserOpHandlerCtx) GetDepositInfo(entity common.Address) *entrypoint.IStakeManagerDepositInfo {
	dep, ok := c.deposits.Load(entity)
	if !ok {
		return nil
	}

	return dep.(*entrypoint.IStakeManagerDepositInfo)
}

// GetPendingOps returns all pending UserOperations in the mempool by the same UserOp.Sender.
func (c *UserOpHandlerCtx) GetPendingOps() []*userop.UserOperation {
	return c.pendingOps
}
