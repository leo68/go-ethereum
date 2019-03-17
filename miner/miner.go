// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package miner implements Ethereum block creation and mining.

//miner用来对worker进行管理， 订阅外部事件，控制worker的启动和停止。
package miner

import (
	"fmt"
	"sync/atomic"

	"github.com/meitu/go-ethereum/accounts"
	"github.com/meitu/go-ethereum/common"
	"github.com/meitu/go-ethereum/consensus"
	"github.com/meitu/go-ethereum/core"
	"github.com/meitu/go-ethereum/core/state"
	"github.com/meitu/go-ethereum/core/types"
	"github.com/meitu/go-ethereum/eth/downloader"
	"github.com/meitu/go-ethereum/ethdb"
	"github.com/meitu/go-ethereum/event"
	"github.com/meitu/go-ethereum/log"
	"github.com/meitu/go-ethereum/params"
)

// Backend wraps all methods required for mining.
type Backend interface {
	AccountManager() *accounts.Manager
	BlockChain() *core.BlockChain
	TxPool() *core.TxPool
	ChainDb() ethdb.Database
}

// Miner creates blocks and searches for proof-of-work values.
type Miner struct {
	mux *event.TypeMux

	worker *worker

	coinbase common.Address
	mining   int32
	eth      Backend
	engine   consensus.Engine

	canStart    int32 // can start indicates whether we can start the mining operation
	shouldStart int32 // should start indicates whether we should start after sync
}
//构造, 创建了一个CPU agent 启动了miner的update goroutine
//挖矿三步走：创建Miner实例、注册Agent(dpos中被干掉)、等待区块同步完成
func New(eth Backend, config *params.ChainConfig, mux *event.TypeMux, engine consensus.Engine) *Miner {
	miner := &Miner{
		eth:      eth,
		mux:      mux,
		engine:   engine,
		//创建一个worker实例，Miner只是一个发起人，真正挖矿的是worker
		worker:   newWorker(config, engine, common.Address{}, eth, mux),
		canStart: 1,
	}
	//在开始挖矿之前，首先需要等待和其他结点之间完成区块同步，这样才能在最新的状态挖矿。
	go miner.update()

	return miner
}

// update keeps track of the downloader events. Please be aware that this is a one shot type of update loop.
// It's entered once and as soon as `Done` or `Failed` has been broadcasted the events are unregistered and
// the loop is exited. This to prevent a major security vuln where external parties can DOS you with blocks
// and halt your mining operation for as long as the DOS continues.
//update订阅了downloader的事件， 注意这个goroutine是一个一次性的循环， 只要接收到一次downloader的downloader.
//DoneEvent或者 downloader.FailedEvent事件， 就会设置canStart为1. 并退出循环， 这是为了避免黑客恶意的 DOS攻击，
// 让你不断的处于异常状态
func (self *Miner) update() {
	//订阅了downloader的StartEvent、DoneEvent、FailedEvent事件
	events := self.mux.Subscribe(downloader.StartEvent{}, downloader.DoneEvent{}, downloader.FailedEvent{})
out:
	for ev := range events.Chan() {
		switch ev.Data.(type) {
		//当收到StartEvent时，会把canStart置为0，这样即使你调用Miner的Start()函数也不会真正启动
		case downloader.StartEvent:
			atomic.StoreInt32(&self.canStart, 0)
			if self.Mining() {
				self.Stop()
				atomic.StoreInt32(&self.shouldStart, 1)
				log.Info("Mining aborted due to sync")
			}
		//当收到DoneEvent或者FailedEvent时，将canStart置为1，然后调用Miner的Start()函数启动挖矿.FailedEvent?
		case downloader.DoneEvent, downloader.FailedEvent:
			shouldStart := atomic.LoadInt32(&self.shouldStart) == 1

			atomic.StoreInt32(&self.canStart, 1)
			atomic.StoreInt32(&self.shouldStart, 0)
			if shouldStart {
				//-----》开始挖矿
				self.Start(self.coinbase)
			}
			// unsubscribe. we're only interested in this event once
			//收到downloader的消息后会立即停止订阅这些消息并退出，也就是说这个函数只会运行一次。
			// 处理完以后要取消订阅
			//Subscribe了下载事件，它只处理一次，原因是为了防止黑客进行DOS攻击
			events.Unsubscribe()
			// stop immediately and ignore all further pending events
			// 跳出循环，不再监听
			break out
		}
	}
}

func (self *Miner) Start(coinbase common.Address) {
	// shouldStart 设置是否应该启动
	//函数atomic.StoreInt32会接受两个参数。第一个参数的类型是*int 32类型的，
	// 其含义是指向被操作值的指针。而第二个参数则是int32类型的，它的值应该代表欲存储的新值。
	// 将应该启动挖矿标志设为1，干啥用？
	atomic.StoreInt32(&self.shouldStart, 1)
	self.worker.setCoinbase(coinbase)
	self.coinbase = coinbase

	// canStart是否能够启动,判断canStart标志，如果同步没有完成的话是不会真正启动的
	if atomic.LoadInt32(&self.canStart) == 0 {
		log.Info("Network syncing, will start miner afterwards")
		return
	}
	//将正在挖矿标志设置为1
	atomic.StoreInt32(&self.mining, 1)

	log.Info("Starting mining operation")
	// 启动worker 开始挖矿
	self.worker.start()
}

func (self *Miner) Stop() {
	self.worker.stop()
	atomic.StoreInt32(&self.mining, 0)
	atomic.StoreInt32(&self.shouldStart, 0)
}

func (self *Miner) Mining() bool {
	return atomic.LoadInt32(&self.mining) > 0
}

func (self *Miner) HashRate() int64 {
	return 0
}

func (self *Miner) SetExtra(extra []byte) error {
	if uint64(len(extra)) > params.MaximumExtraDataSize {
		return fmt.Errorf("Extra exceeds max length. %d > %v", len(extra), params.MaximumExtraDataSize)
	}
	self.worker.setExtra(extra)
	return nil
}

// Pending returns the currently pending block and associated state.
func (self *Miner) Pending() (*types.Block, *state.StateDB) {
	return self.worker.pending()
}

// PendingBlock returns the currently pending block.
//
// Note, to access both the pending block and the pending state
// simultaneously, please use Pending(), as the pending state can
// change between multiple method calls
func (self *Miner) PendingBlock() *types.Block {
	return self.worker.pendingBlock()
}

func (self *Miner) SetCoinbase(addr common.Address) {
	self.coinbase = addr
	self.worker.setCoinbase(addr)
}
