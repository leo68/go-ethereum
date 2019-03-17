package dpos

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"sort"

	"github.com/meitu/go-ethereum/common"
	"github.com/meitu/go-ethereum/core/state"
	"github.com/meitu/go-ethereum/core/types"
	"github.com/meitu/go-ethereum/crypto"
	"github.com/meitu/go-ethereum/log"
	"github.com/meitu/go-ethereum/trie"
)

//周期上下文背景
type EpochContext struct {
	TimeStamp   int64
	DposContext *types.DposContext
	statedb     *state.StateDB
}

// countVotes
/*
	计票的逻辑也很简单:
	1. 先找出候选人对应投票人的列表
	2. 所有投票人的余额作为票数累积到候选人的总票数中
*/
func (ec *EpochContext) countVotes() (votes map[common.Address]*big.Int, err error) {
	votes = map[common.Address]*big.Int{}
	delegateTrie := ec.DposContext.DelegateTrie()
	candidateTrie := ec.DposContext.CandidateTrie()
	statedb := ec.statedb

	iterCandidate := trie.NewIterator(candidateTrie.NodeIterator(nil))
	existCandidate := iterCandidate.Next()
	if !existCandidate {
		return votes, errors.New("no candidates")
	}
	// 遍历候选人列表
	for existCandidate {
		candidate := iterCandidate.Value
		candidateAddr := common.BytesToAddress(candidate)
		delegateIterator := trie.NewIterator(delegateTrie.PrefixIterator(candidate))
		existDelegator := delegateIterator.Next()
		if !existDelegator {
			votes[candidateAddr] = new(big.Int)
			existCandidate = iterCandidate.Next()
			continue
		}
		// 遍历候选人对应的投票人列表
		for existDelegator {
			delegator := delegateIterator.Value
			score, ok := votes[candidateAddr]
			if !ok {
				score = new(big.Int)
			}
			delegatorAddr := common.BytesToAddress(delegator)
			// 获取投票人的余额作为票数累积到候选人的票数中
			weight := statedb.GetBalance(delegatorAddr)
			score.Add(score, weight)
			votes[candidateAddr] = score
			existDelegator = delegateIterator.Next()
		}
		existCandidate = iterCandidate.Next()
	}
	return votes, nil
}

//剔除上一个周期非活动的验证节点
func (ec *EpochContext) kickoutValidator(epoch int64) error {
	//获取验证节点列表
	validators, err := ec.DposContext.GetValidators()
	if err != nil {
		return fmt.Errorf("failed to get validator: %s", err)
	}
	//判断是否验证节点为空
	if len(validators) == 0 {
		return errors.New("no validator could be kickout")
	}
	//获取周期长度，秒
	epochDuration := epochInterval
	// First epoch duration may lt epoch interval,
	// while the first block time wouldn't always align with epoch interval,
	// so caculate the first epoch duartion with first block time instead of epoch interval,
	// prevent the validators were kickout incorrectly.
	//？ 第一个持续时间间隔可以是时期间隔，而第一个区块时间不会总是与时期间隔对齐，
	// 因此用第一个时间段而不是时期间隔计算第一个时期，防止验证器被错误地踢出。
	if ec.TimeStamp-timeOfFirstBlock < epochInterval {
		epochDuration = ec.TimeStamp - timeOfFirstBlock
	}
	//定义需要提出的接收切片，遍历验证节点列表
	needKickoutValidators := sortableAddresses{}
	for _, validator := range validators {
		//定义一个切片，用于存放周期需要及该周期的验证节点，周期序号+验证节点地址
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, uint64(epoch))
		//将validator分开为一个个字节传入key中保存，validator的地址为20字节
		key = append(key, validator.Bytes()...)
		cnt := int64(0)
		//获取返回存储在trie中的键的值，及获取每个节点的出块数。MintCntTrie 记录验证人在周期内的出块数目
		if cntBytes := ec.DposContext.MintCntTrie().Get(key); cntBytes != nil {
			cnt = int64(binary.BigEndian.Uint64(cntBytes))
		}
		//剔除规则：出块数 <  周期时间/区块间隔时间/最大验证节点数量/2 ，即不足平均应出块数的50%
		if cnt < epochDuration/blockInterval/ maxValidatorSize /2 {
			// not active validators need kickout
			needKickoutValidators = append(needKickoutValidators, &sortableAddress{validator, big.NewInt(cnt)})
		}
	}
	// no validators need kickout
	//看需要剔除的节点数量，并进行降序排列？
	needKickoutValidatorCnt := len(needKickoutValidators)
	if needKickoutValidatorCnt <= 0 {
		return nil
	}
	sort.Sort(sort.Reverse(needKickoutValidators))

	//确定候选人数量，1、小于需要剔除的节点数量+安全发小的话按查找树的最多候选人数量 2、大于的话取需要剔除的节点数量+安全发小的话按查找树的最多候选人数量
	//因为后面需要剔除needKickoutValidatorCnt个，故最终候选人数量的上限是safeSize
	candidateCount := 0
	iter := trie.NewIterator(ec.DposContext.CandidateTrie().NodeIterator(nil))
	for iter.Next() {
		candidateCount++
		//已定义：safeSize = maxValidatorSize*2/3 + 1
		if candidateCount >= needKickoutValidatorCnt+safeSize {
			break
		}
	}

	//遍历候选人列表并进行剔除操作
	for i, validator := range needKickoutValidators {
		// ensure candidate count greater than or equal to safeSize
		if candidateCount <= safeSize {
			log.Info("No more candidate can be kickout", "prevEpochID", epoch, "candidateCount", candidateCount, "needKickoutCount", len(needKickoutValidators)-i)
			return nil
		}
		//剔除
		if err := ec.DposContext.KickoutCandidate(validator.address); err != nil {
			return err
		}
		// if kickout success, candidateCount minus 1
		//没剔除成功一个，候选人数量减1
		candidateCount--
		log.Info("Kickout candidate", "prevEpochID", epoch, "candidate", validator.address.String(), "mintCnt", validator.weight.String())
	}
	return nil
}

//查找验证节点
func (ec *EpochContext) lookupValidator(now int64) (validator common.Address, err error) {
	validator = common.Address{}
	//计算时间的偏移量，即计算在该周期之后过了多久
	offset := now % epochInterval
	if offset%blockInterval != 0 {
		return common.Address{}, ErrInvalidMintBlockTime
	}
	//计算该周期已出块的数量
	offset /= blockInterval

	validators, err := ec.DposContext.GetValidators()
	if err != nil {
		return common.Address{}, err
	}
	validatorSize := len(validators)
	if validatorSize == 0 {
		return common.Address{}, errors.New("failed to lookup validator")
	}
	offset %= int64(validatorSize)
	return validators[offset], nil
}

//尝试选举
func (ec *EpochContext) tryElect(genesis, parent *types.Header) error {
	//创世块周期时间
	genesisEpoch := genesis.Time.Int64() / epochInterval
	//父区块周期时间
	prevEpoch := parent.Time.Int64() / epochInterval
	//当前周期时间
	currentEpoch := ec.TimeStamp / epochInterval
	//判断上一个块是否是创世块
	prevEpochIsGenesis := prevEpoch == genesisEpoch
	if prevEpochIsGenesis && prevEpoch < currentEpoch {
		prevEpoch = currentEpoch - 1
	}
	//序列化前一周期详情，从前一周期一直处理到当前周期？可以不是间隔1？
	prevEpochBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(prevEpochBytes, uint64(prevEpoch))
	iter := trie.NewIterator(ec.DposContext.MintCntTrie().PrefixIterator(prevEpochBytes))
	// 根据当前块和上一块的时间计算当前块和上一块是否属于同一个周期，
	// 如果是同一个周期，意味着当前块不是周期的第一块，不需要触发选举
	// 如果不是同一周期，说明当前块是该周期的第一块，则触发选举
	for i := prevEpoch; i < currentEpoch; i++ {
		// if prevEpoch is not genesis, kickout not active candidate
		//如果上一个块不是创世块，那么剔除非活动状态的候选人。首批候选人写死在创世块中，不能剔除。
		//迭代器迁移一位至当前周期
		// 如果前一个周期不是创世周期，触发踢出候选人规则
		// 踢出规则主要是看上一周期是否存在候选人出块少于特定阈值(50%), 如果存在则踢出
		if !prevEpochIsGenesis && iter.Next() {
			if err := ec.kickoutValidator(prevEpoch); err != nil {
				return err
			}
		}
		//统计得票,对候选人进行计票后按照票数由高到低来排序, 选出前 N 个
		// 这里需要注意的是当前对于成为候选人没有门槛限制很容易被恶意攻击
		votes, err := ec.countVotes()
		if err != nil {
			return err
		}
		candidates := sortableAddresses{}
		//获取每个候选人的得票并进行累加
		for candidate, cnt := range votes {
			candidates = append(candidates, &sortableAddress{candidate, cnt})
		}
		//候选人人数小于安全候选人数量，报错退出
		if len(candidates) < safeSize {
			return errors.New("too few candidates")
		}
		//候选人根据得票数进行排序
		sort.Sort(candidates)
		//候选人数量过多则根据设定大小取前N位
		if len(candidates) > maxValidatorSize {
			candidates = candidates[:maxValidatorSize]
		}

		// shuffle candidates
		//打乱候选人顺序，得到洗牌后的候选人列表
		//由于使用 seed 是由父块的 hash 以及当前周期编号组成，所以每个节点计算出来的验证人列表也会一致
		seed := int64(binary.LittleEndian.Uint32(crypto.Keccak512(parent.Hash().Bytes()))) + i
		r := rand.New(rand.NewSource(seed))
		for i := len(candidates) - 1; i > 0; i-- {
			j := int(r.Int31n(int32(i + 1)))
			candidates[i], candidates[j] = candidates[j], candidates[i]
		}
		sortedValidators := make([]common.Address, 0)
		for _, candidate := range candidates {
			sortedValidators = append(sortedValidators, candidate.address)
		}
		//建立新周期的验证人列表
		epochTrie, _ := types.NewEpochTrie(common.Hash{}, ec.DposContext.DB())
		ec.DposContext.SetEpoch(epochTrie)
		ec.DposContext.SetValidators(sortedValidators)
		log.Info("Come to new epoch", "prevEpoch", i, "nextEpoch", i+1)
	}
	return nil
}

//可分类的地址
type sortableAddress struct {
	address common.Address
	weight  *big.Int
}
type sortableAddresses []*sortableAddress

func (p sortableAddresses) Swap(i, j int) { p[i], p[j] = p[j], p[i] }
func (p sortableAddresses) Len() int      { return len(p) }
/*
	1 地址i的权重小于j的权重，返回false
	2 j>j权重，返回true
	3 如果权重相同，则比较字符串
*/
func (p sortableAddresses) Less(i, j int) bool {
	if p[i].weight.Cmp(p[j].weight) < 0 {
		return false
	} else if p[i].weight.Cmp(p[j].weight) > 0 {
		return true
	} else {
		return p[i].address.String() < p[j].address.String()
	}
}
