package transaction

import (
	"context"
	"errors"
	"math/big"
	"strconv"
	"sync"
	"time"

	badger "github.com/dgraph-io/badger/v3"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/go-logr/logr"
	"github.com/stackup-wallet/stackup-bundler/internal/dbutils"
	"github.com/stackup-wallet/stackup-bundler/pkg/entrypoint"
	"github.com/wangjia184/sortedset"
)

const (
	defaultNonceTooFuture  = uint64(100)
	defaultFinalizedBlocks = 200
	defaultMonitorInterval = 15 * time.Second
)

var (
	errNonceTooFuture = errors.New("nonce too future")
	dbKeyPrefix       = dbutils.JoinValues("txnpool")
)

type noncePairs struct {
	nextNonce, latestNonce uint64
}

func (p *noncePairs) isTooFuture(threshold ...uint64) bool {
	thresholdv := defaultNonceTooFuture
	if len(threshold) > 0 {
		thresholdv = threshold[0]
	}

	return p.nextNonce <= p.latestNonce+thresholdv
}

func senderNextNonceDBKey(sender common.Address) []byte {
	return []byte(dbutils.JoinValues(dbKeyPrefix, "nonce", sender.String()))
}

func transactionDBKey(sender common.Address, nonce uint64) []byte {
	return []byte(dbutils.JoinValues(
		dbKeyPrefix, "txn", sender.String(), strconv.FormatUint(nonce, 10)),
	)
}

// Sender implements a nonce-custody transaction sending mechanism for bundling,
// which assigns a unique, self-incrementing nonce to each bundle transaction.
//
// This mechanism continuously retries the transaction in case of abnormal occurrences,
// such as failed broadcasting or low gas price (in which case the gas price may need to
// be adjusted).
type Sender struct {
	mu sync.Mutex

	logger logr.Logger
	// RPC client
	eth *ethclient.Client
	// Persistence db
	db *badger.DB
	// Custodied EOA sender nonce pairs
	nonces map[common.Address]*noncePairs
	// Monitored transactions keyed by sender address
	obsTxns map[common.Address]*sortedset.SortedSet
}

func NewSender(eth *ethclient.Client, db *badger.DB, l logr.Logger) (*Sender, error) {
	s := &Sender{
		db:      db,
		eth:     eth,
		logger:  l.WithName("txnSender"),
		nonces:  make(map[common.Address]*noncePairs),
		obsTxns: make(map[common.Address]*sortedset.SortedSet),
	}

	if err := s.loadFromDisk(); err != nil {
		return nil, err
	}

	go s.monitor()
	return s, nil
}

func (s *Sender) loadFromDisk() error {
	return s.db.View(func(dbTxn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 10
		it := dbTxn.NewIterator(opts)
		prefix := []byte(dbKeyPrefix)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()

			strSplits := dbutils.SplitValues(string(item.Key()))
			sender := common.HexToAddress(strSplits[1])

			err := item.Value(func(data []byte) error {
				switch strSplits[1] {
				case "nonce":
					nonce, err := strconv.ParseUint(string(data), 10, 64)
					if err != nil {
						return err
					}
					s.nonces[sender] = &noncePairs{nextNonce: nonce}
				case "txn":
					txn := &types.Transaction{}
					if err := txn.UnmarshalBinary(data); err != nil {
						return err
					}
					s.addObserveeTxn(sender, txn)
				default:
					return errors.New("invalid DB key")
				}

				return nil
			})

			if err != nil {
				return err
			}
		}

		return nil
	})
}

// HandleOps submits a transaction to send a batch of UserOperations to the EntryPoint.
func (s *Sender) HandleOps(opts *Opts) (txn *types.Transaction, err error) {
	ep, err := entrypoint.NewEntrypoint(opts.EntryPoint, opts.Eth)
	if err != nil {
		return nil, err
	}

	auth, err := bind.NewKeyedTransactorWithChainID(opts.EOA.PrivateKey, opts.ChainID)
	if err != nil {
		return nil, err
	}
	auth.GasLimit = opts.GasLimit

	ctx := context.Background()
	if opts.WaitTimeout > 0 {
		c, cancel := context.WithTimeout(context.Background(), opts.WaitTimeout)
		ctx = c
		defer cancel()
	}

	np, err := s.noncePairs(ctx, opts.Eth, auth.From)
	if err != nil {
		return nil, err
	}

	if np.isTooFuture() {
		return nil, errNonceTooFuture
	}

	auth.Nonce = big.NewInt(int64(np.nextNonce))

	if (opts.BaseFee == nil || opts.Tip == nil) && opts.GasPrice == nil {
		return nil, errors.New("transaction: either the dynamic or legacy gas fees must be set")
	}

	// Assemble a signed transaction
	auth.NoSend = true
	txn, err = ep.HandleOps(auth, toAbiType(opts.Batch), opts.Beneficiary)
	if err != nil {
		return nil, err
	}

	err = s.db.Update(func(dbTxn *badger.Txn) error {
		nextNonceStr := strconv.FormatUint(txn.Nonce()+1, 10)

		// Update the next nonce
		dbNonceKey := senderNextNonceDBKey(auth.From)
		if err := dbTxn.Set(dbNonceKey, []byte(nextNonceStr)); err != nil {
			return err
		}

		// Store the RLP encoded data of the transaction.
		data, err := txn.MarshalBinary()
		if err != nil {
			return err
		}

		dbTxnKey := transactionDBKey(auth.From, txn.Nonce())
		if err := dbTxn.Set(dbTxnKey, data); err != nil {
			return err
		}

		// Do the real txn sending
		if err := opts.Eth.SendTransaction(ctx, txn); err != nil {
			return err
		}

		if opts.WaitTimeout == 0 {
			// Don't wait for transaction to be included. All userOps in the current batch will be dropped
			// regardless of the transaction status.
			return nil
		}

		receipt, err := bind.WaitMined(ctx, opts.Eth, txn)
		if err == nil && receipt.Status == types.ReceiptStatusFailed {
			// Return an error here so that the current batch stays in the mempool. In the next bundler iteration,
			// the offending userOps will be dropped during gas estimation.
			return errors.New("transaction: failed status")
		}

		// Otherwise, we shall continue to monitor the transaction and enforce the final execution.
		s.addObserveeTxn(auth.From, txn)
		s.nonces[auth.From].nextNonce++

		return nil
	})

	if err != nil {
		s.logger.Error(err, "HandleOps sent failed")
		return nil, err
	}

	return txn, nil
}

func (s *Sender) monitor() {
	t := time.NewTimer(defaultMonitorInterval)
	defer t.Stop()

	for range t.C {
		obsTxns := s.selectObserveeTxns()

		for sender, txn := range obsTxns {
			finalized, err := s.checkObserveeTxn(sender, txn)
			if err != nil {
				s.logger.WithValues("sender", sender).
					Error(err, "Observee Txn check failed")
				continue
			}

			if finalized {
				s.deleteObserveeTxn(sender, txn)
			}
		}

		t.Reset(defaultMonitorInterval)
	}
}

func (s *Sender) addObserveeTxn(sender common.Address, txn *types.Transaction) {
	s.mu.Lock()
	defer s.mu.Unlock()

	txnSortset := s.obsTxns[sender]
	if txnSortset == nil {
		txnSortset = sortedset.New()
	}

	txnSortset.AddOrUpdate(
		txn.Hash().String(), sortedset.SCORE(txn.Nonce()), txn,
	)
	s.obsTxns[sender] = txnSortset
}

func (s *Sender) deleteObserveeTxn(sender common.Address, txn *types.Transaction) error {
	return s.db.Update(func(dbTxn *badger.Txn) error {
		if err := dbTxn.Delete(transactionDBKey(sender, txn.Nonce())); err != nil {
			return err
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		if obsTxnSortset, ok := s.obsTxns[sender]; ok {
			obsTxnSortset.Remove(txn.Hash().String())
		}

		return nil
	})
}

func (s *Sender) checkObserveeTxn(sender common.Address, txn *types.Transaction) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultMonitorInterval)
	defer cancel()

	recpt, err := s.eth.TransactionReceipt(ctx, txn.Hash())
	if errors.Is(err, ethereum.NotFound) {
		// Transaction not yet mined or dropped off the txn pool, let's just try to send
		// the transaction again no matter it succeeds or not.
		return false, s.eth.SendTransaction(ctx, txn)
	}

	if err != nil { // Some unexpected error happens?
		return false, err
	}

	if recpt.BlockNumber == nil {
		// Still pending, maybe we should adjust the gas price. But for now, let's just see
		// what would happen next.
		return false, nil
	}

	latestBlockNum, err := s.eth.BlockNumber(ctx)
	if err != nil {
		return false, err
	}

	// The block number of the transaction is away behind the latest block number, in which case
	// we shall consider this transaction as finalized.
	tmpBlockNum := big.NewInt(0).Add(recpt.BlockNumber, big.NewInt(defaultFinalizedBlocks))
	if tmpBlockNum.Cmp(big.NewInt(int64(latestBlockNum))) <= 0 {
		return true, nil
	}

	return false, nil
}

func (s *Sender) selectObserveeTxns() map[common.Address]*types.Transaction {
	s.mu.Lock()
	defer s.mu.Unlock()

	res := make(map[common.Address]*types.Transaction)
	for sender, txnSortset := range s.obsTxns {
		node := txnSortset.PopMin()
		if node == nil {
			continue
		}

		txn, ok := node.Value.(*types.Transaction)
		if ok {
			res[sender] = txn
		}
	}

	return res
}

func (s *Sender) noncePairs(
	ctx context.Context, eth *ethclient.Client, senderAddr common.Address) (*noncePairs, error) {
	np, ok := s.nonces[senderAddr]
	if ok && !np.isTooFuture() {
		return np, nil
	}

	// Sender nonce not exists or latest nonce expired.
	latestNonce, err := eth.NonceAt(ctx, senderAddr, nil)
	if err != nil {
		return nil, err
	}

	if np == nil {
		np = &noncePairs{nextNonce: latestNonce}
	}
	np.latestNonce = latestNonce

	s.nonces[senderAddr] = np
	return np, nil
}
