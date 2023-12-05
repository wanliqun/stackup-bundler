package gasprice

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stackup-wallet/stackup-bundler/internal/logger"
	"github.com/stackup-wallet/stackup-bundler/pkg/modules"
	"github.com/stackup-wallet/stackup-bundler/pkg/userop"
)

// FilterUnderpriced returns a BatchHandlerFunc that will filter out all the userOps that are below either the
// dynamic or legacy GasPrice set in the context.
func FilterUnderpriced() modules.BatchHandlerFunc {
	return func(ctx *modules.BatchHandlerCtx) error {
		var b, f []*userop.UserOperation
		for _, op := range ctx.Batch {
			if ctx.BaseFee != nil && ctx.BaseFee.Cmp(common.Big0) != 0 && ctx.Tip != nil {
				gp := big.NewInt(0).Add(ctx.BaseFee, ctx.Tip)
				if op.GetDynamicGasPrice(ctx.BaseFee).Cmp(gp) >= 0 {
					b = append(b, op)
				} else {
					f = append(f, op)
				}
			} else if ctx.GasPrice != nil {
				if op.MaxFeePerGas.Cmp(ctx.GasPrice) >= 0 {
					b = append(b, op)
				} else {
					f = append(f, op)
				}
			}
		}

		if len(f) > 0 {
			logger.Shared().WithValues("userops", f).
				Info("userops filtered due to being underpriced")
		}

		ctx.Batch = b
		return nil
	}
}
