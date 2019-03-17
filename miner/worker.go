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


//worker 内部包含了很多agent，可以包含agent和remote_agent。
// worker同时负责构建区块和对象。同时把任务提供给agent。
package miner

import (
	"bytes"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/meitu/go-ethereum/common"
	"github.com/meitu/go-ethereum/consensus"
	"github.com/meitu/go-ethereum/consensus/dpos"
	"github.com/meitu/go-ethereum/consensus/misc"
	"github.com/meitu/go-ethereum/core"
	"github.com/meitu/go-ethereum/core/state"
	"github.com/meitu/go-ethereum/core/types"
	"github.com/meitu/go-ethereum/core/vm"
	"github.com/meitu/go-ethereum/ethdb"
	"github.com/meitu/go-ethereum/event"
	"github.com/meitu/go-ethereum/log"
	"github.com/meitu/go-ethereum/params"
	"gopkg.in/fatih/set.v0"
)

const (
	resultQueueSize  = 10
	miningLogAtDepth = 5

	// txChanSize is the size of channel listening to TxPreEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096
	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10

	// chainSideChanSize is the size of channel listening to ChainSideEvent.
	chainSideChanSize = 10
)

// Work is the workers current environment and holds
// all of the current state information
//Work结构，Work存储了工作者的当时的环境，并且持有所有的暂时的状态信息。
type Work struct {
	config *params.ChainConfig
	// 签名者
	signer types.Signer

	//状态数据库
	state       *state.StateDB // apply state changes here
	//dpos上下文集合
	dposContext *types.DposContext
	//祖先集合，用来检查祖先是否有效
	ancestors   *set.Set // ancestor set (used for checking uncle parent validity)
	//家族集合，用来检查祖先的无效性
	family      *set.Set // family set (used for checking uncle invalidity)
	//uncles集合
	uncles      *set.Set // uncle set
	//当前周期的交易数量
	tcount      int      // tx count in cycle
	//新的区块
	Block *types.Block // the new block

	//区块头
	header   *types.Header
	// 交易
	txs      []*types.Transaction
	// 收据
	receipts []*types.Receipt
	// 创建时间
	createdAt time.Time
}

//结果
type Result struct {
	Work  *Work
	Block *types.Block
}

// worker is the main object which takes care of applying messages to the new state
// worker是负责将消息应用到新状态的主要对象
type worker struct {
	config *params.ChainConfig
	engine consensus.Engine

	mu sync.Mutex

	// update loop
	mux         *event.TypeMux
	txCh        chan core.TxPreEvent   // 用来接受txPool里面的交易的通道
	txSub       event.Subscription     // 用来接受txPool里面的交易的订阅器
	chainHeadCh chan core.ChainHeadEvent	// 用来接受区块头的通道

	chainHeadSub event.Subscription
	wg           sync.WaitGroup

	recv chan *Result						// agent会把结果发送到这个通道

	eth     Backend							// eth的协议
	chain   *core.BlockChain				// 区块链
	proc    core.Validator					// 区块链验证器
	chainDb ethdb.Database					// 区块链数据库

	coinbase common.Address					// 挖矿者的地址
	extra    []byte

	currentMu sync.Mutex
	current   *Work

	uncleMu        sync.Mutex
	possibleUncles map[common.Hash]*types.Block			//可能的叔父节点

	unconfirmed *unconfirmedBlocks // set of locally mined blocks pending canonicalness confirmations

	// atomic status counters
	//原子状态计数器
	mining int32
	atWork int32

	quitCh  chan struct{}
	stopper chan struct{}
}

func newWorker(config *params.ChainConfig, engine consensus.Engine, coinbase common.Address, eth Backend, mux *event.TypeMux) *worker {
	worker := &worker{
		config:         config,
		engine:         engine,
		eth:            eth,
		mux:            mux,
		txCh:           make(chan core.TxPreEvent, txChanSize),
		chainHeadCh:    make(chan core.ChainHeadEvent, chainHeadChanSize),
		chainDb:        eth.ChainDb(),
		//用于接收从agent那传过来的result,dpos中没有agent故直接由worker发送
		recv:           make(chan *Result, resultQueueSize),
		chain:          eth.BlockChain(),
		proc:           eth.BlockChain().Validator(),
		possibleUncles: make(map[common.Hash]*types.Block),
		coinbase:       coinbase,
		unconfirmed:    newUnconfirmedBlocks(eth.BlockChain(), miningLogAtDepth),
		quitCh:         make(chan struct{}, 1),
		stopper:        make(chan struct{}, 1),
	}
	// Subscribe TxPreEvent for tx pool
	//注册订阅交易池，并向其发送交易。用于接收和发送交易？
	//注册TxPreEvent事件到tx pool交易池
	//第一个订阅内存池交易处理的地方
	worker.txSub = eth.TxPool().SubscribeTxPreEvent(worker.txCh)
	// Subscribe events for blockchain
	//注册订阅区块链事件，用于接收和发送区块？
	worker.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(worker.chainHeadCh)

	//接收外部区块及链消息，分类处理。即接收上面两个订阅中的消息并进行处理
	//开启了一个goroutine来接收TxPreEvent
	go worker.update()
	//接收自身挖矿消息，即监听recv,用来接收Agent发过来的Result的(dpos中直接由worker返回)
	//监听recv，把新区块写入数据库从而更新世界状态。
	go worker.wait()
	//创建新的打块任务，即启动下一次打包
	worker.createNewWork()

	return worker
}

func (self *worker) setCoinbase(addr common.Address) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.coinbase = addr
}

func (self *worker) setExtra(extra []byte) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.extra = extra
}

func (self *worker) pending() (*types.Block, *state.StateDB) {
	self.currentMu.Lock()
	defer self.currentMu.Unlock()

	if atomic.LoadInt32(&self.mining) == 0 {
		return types.NewBlock(
			self.current.header,
			self.current.txs,
			nil,
			self.current.receipts,
		), self.current.state.Copy()
	}
	return self.current.Block, self.current.state.Copy()
}

func (self *worker) pendingBlock() *types.Block {
	self.currentMu.Lock()
	defer self.currentMu.Unlock()

	if atomic.LoadInt32(&self.mining) == 0 {
		return types.NewBlock(
			self.current.header,
			self.current.txs,
			nil,
			self.current.receipts,
		)
	}
	return self.current.Block
}

func (self *worker) start() {
	self.mu.Lock()
	defer self.mu.Unlock()
	//再次将正在挖矿标志设置为1？
	atomic.StoreInt32(&self.mining, 1)
	//自身启动挖矿的主循环，而原以太坊中会遍历所有的Agent，调用它们的Start()函数
	go self.mintLoop()
}

func (self *worker) mintBlock(now int64) {
	//初始化dpos共识引擎
	engine, ok := self.engine.(*dpos.Dpos)
	if !ok {
		log.Error("Only the dpos engine was allowed")
		return
	}
	//检查当前链的最新区块情况（是否最新区块，是否是未来的区块，区块时间戳是否正确），及其验证节点情况（验证节点签名情况是否正确）
	//并非检查新挖出来的块，因为此时还没有出块
	err := engine.CheckValidator(self.chain.CurrentBlock(), now)
	if err != nil {
		switch err {
		case dpos.ErrWaitForPrevBlock,
			dpos.ErrMintFutureBlock,
			dpos.ErrInvalidBlockValidator,
			dpos.ErrInvalidMintBlockTime:
			log.Debug("Failed to mint the block, while ", "err", err)
		default:
			log.Error("Failed to mint the block", "err", err)
		}
		return
	}
	//createNewWork()是一个关键函数，完成主要的区块打包工作。
	work, err := self.createNewWork()
	if err != nil {
		log.Error("Failed to create the new work", "err", err)
		return
	}
	//对新块进行签名
	result, err := self.engine.Seal(self.chain, work.Block, self.quitCh)
	if err != nil {
		log.Error("Failed to seal the block", "err", err)
		return
	}
	//把挖到的新块给接收通道，跳过agent的任何处理，直接提交到recv进行最终区块的处理
	self.recv <- &Result{work, result}
}

//挖矿的主循环，利用channel进行控制挖矿、停止和退出
func (self *worker) mintLoop() {
	//使用channel控制定时器
	ticker := time.NewTicker(time.Second).C
	for {
		select {
		//定时器时间到了就可以进行挖矿，确保10秒钟出一个区块？
		case now := <-ticker:
			self.mintBlock(now.Unix())
		case <-self.stopper:
			close(self.quitCh)
			self.quitCh = make(chan struct{}, 1)
			self.stopper = make(chan struct{}, 1)
			return
		}
	}
}

func (self *worker) stop() {
	if atomic.LoadInt32(&self.mining) == 0 {
		return
	}

	self.wg.Wait()

	self.mu.Lock()
	defer self.mu.Unlock()

	atomic.StoreInt32(&self.mining, 0)
	atomic.StoreInt32(&self.atWork, 0)
	close(self.stopper)
}

//如果结点不挖矿的话，这里会立即调用commitTransactions()提交给EVM执行，获得本地回执。
//如果结点挖矿的话，miner会调用commitNewWork()，内部也会调用commitTransactions()执行交易。
func (self *worker) update() {
	defer self.txSub.Unsubscribe()
	defer self.chainHeadSub.Unsubscribe()

	for {
		// A real event arrived, process interesting content
		select {
		// Handle ChainHeadEvent
		////当接收到一个区块头的信息的时候，马上开启挖矿服务。
		case <-self.chainHeadCh:
			close(self.quitCh)
			self.quitCh = make(chan struct{}, 1)

		// Handle TxPreEvent
		//接收到交易消息
		// Handle TxPreEvent 接收到txPool里面的交易信息的时候。
		case ev := <-self.txCh:
			// Apply transaction to the pending state if we're not mining
			//// 如果当前没有挖矿， 那么把交易应用到当前的状态上，以便马上开启挖矿任务。
			if atomic.LoadInt32(&self.mining) == 0 {
				self.currentMu.Lock()
				acc, _ := types.Sender(self.current.signer, ev.Tx)
				txs := map[common.Address]types.Transactions{acc: {ev.Tx}}
				txset := types.NewTransactionsByPriceAndNonce(self.current.signer, txs)

				self.current.commitTransactions(self.mux, txset, self.chain, self.coinbase)
				self.currentMu.Unlock()
			}
		// System stopped
		case <-self.txSub.Err():
			return
		case <-self.chainHeadSub.Err():
			return
		}
	}
}

//wait函数用来接受挖矿的结果然后写入本地区块链，同时通过eth协议广播出去。
func (self *worker) wait() {
	//无限循环，从recv这个channel中读取Result，获得Work和Block
	for {
		for result := range self.recv {
			//原子增值，atwork-1表明停止挖矿工作
			atomic.AddInt32(&self.atWork, -1)

			if result == nil || result.Block == nil {
				continue
			}
			block := result.Block
			work := result.Work

			// Update the block hash in all logs since it is now available and not when the
			// receipt/log of individual transactions were created.
			//1 修改Log中的区块hash值
			//这个Log是用来记录智能合约执行过程中产生的event的。由于之前区块尚未生成，所以无法计算区块的hash值，
			//现在已经生成了，因此需要更新每个Log的BlockHash字段。
			//更新所有日志中的块哈希，因为它现在可用，而不是在创建单个事务的接收/日志时。
			//取出每个交易的结果，Receipt收据代表交易的结果。
			for _, r := range work.receipts {
				//更新交易结果日志中的区块哈希
				for _, l := range r.Logs {
					l.BlockHash = block.Hash()
				}
			}
			//更新状态数据库的日志中的区块哈希
			for _, log := range work.state.Logs() {
				log.BlockHash = block.Hash()
			}
			//2 将区块和状态信息写入数据库
			stat, err := self.chain.WriteBlockAndState(block, work.receipts, work.state)
			if err != nil {
				log.Error("Failed writing block to chain", "err", err)
				continue
			}
			// check if canon block and write transactions
			// 说明已经插入到规范的区块链
			if stat == core.CanonStatTy {
				// implicit by posting ChainHeadEvent
				// 【dpos中去掉了】
				// 因为这种状态下，会发送ChainHeadEvent，会触发上面的update里面的代码，
				// 这部分代码会commitNewWork，所以在这里就不需要commit了。
			}
			// Broadcast the block and announce chain insertion event
			//3 发送NewMinedBlockEvent事件
			//发送这个事件是为了把新挖出的区块广播给其他结点，事件处理代码位于eth/handler.go的minedBroadcastLoop()函数
			// 广播区块，并且申明区块链插入事件。
			self.mux.Post(core.NewMinedBlockEvent{Block: block})
			//4 发送ChainEvent事件，似乎只有filter订阅了这个事件？
			var (
				events []interface{}
				logs   = work.state.Logs()
			)
			events = append(events, core.ChainEvent{Block: block, Hash: block.Hash(), Logs: logs})
			if stat == core.CanonStatTy {
				events = append(events, core.ChainHeadEvent{Block: block})
			}
			//广播区块链插入事件
			self.chain.PostChainEvents(events, logs)

			// Insert the block into the set of pending ones to wait for confirmations
			// 插入本地跟踪列表， 查看后续的确认状态。
			self.unconfirmed.Insert(block.NumberU64(), block.Hash())
			log.Info("Successfully sealed new block", "number", block.Number(), "hash", block.Hash())
		}
	}
}

// makeCurrent creates a new environment for the current cycle.
//makeCurrent为当前的周期创建一个新的环境。
//makeCurrent()函数首先根据父块状态创建了一个新的StateDB实例。
//然后创建Work实例，主要初始化了它的state和header字段。
//接下来还要更新Work中和叔块（Uncle Block）相关的字段
//最后把新创建的Work实例赋值给self.current字段
func (self *worker) makeCurrent(parent *types.Block, header *types.Header) error {
	state, err := self.chain.StateAt(parent.Root())
	if err != nil {
		return err
	}
	dposContext, err := types.NewDposContextFromProto(self.chainDb, parent.Header().DposContext)
	if err != nil {
		return err
	}
	work := &Work{
		config:      self.config,
		signer:      types.NewEIP155Signer(self.config.ChainId),
		state:       state,
		dposContext: dposContext,
		ancestors:   set.New(),
		family:      set.New(),
		uncles:      set.New(),
		header:      header,
		createdAt:   time.Now(),
	}

	// when 08 is processed ancestors contain 07 (quick block)
	for _, ancestor := range self.chain.GetBlocksFromHash(parent.Hash(), 7) {
		for _, uncle := range ancestor.Uncles() {
			work.family.Add(uncle.Hash())
		}
		work.family.Add(ancestor.Hash())
		work.ancestors.Add(ancestor.Hash())
	}

	// Keep track of transactions which return errors so they can be removed
	work.tcount = 0
	self.current = work
	return nil
}
//createNewWork 提交新的任务
func (self *worker) createNewWork() (*Work, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.uncleMu.Lock()
	defer self.uncleMu.Unlock()
	self.currentMu.Lock()
	defer self.currentMu.Unlock()

	tstart := time.Now()
	parent := self.chain.CurrentBlock()

	tstamp := tstart.Unix()
	// 不能出现比parent的时间还早的情况
	if parent.Time().Cmp(new(big.Int).SetInt64(tstamp)) >= 0 {
		tstamp = parent.Time().Int64() + 1
	}
	// this will ensure we're not going off too far in the future
	//这将确保我们未来不会走得太远
	// 我们的时间不要超过现在的时间太远， 那么等待一段时间，
	// 感觉这个功能完全是为了测试实现的， 如果是真实的挖矿程序，应该不会等待。
	if now := time.Now().Unix(); tstamp > now+1 {
		wait := time.Duration(tstamp-now) * time.Second
		log.Info("Mining too far in the future", "wait", common.PrettyDuration(wait))
		time.Sleep(wait)
	}

	//1、创建新区块头
	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   core.CalcGasLimit(parent),
		GasUsed:    new(big.Int),
		Extra:      self.extra,
		Time:       big.NewInt(tstamp),
	}
	// Only set the coinbase if we are mining (avoid spurious block rewards)
	// 只有当我们挖矿的时候才设置coinbase(避免虚假的块奖励？ TODO 没懂)
	if atomic.LoadInt32(&self.mining) == 1 {
		header.Coinbase = self.coinbase
	}
	//2、初始化共识引擎。调用dpos共识引擎的Prepare()函数。共识接口，初始化区块头信息。
	if err := self.engine.Prepare(self.chain, header); err != nil {
		return nil, fmt.Errorf("got error when preparing header, err: %s", err)
	}
	// If we are care about TheDAO hard-fork check whether to override the extra-data or not
	// 根据我们是否关心DAO硬分叉来决定是否覆盖额外的数据。
	if daoBlock := self.config.DAOForkBlock; daoBlock != nil {
		// Check whether the block is among the fork extra-override range
		// 检查区块是否在 DAO硬分叉的范围内   [daoblock,daoblock+limit]
		limit := new(big.Int).Add(daoBlock, params.DAOForkExtraRange)
		if header.Number.Cmp(daoBlock) >= 0 && header.Number.Cmp(limit) < 0 {
			// Depending whether we support or oppose the fork, override differently
			// 如果我们支持DAO 那么设置保留的额外的数据
			if self.config.DAOForkSupport {
				header.Extra = common.CopyBytes(params.DAOForkBlockExtra)
			} else if bytes.Equal(header.Extra, params.DAOForkBlockExtra) {
				header.Extra = []byte{} // If miner opposes, don't let it use the reserved extra-data
			}
		}
	}

	// Could potentially happen if starting to mine in an odd state.
	// 3、创建新Work。用新的区块头来设置当前的状态
	//makeCurrent()函数首先根据父块状态创建了一个新的StateDB实例。
	//然后创建Work实例，主要初始化了它的state和header字段。
	//接下来还要更新Work中和叔块（Uncle Block）相关的字段
	//最后把新创建的Work实例赋值给self.current字段
	err := self.makeCurrent(parent, header)
	if err != nil {
		return nil, fmt.Errorf("got error when create mining context, err: %s", err)
	}
	// Create the current work task and check any fork transitions needed
	work := self.current
	// 把DAO里面的资金转移到指定的账户。
	if self.config.DAOForkSupport && self.config.DAOForkBlock != nil && self.config.DAOForkBlock.Cmp(header.Number) == 0 {
		misc.ApplyDAOHardFork(work.state)
	}
	//得到阻塞的资金
	//首先获取txpool的待处理交易列表的一个拷贝，然后封装进一个TransactionsByPriceAndNonce类型的结构中。
	//这个结构中包含一个heads字段，把交易按照gas price进行排序
	pending, err := self.eth.TxPool().Pending()
	if err != nil {
		return nil, fmt.Errorf("got error when fetch pending transactions, err: %s", err)
	}
	// 创建交易
	txs := types.NewTransactionsByPriceAndNonce(self.current.signer, pending)
	// 4、执行交易
	//调用commitTransactions()把交易提交到EVM去执行
	work.commitTransactions(self.mux, txs, self.chain, self.coinbase)

	// compute uncles for the new block.
	//5、处理叔区块
	//遍历所有叔块，然后调用commitUncle()把叔块header的hash添加进Work.uncles集合中。
	//以太坊规定每个区块最多打包2个叔块的header，每打包一个叔块可以获得挖矿报酬的1/32。
	var (
		uncles    []*types.Header
		badUncles []common.Hash
	)
	for hash, uncle := range self.possibleUncles {
		if len(uncles) == 2 {
			break
		}
		//commitUncle()函数会用之前初始化的几个集合来验证叔块的有效性，以太坊规定叔块必须是之前2～7层的祖先的直接子块。
		// 如果发现叔块无效，会从集合中剔除。
		if err := self.commitUncle(work, uncle.Header()); err != nil {
			log.Trace("Bad uncle found and will be removed", "hash", hash)
			log.Trace(fmt.Sprint(uncle))

			badUncles = append(badUncles, hash)
		} else {
			log.Debug("Committing new uncle to block", "hash", hash)
			uncles = append(uncles, uncle.Header())
		}
	}
	for _, hash := range badUncles {
		delete(self.possibleUncles, hash)
	}
	// Create the new block to seal with the consensus engine
	//6、打包新区块
	//只需要把header、txs、uncles、receipts送到共识引擎的Finalize()函数中生成新区块
	//创建新块以使用共识引擎进行封装，即打包新块，得到最终确定的新区块
	// 使用给定的状态来创建新的区块，Finalize会进行区块奖励等操作
	if work.Block, err = self.engine.Finalize(self.chain, header, work.state, work.txs, uncles, work.receipts, work.dposContext); err != nil {
		return nil, fmt.Errorf("got error when finalize block for sealing, err: %s", err)
	}
	work.Block.DposContext = work.dposContext

	// update the count for the miner of new block
	// We only care about logging if we're actually mining.
	//7、向Agent推送Work（dpos中被干掉了，因为不需要agent做pow工作了，因此直接返回work）
	//更新新区块矿工的计数，日志中只关心我们是否在实际挖矿
	if atomic.LoadInt32(&self.mining) == 1 {
		log.Info("Commit new mining work", "number", work.Block.Number(), "txs", work.tcount, "uncles", len(uncles), "elapsed", common.PrettyDuration(time.Since(tstart)))
		//上一步已经生成新区块了，这里会先把它放进未经确认的区块列表unconfirmed中
		self.unconfirmed.Shift(work.Block.NumberU64() - 1)
	}
	//以太坊pow中会将work推送到agent，并通过其获取的channel进行最终work的返回，
	// 这里直接返回最终区块即可，由调用函数直接返回给recv通道
	return work, nil
}

//commitUncle()函数会用之前初始化的几个集合来验证叔块的有效性，以太坊规定叔块必须是之前2～7层的祖先的直接子块。
// 如果发现叔块无效，会从集合中剔除。
func (self *worker) commitUncle(work *Work, uncle *types.Header) error {
	hash := uncle.Hash()
	if work.uncles.Has(hash) {
		return fmt.Errorf("uncle not unique")
	}
	if !work.ancestors.Has(uncle.ParentHash) {
		return fmt.Errorf("uncle's parent unknown (%x)", uncle.ParentHash[0:4])
	}
	if work.family.Has(hash) {
		return fmt.Errorf("uncle already in family (%x)", hash)
	}
	work.uncles.Add(uncle.Hash())
	return nil
}

////调用commitTransactions()把交易提交到EVM去执行
func (env *Work) commitTransactions(mux *event.TypeMux, txs *types.TransactionsByPriceAndNonce, bc *core.BlockChain, coinbase common.Address) {
	//GasPool是uint64类型，初始值为GasLimit，后面每执行一笔交易都会递减。
	gp := new(core.GasPool).AddGas(env.header.GasLimit)

	var coalescedLogs []*types.Log
	//进入循环，首先判断当前剩余的gas是否还够执行一笔交易，如果不够的话就退出循环。
	//从交易列表中取出一个交易，计算出发送方地址，进而提交给EVM执行。
	for {
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()

		if tx == nil {
			break
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		from, _ := types.Sender(env.signer, tx)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		// 请参考 https://github.com/ethereum/EIPs/blob/master/EIPS/eip-155.md
		// DAO事件发生后，以太坊分裂为ETH和ETC,因为两个链上的东西一摸一样，所以在ETC
		// 上面发生的交易可以拿到ETH上面进行重放， 反之亦然。 所以Vitalik提出了EIP155来避免这种情况。
		if tx.Protected() && !env.config.IsEIP155(env.header.Number) {
			log.Trace("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", env.config.EIP155Block)

			txs.Pop()
			continue
		}
		// Start executing the transaction
		//开始执行交易
		//Prepare()函数仅仅是几个赋值操作，记录了交易的hash，块hash目前为空，txIndex表明这是正在执行的第几笔交易。
		env.state.Prepare(tx.Hash(), common.Hash{}, env.tcount)
		//commitTransaction()函数首先获取当前状态的快照，然后调用ApplyTransaction()执行交易
		//如果交易执行失败，则回滚到之前的快照状态并返回错误，该账户的所有后续交易都将被跳过（txs.Pop()）
		//如果交易执行成功，则记录该交易以及交易执行的回执（receipt）并返回，然后移动到下一笔交易（txs.Shift()）
		err, logs := env.commitTransaction(tx, bc, coinbase, gp)
		switch err {
		case core.ErrGasLimitReached:
			// Pop the current out-of-gas transaction without shifting in the next from the account
			// 弹出整个账户的所有交易， 不处理用户的下一个交易。
			log.Trace("Gas limit exceeded for current block", "sender", from)
			txs.Pop()

		case core.ErrNonceTooLow:
			// New head notification data race between the transaction pool and miner, shift
			// 移动到用户的下一个交易
			log.Trace("Skipping transaction with low nonce", "sender", from, "nonce", tx.Nonce())
			txs.Shift()

		case core.ErrNonceTooHigh:
			// Reorg notification data race between the transaction pool and miner, skip account =
			// 跳过这个账户
			log.Trace("Skipping account with hight nonce", "sender", from, "nonce", tx.Nonce())
			txs.Pop()

		case nil:
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			env.tcount++
			txs.Shift()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			// 其他奇怪的错误，跳过这个交易。
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			txs.Shift()
		}
	}

	if len(coalescedLogs) > 0 || env.tcount > 0 {
		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		// 因为需要把log发送出去，而这边在挖矿完成后需要对log进行修改，所以拷贝一份发送出去，避免争用。
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		go func(logs []*types.Log, tcount int) {
			if len(logs) > 0 {
				mux.Post(core.PendingLogsEvent{Logs: logs})
			}
			if tcount > 0 {
				mux.Post(core.PendingStateEvent{})
			}
		}(cpy, env.tcount)
	}
}

//commitTransaction()函数首先获取当前状态的快照，然后调用ApplyTransaction()执行交易
//如果交易执行失败，则回滚到之前的快照状态并返回错误，该账户的所有后续交易都将被跳过（txs.Pop()）
//如果交易执行成功，则记录该交易以及交易执行的回执（receipt）并返回，然后移动到下一笔交易（txs.Shift()）
func (env *Work) commitTransaction(tx *types.Transaction, bc *core.BlockChain, coinbase common.Address, gp *core.GasPool) (error, []*types.Log) {
	snap := env.state.Snapshot()
	dposSnap := env.dposContext.Snapshot()
	receipt, _, err := core.ApplyTransaction(env.config, env.dposContext, bc, &coinbase, gp, env.state, env.header, tx, env.header.GasUsed, vm.Config{})
	if err != nil {
		env.state.RevertToSnapshot(snap)
		env.dposContext.RevertToSnapShot(dposSnap)
		return err, nil
	}
	env.txs = append(env.txs, tx)
	env.receipts = append(env.receipts, receipt)

	return nil, receipt.Logs
}
