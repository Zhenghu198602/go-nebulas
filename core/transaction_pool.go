// Copyright (C) 2017 go-nebulas authors
//
// This file is part of the go-nebulas library.
//
// the go-nebulas library is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// the go-nebulas library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with the go-nebulas library.  If not, see <http://www.gnu.org/licenses/>.
//

package core

import (
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/nebulasio/go-nebulas/common/sorted"
	"github.com/nebulasio/go-nebulas/core/pb"
	"github.com/nebulasio/go-nebulas/net"
	"github.com/nebulasio/go-nebulas/util"
	"github.com/nebulasio/go-nebulas/util/byteutils"
	"github.com/nebulasio/go-nebulas/util/logging"
	"github.com/sirupsen/logrus"
)

// TransactionPool cache txs, is thread safe
type TransactionPool struct {
	receivedMessageCh chan net.Message
	quitCh            chan int

	size       int
	candidates *sorted.Slice
	buckets    map[byteutils.HexHash]*sorted.Slice
	all        map[byteutils.HexHash]*Transaction

	ns net.Service
	mu sync.RWMutex

	minGasPrice *util.Uint128 // the lowest gasPrice.
	maxGasLimit *util.Uint128 // the maximum gasLimit.

	eventEmitter *EventEmitter
	bc           *BlockChain
}

func nonceCmp(a interface{}, b interface{}) int {
	txa := a.(*Transaction)
	txb := b.(*Transaction)
	if txa.Nonce() < txb.Nonce() {
		return -1
	} else if txa.Nonce() > txb.Nonce() {
		return 1
	} else {
		return txb.GasPrice().Cmp(txa.GasPrice())
	}
}

func gasCmp(a interface{}, b interface{}) int {
	txa := a.(*Transaction)
	txb := b.(*Transaction)
	return txb.GasPrice().Cmp(txa.GasPrice())
}

// NewTransactionPool create a new TransactionPool
func NewTransactionPool(size int) *TransactionPool {
	return &TransactionPool{
		receivedMessageCh: make(chan net.Message, size),
		quitCh:            make(chan int, 1),
		size:              size,
		candidates:        sorted.NewSlice(gasCmp),
		buckets:           make(map[byteutils.HexHash]*sorted.Slice),
		all:               make(map[byteutils.HexHash]*Transaction),
		minGasPrice:       TransactionGasPrice,
		maxGasLimit:       TransactionMaxGas,
	}
}

// SetGasConfig config the lowest gasPrice and the maximum gasLimit.
func (pool *TransactionPool) SetGasConfig(gasPrice, gasLimit *util.Uint128) {
	if gasPrice == nil || gasPrice.Cmp(util.NewUint128()) <= 0 {
		pool.minGasPrice = TransactionGasPrice
	} else {
		pool.minGasPrice = gasPrice
	}
	if gasLimit == nil || gasLimit.Cmp(util.NewUint128()) == 0 || gasLimit.Cmp(TransactionMaxGas) > 0 {
		pool.maxGasLimit = TransactionMaxGas
	} else {
		pool.maxGasLimit = gasLimit
	}
}

// RegisterInNetwork register message subscriber in network.
func (pool *TransactionPool) RegisterInNetwork(ns net.Service) {
	ns.Register(net.NewSubscriber(pool, pool.receivedMessageCh, true, MessageTypeNewTx, net.MessageWeightNewTx))
	pool.ns = ns
}

func (pool *TransactionPool) setBlockChain(bc *BlockChain) {
	pool.bc = bc
}

func (pool *TransactionPool) setEventEmitter(emitter *EventEmitter) {
	pool.eventEmitter = emitter
}

// Start start loop.
func (pool *TransactionPool) Start() {
	logging.CLog().WithFields(logrus.Fields{
		"size": pool.size,
	}).Info("Starting TransactionPool...")

	go pool.loop()
}

// Stop stop loop.
func (pool *TransactionPool) Stop() {
	logging.CLog().WithFields(logrus.Fields{
		"size": pool.size,
	}).Info("Stop TransactionPool.")

	pool.quitCh <- 0
}

func (pool *TransactionPool) loop() {
	logging.CLog().WithFields(logrus.Fields{
		"size": pool.size,
	}).Info("Started TransactionPool.")

	timerChan := time.NewTicker(time.Second).C
	for {
		select {
		case <-timerChan:
			metricsCachedTx.Update(int64(len(pool.receivedMessageCh)))
		case <-pool.quitCh:
			logging.CLog().WithFields(logrus.Fields{
				"size": pool.size,
			}).Info("Stopped TransactionPool.")
			return
		case msg := <-pool.receivedMessageCh:
			if msg.MessageType() != MessageTypeNewTx {
				logging.VLog().WithFields(logrus.Fields{
					"messageType": msg.MessageType(),
					"message":     msg,
					"err":         "not new tx msg",
				}).Debug("Received unregistered message.")
				continue
			}

			tx := new(Transaction)
			pbTx := new(corepb.Transaction)
			if err := proto.Unmarshal(msg.Data(), pbTx); err != nil {
				logging.VLog().WithFields(logrus.Fields{
					"msgType": msg.MessageType(),
					"msg":     msg,
					"err":     err,
				}).Debug("Failed to unmarshal data.")
				continue
			}
			if err := tx.FromProto(pbTx); err != nil {
				logging.VLog().WithFields(logrus.Fields{
					"msgType": msg.MessageType(),
					"msg":     msg,
					"err":     err,
				}).Debug("Failed to recover a tx from proto data.")
				continue
			}

			if err := pool.PushAndRelay(tx); err != nil {
				logging.VLog().WithFields(logrus.Fields{
					"func":        "TxPool.loop",
					"messageType": msg.MessageType(),
					"transaction": tx,
					"err":         err,
				}).Debug("Failed to push a tx into tx pool.")
				continue
			}
		}
	}
}

// GetTransaction return transaction of given hash from transaction pool.
func (pool *TransactionPool) GetTransaction(hash byteutils.Hash) *Transaction {
	return pool.all[hash.Hex()]
}

// PushAndRelay push tx into pool and relay it
func (pool *TransactionPool) PushAndRelay(tx *Transaction) error {
	if err := pool.Push(tx); err != nil {
		return err
	}

	pool.ns.Relay(MessageTypeNewTx, tx, net.MessagePriorityNormal)
	return nil
}

// PushAndBroadcast push tx into pool and broadcast it
func (pool *TransactionPool) PushAndBroadcast(tx *Transaction) error {
	if err := pool.Push(tx); err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"tx":  tx,
			"err": err,
		}).Debug("Failed to push a new tx into tx pool")
		return err
	}

	pool.ns.Broadcast(MessageTypeNewTx, tx, net.MessagePriorityNormal)
	return nil
}

// Push tx into pool, input:1)RPC, 2)netService
func (pool *TransactionPool) Push(tx *Transaction) error { //ToRefine, change to local push

	// if tx's gasPrice below the pool config lowest gasPrice, return ErrBelowGasPrice
	if tx.gasPrice.Cmp(pool.minGasPrice) < 0 {
		metricsTxPoolBelowGasPrice.Inc(1)
		return ErrBelowGasPrice
	}

	if tx.gasLimit.Cmp(util.NewUint128()) <= 0 {
		metricsTxPoolGasLimitLessOrEqualToZero.Inc(1)
		return ErrGasLimitLessOrEqualToZero
	}

	if tx.gasLimit.Cmp(pool.maxGasLimit) > 0 {
		metricsTxPoolOutOfGasLimit.Inc(1)
		return ErrOutOfGasLimit
	}

	// verify hash & sign of tx
	if err := tx.VerifyIntegrity(pool.bc.chainID); err != nil {
		metricsInvalidTx.Inc(1)
		return err
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()

	// verify non-dup tx
	if _, ok := pool.all[tx.hash.Hex()]; ok {
		metricsDuplicateTx.Inc(1)
		return ErrDuplicatedTransaction
	}

	// cache the verified tx
	pool.pushTx(tx)
	// drop max tx in longest bucket if full
	if len(pool.all) > pool.size {
		pool.dropTx()
	}

	// trigger pending transaction
	event := &Event{
		Topic: TopicPendingTransaction,
		Data:  tx.String(),
	}
	pool.eventEmitter.Trigger(event)

	return nil
}

func (pool *TransactionPool) pushTx(tx *Transaction) {
	slot := tx.from.address.Hex()
	bucket, ok := pool.buckets[slot]
	if !ok {
		bucket = sorted.NewSlice(nonceCmp)
		pool.buckets[slot] = bucket
	}
	oldCandidate := bucket.Left()
	bucket.Push(tx)
	pool.all[tx.hash.Hex()] = tx
	newCandidate := bucket.Left()
	// replace candidate
	if oldCandidate == nil {
		pool.candidates.Push(newCandidate)
	} else if oldCandidate != newCandidate {
		pool.candidates.Del(oldCandidate)
		pool.candidates.Push(newCandidate)
	}
}

func (pool *TransactionPool) popTx(tx *Transaction) {
	bucket := pool.buckets[tx.from.address.Hex()]
	delete(pool.all, tx.hash.Hex())
	bucket.PopLeft()
	if bucket.Len() != 0 {
		candidate := bucket.Left()
		pool.candidates.Push(candidate)
	} else {
		delete(pool.buckets, tx.from.address.Hex())
	}
}

func (pool *TransactionPool) dropTx() {
	var longestSlice *sorted.Slice
	longestLen := 0
	for _, v := range pool.buckets {
		if v.Len() > longestLen {
			longestLen = v.Len()
			longestSlice = v
		}
	}
	if longestLen > 0 {
		drop := longestSlice.PopRight().(*Transaction)
		if drop != nil {
			delete(pool.all, drop.Hash().Hex())
			if longestLen == 1 {
				pool.candidates.Del(drop)
				delete(pool.buckets, drop.from.address.Hex())
			}
		}
	}
}

// PopWithBlacklist pop a tx not in the blacklist
func (pool *TransactionPool) PopWithBlacklist(blacklist map[byteutils.HexHash]bool) *Transaction {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	if blacklist == nil {
		blacklist = make(map[byteutils.HexHash]bool)
	}
	size := pool.candidates.Len()
	for i := 0; i < size; i++ {
		tx := pool.candidates.Index(i).(*Transaction)
		from := tx.from.address.Hex()
		if _, ok := blacklist[from]; !ok {
			pool.candidates.Del(tx)
			pool.popTx(tx)
			return tx
		}
	}
	return nil
}

// Pop a transaction from pool
func (pool *TransactionPool) Pop() *Transaction {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	candidates := pool.candidates
	val := candidates.PopLeft()
	if val == nil {
		return nil
	}
	tx := val.(*Transaction)
	pool.popTx(tx)
	return tx
}

// Del a transaction from pool
func (pool *TransactionPool) Del(tx *Transaction) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	bucket := pool.buckets[tx.from.address.Hex()]
	if bucket != nil && bucket.Len() > 0 {
		oldCandidate := bucket.Left()
		left := oldCandidate.(*Transaction)
		for left.Nonce() <= tx.Nonce() {
			bucket.PopLeft()
			delete(pool.all, left.Hash().Hex())
			if bucket.Len() > 0 {
				left = bucket.Left().(*Transaction)
			} else {
				delete(pool.buckets, left.from.address.Hex())
				break
			}
		}
		newCandidate := bucket.Left()
		// replace candidate
		if oldCandidate != newCandidate {
			pool.candidates.Del(oldCandidate)
			if newCandidate != nil {
				pool.candidates.Push(newCandidate)
			}
		}
	}
}

// Empty return if the pool is empty
func (pool *TransactionPool) Empty() bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return len(pool.all) == 0
}
