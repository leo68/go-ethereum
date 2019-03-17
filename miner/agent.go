// Copyright 2015 The go-ethereum Authors
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

//agent 是具体执行挖矿的对象。 它执行的流程就是，接受计算好了的区块头， 计算mixhash和nonce，
// 把挖矿好的区块头返回。
package miner

import (
	"sync"

	"sync/atomic"

	"github.com/meitu/go-ethereum/consensus"
	"github.com/meitu/go-ethereum/log"
)

type CpuAgent struct {
	mu sync.Mutex

	//接受挖矿任务的通道
	workCh        chan *Work
	//停止挖矿任务的通道
	stop          chan struct{}
	//退出当前操作
	quitCurrentOp chan struct{}
	//挖矿完成后的返回channel
	returnCh      chan<- *Result
	//获取区块链的信息
	chain  consensus.ChainReader
	//一致性引擎，本算法下指dpos引擎
	engine consensus.Engine
	//isMining指示当前是否正在执行挖矿
	isMining int32 // isMining indicates whether the agent is currently mining
}

func NewCpuAgent(chain consensus.ChainReader, engine consensus.Engine) *CpuAgent {
	miner := &CpuAgent{
		chain:  chain,
		engine: engine,
		stop:   make(chan struct{}, 1),
		workCh: make(chan *Work, 1),
	}
	return miner
}
//设置返回值channel和得到Work的channel， 方便外界传值和得到返回信息。
func (self *CpuAgent) Work() chan<- *Work            { return self.workCh }
func (self *CpuAgent) SetReturnCh(ch chan<- *Result) { self.returnCh = ch }

func (self *CpuAgent) Stop() {
	if !atomic.CompareAndSwapInt32(&self.isMining, 1, 0) {
		return // agent already stopped
	}
	self.stop <- struct{}{}
done:
	// Empty work channel
	for {
		select {
		case <-self.workCh:
		default:
			break done
		}
	}
}

//启动和消息循环，如果已经启动挖矿，那么直接退出， 否则启动update 这个goroutine update
//从workCh接受任务，进行挖矿，或者是接受退出信息，退出。
//用于监听推送过来的Work
func (self *CpuAgent) Start() {
	//CompareAndSwapInt32函数先比较变量的值是否等于给定旧值，等于旧值的情况下才赋予新值，最后返回新值是否设置成功。
	if !atomic.CompareAndSwapInt32(&self.isMining, 0, 1) {
		return // agent already started正在挖矿
	}
	go self.update()
}

func (self *CpuAgent) update() {
out:
	for {
		select {
		//在接收到Work后，会起一个goroutine调用mine()函数进行处理
		case work := <-self.workCh:
			self.mu.Lock()
			if self.quitCurrentOp != nil {
				close(self.quitCurrentOp)
			}
			self.quitCurrentOp = make(chan struct{})
			//起一个goroutine来进行挖矿，并传入退出通道
			//在接收到Work后，会起一个goroutine调用mine()函数进行处理
			go self.mine(work, self.quitCurrentOp)
			self.mu.Unlock()
		case <-self.stop:
			self.mu.Lock()
			if self.quitCurrentOp != nil {
				close(self.quitCurrentOp)
				self.quitCurrentOp = nil
			}
			self.mu.Unlock()
			break out
		}
	}
}
//mine, 挖矿，调用一致性引擎进行挖矿， 如果挖矿成功，把消息发送到returnCh上面。
//先调用共识引擎的Seal()函数，实际上就是进行POW计算，不断修改nonce值直到找到一个小于难度值的hash。
// 如果计算完成，就说明成功挖出了一个新块，我们获得的返回值就是一个有效的Block。
//把Work和Block组织成一个Result结构，发送给之前注册返回channel的调用者，也就是worker。
func (self *CpuAgent) mine(work *Work, stop <-chan struct{}) {
	if result, err := self.engine.Seal(self.chain, work.Block, stop); result != nil {
		log.Info("Successfully sealed new block", "number", result.Number(), "hash", result.Hash())
		self.returnCh <- &Result{work, result}
	} else {
		if err != nil {
			log.Warn("Block sealing failed", "err", err)
		}
		self.returnCh <- nil
	}
}
//GetHashRate， 这个函数返回当前的HashRate（算力），dpos算法下返回0。
func (self *CpuAgent) GetHashRate() int64 {
	if pow, ok := self.engine.(consensus.PoW); ok {
		return int64(pow.Hashrate())
	}
	return 0
}
