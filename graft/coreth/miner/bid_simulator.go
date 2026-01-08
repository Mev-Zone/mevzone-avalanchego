package miner

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ava-labs/avalanchego/graft/coreth/core"
	"github.com/ava-labs/avalanchego/graft/coreth/core/txpool"
	"github.com/ava-labs/avalanchego/graft/coreth/mev"
	"github.com/ava-labs/avalanchego/graft/coreth/mev/builderclient"
	types2 "github.com/ava-labs/avalanchego/graft/coreth/mev/types"
	"github.com/ava-labs/avalanchego/graft/coreth/params"
	"github.com/ava-labs/avalanchego/graft/coreth/precompile/precompileconfig"
	"github.com/ava-labs/avalanchego/graft/coreth/rpc"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/metrics"
	ethparams "github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"
	"go.uber.org/zap"
)

var _ bidWorker = (*worker)(nil)
var _ BidFetcher = (*bidSimulator)(nil)

const (
	// maxBidPerBuilderPerBlock is the max bid number per builder
	maxBidPerBuilderPerBlock = 3
	tries                    = 128
	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10
	// the default to wait for the mev miner to finish
	waitMEVMinerEndTimeLimit = 50 * time.Millisecond
)

const (
	commitInterruptBetterBid = iota
)

var (
	burnAddress           = common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	ErrUnrevertibleFailed = errors.New("no revertible transaction failed")
)

var (
	dialer = &net.Dialer{
		Timeout:   time.Second,
		KeepAlive: 60 * time.Second,
	}

	transport = &http.Transport{
		DialContext:         dialer.DialContext,
		MaxIdleConnsPerHost: 50,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
	}

	client = &http.Client{
		Timeout:   5 * time.Second,
		Transport: transport,
	}
)

type BidFetcher interface {
	GetFinalBid(header *types.Header, burn *uint256.Int) *BidRuntime
	Init(ctx context.Context, backend mev.Backend, config mev.Config, snowCtx *snow.Context)
	ExistBuilder(builder common.Address) bool
	CheckPending(blockNumber uint64, builder common.Address, bidHash common.Hash) error
	SendBid(ctx context.Context, bid *types2.Bid) error
	Builders() map[common.Address]mev.Builder
}

type bidWorker interface {
	createEnvironment(predicateContext *precompileconfig.PredicateContext, parentHash common.Hash) (*environment, error)
	commitTransaction(env *environment, tx *types.Transaction, coinbase common.Address) ([]*types.Log, error)
}

type BlockChain interface {
	SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription
}

// simBidReq is the request for simulating a bid
type simBidReq struct {
	bid         *BidRuntime
	interruptCh chan int32
}

// newBidPackage is the warp of a new bid and a feedback channel
type newBidPackage struct {
	bid      *types2.Bid
	feedback chan error
}

type simStats struct {
	profitableCounter   metrics.Counter
	unprofitableCounter metrics.Counter
	bidReceivedCounter  metrics.Counter
	bidSimTimer         metrics.Timer

	errorsTotal         metrics.Counter
	errorsRate          metrics.Meter
	issueReportSent     metrics.Counter
	issueReportSuccess  metrics.Counter
	issueReportFail     metrics.Counter
	issueReportDuration metrics.Timer
}

func newStats() simStats {
	return simStats{
		profitableCounter:   metrics.GetOrRegisterCounter("bid/profitable", nil),
		unprofitableCounter: metrics.GetOrRegisterCounter("bid/unprofitable", nil),
		bidReceivedCounter:  metrics.GetOrRegisterCounter("bid/received", nil),
		bidSimTimer:         metrics.NewRegisteredTimer("bid/sim/duration", nil),

		errorsTotal:         metrics.GetOrRegisterCounter("bid/errors_total", nil),
		errorsRate:          metrics.GetOrRegisterMeter("bid/errors_rate", nil),
		issueReportSent:     metrics.GetOrRegisterCounter("bid/issue_report/sent_total", nil),
		issueReportSuccess:  metrics.GetOrRegisterCounter("bid/issue_report/success_total", nil),
		issueReportFail:     metrics.GetOrRegisterCounter("bid/issue_report/fail_total", nil),
		issueReportDuration: metrics.NewRegisteredTimer("bid/issue_report/duration", nil),
	}
}

// bidSimulator is in charge of receiving bid from builders, reporting issue to builders.
// And take care of bid simulation, rewards computing, best bid maintaining.
type bidSimulator struct {
	config      *mev.Config
	minGasPrice *big.Int
	chain       BlockChain
	txpool      *txpool.TxPool
	chainConfig *params.ChainConfig
	bidWorker   bidWorker
	exitCh      chan int32

	bidReceiving atomic.Bool // controlled by config and eth.AdminAPI

	chainHeadCh  chan core.ChainHeadEvent
	chainHeadSub event.Subscription

	// builder info (warning: only keep status in memory!)
	buildersMu sync.RWMutex
	builders   map[common.Address]*builderclient.Client

	// channels
	simBidCh chan *simBidReq
	newBidCh chan newBidPackage

	pendingMu sync.RWMutex
	pending   map[uint64]map[common.Address]map[common.Hash]struct{} // blockNumber -> builder -> bidHash -> struct{}

	bestBidMu sync.RWMutex
	bestBid   map[common.Hash]*BidRuntime // prevBlockHash -> bidRuntime

	simBidMu      sync.RWMutex
	simulatingBid map[common.Hash]*BidRuntime // prevBlockHash -> bidRuntime, in the process of simulation

	stats   simStats
	snowCtx *snow.Context
	backend mev.Backend
}

func newBidSimulator(
	minGasPrice *big.Int,
	eth Backend,
	chainConfig *params.ChainConfig,
	bidWorker bidWorker,
) *bidSimulator {
	return &bidSimulator{
		minGasPrice:   minGasPrice,
		chain:         eth.BlockChain(),
		txpool:        eth.TxPool(),
		chainConfig:   chainConfig,
		bidWorker:     bidWorker,
		exitCh:        make(chan int32),
		chainHeadCh:   make(chan core.ChainHeadEvent, chainHeadChanSize),
		builders:      make(map[common.Address]*builderclient.Client),
		simBidCh:      make(chan *simBidReq),
		newBidCh:      make(chan newBidPackage, 100),
		pending:       make(map[uint64]map[common.Address]map[common.Hash]struct{}),
		bestBid:       make(map[common.Hash]*BidRuntime),
		simulatingBid: make(map[common.Hash]*BidRuntime),
		stats:         newStats(),
	}
}

func (b *bidSimulator) Close() {
	close(b.exitCh)
	b.chainHeadSub.Unsubscribe()
}

func (b *bidSimulator) Init(ctx context.Context, backend mev.Backend, config mev.Config, snowCtx *snow.Context) {
	b.snowCtx = snowCtx
	b.config = &config
	b.backend = backend
	b.chainHeadSub = b.chain.SubscribeChainHeadEvent(b.chainHeadCh)
	b.bidReceiving.Store(true)

	go b.dialBuilders(ctx)
	go b.clearLoop()
	go b.mainLoop()
	go b.newBidLoop()
}

func (b *bidSimulator) dialBuilders(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-b.exitCh:
			return
		case <-timer.C:
			params, err := b.backend.MevParams()
			if err != nil {
				b.snowCtx.Log.Warn("BidSimulator: error fetching mev params", zap.Error(err))
				continue
			}

			for _, v := range b.config.Builders {
				b.AddBuilder(ctx, v.Address, v.URL, params)
			}

			if len(b.builders) == 0 {
				b.snowCtx.Log.Warn("BidSimulator: no valid builders")
			}

			timer.Reset(5 * time.Minute)
		}
	}
}

func (b *bidSimulator) receivingBid() bool {
	return b.bidReceiving.Load()
}

func (b *bidSimulator) Builders() map[common.Address]mev.Builder {
	b.buildersMu.RLock()
	defer b.buildersMu.RUnlock()

	cp := make(map[common.Address]mev.Builder, len(b.builders))
	for k, v := range b.builders {
		cp[k] = v
	}
	return cp
}

func (b *bidSimulator) AddBuilder(ctx context.Context, builder common.Address, url string, params *types2.HexParams) {
	b.buildersMu.Lock()
	defer b.buildersMu.Unlock()

	var builderCli *builderclient.Client

	if url != "" {
		var err error

		builderCli, err = builderclient.DialOptions(ctx, url, rpc.WithHTTPClient(client))
		if err != nil {
			kind := issueKindFromError(err)
			b.incIssueMetrics(builder, kind)
			b.snowCtx.Log.Error(
				"BidSimulator: failed to dial builder",
				zap.String("url", url),
				zap.Error(err),
			)
			return
		}
		err = builderCli.Notify(ctx, params)
		if err != nil {
			kind := issueKindFromError(err)
			b.incIssueMetrics(builder, kind)
			b.snowCtx.Log.Error(
				"BidSimulator: failed to notify builder",
				zap.String("url", url),
				zap.Error(err),
			)
			return
		}
		b.builders[builder] = builderCli
	}
}

func (b *bidSimulator) ExistBuilder(builder common.Address) bool {
	b.buildersMu.RLock()
	defer b.buildersMu.RUnlock()

	_, ok := b.builders[builder]

	return ok
}

func (b *bidSimulator) SetBestBid(prevBlockHash common.Hash, bid *BidRuntime) {
	b.bestBidMu.Lock()
	defer b.bestBidMu.Unlock()
	last := b.bestBid[prevBlockHash]
	if last != nil && last.env != nil {
		last.env.discard()
	}

	b.bestBid[prevBlockHash] = bid
}

func (b *bidSimulator) GetFinalBid(header *types.Header, currentBurn *uint256.Int) *BidRuntime {
	if !b.receivingBid() {
		return nil
	}

	err := b.backend.FetchBids(context.Background(), header.Number.Int64())
	if err.Err != nil && err.Builder != (common.Address{}) {
		kind := issueKindFromError(err.Err)
		b.incIssueMetrics(err.Builder, kind)
	}

	if pendingBid := b.GetSimulatingBid(header.ParentHash); pendingBid != nil {
		waitBidTimer := time.NewTimer(waitMEVMinerEndTimeLimit)
		defer waitBidTimer.Stop()
		select {
		case <-waitBidTimer.C:
		case <-pendingBid.finished:
		}
	}

	bestBid := b.GetBestBid(header.ParentHash)

	if bestBid != nil {
		b.snowCtx.Log.Info("BidSimulator: final compare",
			zap.Uint64("block", header.Number.Uint64()),
			zap.Stringer("validatorBurn", currentBurn),
			zap.Stringer("validatorRewards", bestBid.packedValidatorReward),
			zap.Stringer("blockReward", bestBid.packedBlockReward))

		mevProfit := new(big.Int).Add(bestBid.packedValidatorReward, bestBid.packedBlockReward)

		if currentBurn.CmpBig(mevProfit) < 0 {
			b.stats.profitableCounter.Inc(1)
			b.snowCtx.Log.Info("BidSimulator: bid accepted profitable",
				zap.Uint64("block", header.Number.Uint64()),
				zap.Stringer("builder", bestBid.bid.Builder),
				zap.Stringer("burn", bestBid.packedBlockReward),
				zap.Stringer("validatorReward", bestBid.packedValidatorReward),
				zap.String("bid", bestBid.bid.Hash().TerminalString()),
			)
			return bestBid
		} else {
			b.stats.unprofitableCounter.Inc(1)
			b.snowCtx.Log.Info("BidSimulator: bid skipped unprofitable",
				zap.Uint64("block", header.Number.Uint64()),
				zap.Stringer("builder", bestBid.bid.Builder),
				zap.Stringer("mevProfit", mevProfit),
				zap.Stringer("currentBurn", currentBurn),
				zap.String("bid", bestBid.bid.Hash().TerminalString()),
			)
		}
	}

	return nil
}

func (b *bidSimulator) GetBestBid(prevBlockHash common.Hash) *BidRuntime {
	b.bestBidMu.RLock()
	defer b.bestBidMu.RUnlock()

	return b.bestBid[prevBlockHash]
}

func (b *bidSimulator) SetSimulatingBid(prevBlockHash common.Hash, bid *BidRuntime) {
	b.simBidMu.Lock()
	defer b.simBidMu.Unlock()

	b.simulatingBid[prevBlockHash] = bid
}

func (b *bidSimulator) GetSimulatingBid(prevBlockHash common.Hash) *BidRuntime {
	b.simBidMu.RLock()
	defer b.simBidMu.RUnlock()

	return b.simulatingBid[prevBlockHash]
}

func (b *bidSimulator) RemoveSimulatingBid(prevBlockHash common.Hash) {
	b.simBidMu.Lock()
	defer b.simBidMu.Unlock()

	delete(b.simulatingBid, prevBlockHash)
}

func (b *bidSimulator) mainLoop() {
	defer b.chainHeadSub.Unsubscribe()
	for {
		select {
		case req := <-b.simBidCh:
			b.simBid(req.interruptCh, req.bid)
		case <-b.chainHeadSub.Err():
			return
		case <-b.exitCh:
			return
		}
	}
}

func (b *bidSimulator) newBidLoop() {
	var interruptCh chan int32

	// commit aborts in-flight bid execution (if any) and schedules the new one.
	commit := func(reason int32, bidRuntime *BidRuntime) {
		if interruptCh != nil {
			interruptCh <- reason
			close(interruptCh)
		}
		interruptCh = make(chan int32, 1)
		b.simBidCh <- &simBidReq{interruptCh: interruptCh, bid: bidRuntime}
		b.snowCtx.Log.Debug(
			"BidSimulator: commit",
			zap.Stringer("builder", bidRuntime.bid.Builder),
			zap.String("bidHash", bidRuntime.bid.Hash().Hex()),
		)
	}

	genDiscardedReply := func(better *BidRuntime) error {
		return fmt.Errorf(
			"bid is discarded, current bestBid is [blockReward: %s, validatorReward: %s]",
			better.expectedBlockReward, better.expectedValidatorReward,
		)
	}

	for {
		select {
		case <-b.exitCh:
			return
		case newBid := <-b.newBidCh:
			rt, err := newBidRuntime(newBid.bid, b.config.ValidatorCommission)
			if err != nil {
				b.snowCtx.Log.Warn(
					"BidSimulator: invalid bid",
					zap.Stringer("builder", newBid.bid.Builder),
					zap.Error(err),
				)
				if newBid.feedback != nil {
					newBid.feedback <- err
				}
				continue
			}

			should, ref := b.shouldSimulate(rt)
			var replyErr error
			if should {
				commit(commitInterruptBetterBid, rt)
			} else if ref != nil {
				replyErr = genDiscardedReply(ref)
			} else {
				// Shouldn't happen
				replyErr = errors.New("bid is discarded")
			}

			if newBid.feedback != nil {
				newBid.feedback <- replyErr
				b.snowCtx.Log.Info("BidSimulator: bid arrived",
					zap.Uint64("block", newBid.bid.BlockNumber),
					zap.Stringer("builder", newBid.bid.Builder),
					zap.Bool("accepted", replyErr == nil),
					zap.Stringer("blockReward", rt.expectedBlockReward),
					zap.Stringer("validatorReward", rt.expectedValidatorReward),
					zap.Int("tx", len(newBid.bid.Txs)),
					zap.String("hash", newBid.bid.Hash().TerminalString()),
				)
			}
		}
	}
}

// shouldSimulate decides whether to preempt the current work (if any) and
// simulate the new bid. It returns (shouldCommit, referenceBid).
// referenceBid is the currently-better runtime (simulating or best) used for msgs.
func (b *bidSimulator) shouldSimulate(newRt *BidRuntime) (bool, *BidRuntime) {
	parent := newRt.bid.ParentHash

	// If there's an in-flight simulation, only preempt if strictly better.
	if sim := b.GetSimulatingBid(parent); sim != nil {
		if newRt.isExpectedBetterThan(sim) {
			return true, sim
		}
		return false, sim
	}

	// Otherwise compare against the recorded best (if any).
	if best := b.GetBestBid(parent); best == nil || newRt.isExpectedBetterThan(best) {
		return true, best
	}
	return false, b.GetBestBid(parent)
}

func (b *bidSimulator) clearLoop() {
	clearFn := func(parentHash common.Hash, blockNumber uint64) {
		b.pendingMu.Lock()
		delete(b.pending, blockNumber)
		b.pendingMu.Unlock()

		b.bestBidMu.Lock()
		if bid, ok := b.bestBid[parentHash]; ok {
			bid.env.discard()
		}
		delete(b.bestBid, parentHash)

		for k, v := range b.bestBid {
			if v.bid.BlockNumber <= blockNumber-tries {
				v.env.discard()
				delete(b.bestBid, k)
			}
		}
		b.bestBidMu.Unlock()

		b.simBidMu.Lock()
		for k, v := range b.simulatingBid {
			if v.bid.BlockNumber <= blockNumber-tries {
				if v.env != nil {
					v.env.discard()
				}
				delete(b.simulatingBid, k)
			}
		}
		b.simBidMu.Unlock()
	}

	for {
		select {
		case <-b.exitCh:
			return
		case head := <-b.chainHeadCh:
			clearFn(head.Block.ParentHash(), head.Block.NumberU64())
		}
	}
}

// SendBid checks if the bid is already exists or if the builder sends too many bids,
// if yes, return error, if not, add bid into newBid chan waiting for judge profit.
func (b *bidSimulator) SendBid(_ context.Context, bid *types2.Bid) error {
	b.stats.bidReceivedCounter.Inc(1)

	timeout := time.After(time.Second)
	replyCh := make(chan error, 1)

	select {
	case b.newBidCh <- newBidPackage{bid: bid, feedback: replyCh}:
		b.AddPending(bid.BlockNumber, bid.Builder, bid.Hash())
	case <-timeout:
		return types2.ErrMevBusy
	}

	select {
	case reply := <-replyCh:
		return reply
	case <-timeout:
		return types2.ErrMevBusy
	}
}

func (b *bidSimulator) CheckPending(blockNumber uint64, builder common.Address, bidHash common.Hash) error {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()

	pm, ok := b.pending[blockNumber]
	if !ok {
		pm = make(map[common.Address]map[common.Hash]struct{})
		b.pending[blockNumber] = pm
	}
	bm, ok := pm[builder]
	if !ok {
		bm = make(map[common.Hash]struct{})
		pm[builder] = bm
	}

	if _, ok = bm[bidHash]; ok {
		return errors.New("bid already exists")
	}

	if len(bm) >= maxBidPerBuilderPerBlock {
		return errors.New("too many bids")
	}

	return nil
}

func (b *bidSimulator) AddPending(blockNumber uint64, builder common.Address, bidHash common.Hash) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()

	pm, ok := b.pending[blockNumber]
	if !ok {
		pm = make(map[common.Address]map[common.Hash]struct{})
		b.pending[blockNumber] = pm
	}
	bm, ok := pm[builder]
	if !ok {
		bm = make(map[common.Hash]struct{})
		pm[builder] = bm
	}
	bm[bidHash] = struct{}{}
}

// simBid simulates a newBid with txs.
// simBid does not enable state prefetching when commit transaction.
func (b *bidSimulator) simBid(interruptCh chan int32, rt *BidRuntime) {
	if !b.receivingBid() {
		return
	}

	startTS := time.Now()
	parentHash := rt.bid.ParentHash
	b.SetSimulatingBid(parentHash, rt)

	// Standard defers
	defer func(simStart time.Time) {
		if rt.env != nil && len(rt.env.receipts) == 0 {
			// failed early; ensure we stop prefetcher etc.
			rt.env.discard()
		}
		b.RemoveSimulatingBid(parentHash)
		close(rt.finished)
		if rt.env != nil && rt.env.state != nil && rt.env.tcount == 0 {
			rt.env.discard()
		}
		if rt.duration == 0 && len(rt.env.receipts) > 0 {
			rt.duration = time.Since(simStart)
			b.stats.bidSimTimer.UpdateSince(simStart)
		}
	}(startTS)

	var err error
	rt.env, err = b.bidWorker.createEnvironment(
		&precompileconfig.PredicateContext{SnowCtx: b.snowCtx, ProposerVMBlockCtx: nil},
		rt.bid.ParentHash,
	)
	if err != nil {
		b.reportIssue(rt, err)
		return
	}

	// Prepare gas pool for reserved burn/payBid gas
	gasLimit := rt.env.header.GasLimit
	if rt.env.gasPool == nil {
		rt.env.gasPool = new(core.GasPool).AddGas(gasLimit)
		rt.env.gasPool.SubGas(ethparams.TxGas) // reserve space like before
	}
	if rt.bid.GasUsed > rt.env.gasPool.Gas() {
		b.reportIssue(rt, errors.New("gas used exceeds gas limit"))
		return
	}

	// Balances snapshot for accounting
	rt.balanceBefore = rt.env.state.GetBalance(rt.env.header.Coinbase).ToBig()
	rt.balanceBeforeWallet = rt.env.state.GetBalance(b.config.ValidatorWallet).ToBig()
	rt.balanceBeforeBurn = rt.env.state.GetBalance(burnAddress).ToBig()

	txs := rt.bid.Txs
	n := len(txs)
	if n < 2 {
		b.reportIssue(rt, errors.New("bid must contain at least burn and payBid tx"))
		return
	}
	bodyEnd := n - 2
	burnTx := txs[n-2]
	payTx := txs[n-1]

	// === 1) Body txs
	for i := 0; i < bodyEnd; i++ {
		select {
		case <-interruptCh:
			b.reportIssue(rt, errors.New("simulation abort due to better bid arrived"))
			return
		default:
		}
		tx := txs[i]
		mustSucceed := rt.bid.UnRevertible.Contains(tx.Hash())

		if _, err = commitAndCheck(b.bidWorker, rt.env, tx, mustSucceed); err != nil {
			b.reportIssue(rt, fmt.Errorf("invalid tx in bid, %v", err))
			return
		}
	}

	// Reward sanity (block fee part)
	if err = b.checkBidReward(rt); err != nil {
		b.reportIssue(rt, err)
		return
	}

	// Min gas price check (only for non-mempool txs)
	if err = b.checkMinGasPrice(rt); err != nil {
		b.reportIssue(rt, err)
		return
	}

	// Burn tx (must succeed)
	rt.env.gasPool.AddGas(ethparams.TxGas)
	if _, err = commitAndCheck(b.bidWorker, rt.env, burnTx, true); err != nil {
		b.reportIssue(rt, fmt.Errorf("invalid tx in bid, %v", err))
		return
	}
	if err = b.checkBidBurn(rt); err != nil {
		b.reportIssue(rt, err)
		return
	}

	// Pay-bid tx (must succeed)
	rt.env.gasPool.AddGas(ethparams.TxGas)
	if _, err = commitAndCheck(b.bidWorker, rt.env, payTx, true); err != nil {
		b.reportIssue(rt, fmt.Errorf("invalid tx in bid, %v", err))
		return
	}
	if err = b.checkValidatorReward(rt); err != nil {
		b.reportIssue(rt, err)
		return
	}

	// Size limit
	if rt.env.size > targetTxsSize {
		b.reportIssue(rt, errors.New("invalid bid size"))
		return
	}

	// Publish result
	_ = b.tryPublishBest(parentHash, rt)

	rt.duration = time.Since(startTS)
}

func (b *bidSimulator) tryPublishBest(parent common.Hash, rt *BidRuntime) (won bool) {
	best := b.GetBestBid(parent)
	if best == nil || rt.packedBlockReward.Cmp(best.packedBlockReward) > 0 {
		b.SetBestBid(parent, rt)
		return true
	}
	if len(b.newBidCh) == 0 { // enqueue prior best for re-sim if queue is empty
		select {
		case b.newBidCh <- newBidPackage{bid: best.bid}:
		default:
		}
	}
	return false
}

func (b *bidSimulator) checkBidReward(rt *BidRuntime) error {
	rt.packReward()
	if !rt.validReward() {
		return fmt.Errorf("reward does not achieve the gas expectation, got: %v, expect: %v",
			rt.packedBlockReward, rt.expectedBlockReward)
	}
	return nil
}

func (b *bidSimulator) checkBidBurn(rt *BidRuntime) error {
	rt.packBurnShare()
	if !rt.validBurnShare() {
		return fmt.Errorf("reward does not achieve the burn expectation, got: %v, expect: %v",
			rt.packedBurnShare, rt.expectedBurnShare)
	}
	return nil
}

func (b *bidSimulator) checkValidatorReward(rt *BidRuntime) error {
	rt.packValidatorReward(b.config.ValidatorWallet)
	if !rt.validValidatorReward() {
		return fmt.Errorf("reward does not achieve the validator expectation, got: %v, expect: %v",
			rt.packedValidatorReward, rt.expectedValidatorReward)
	}
	return nil
}

// Average price only for txs NOT in the mempool, identical to your logic.
func (b *bidSimulator) checkMinGasPrice(rt *BidRuntime) error {
	var gasUsed uint64
	gasFee := new(big.Int)
	for i, rcpt := range rt.env.receipts {
		tx := rt.env.txs[i]
		if b.txpool.Has(tx.Hash()) {
			continue
		}
		gasUsed += rcpt.GasUsed

		effectiveTip, err := tx.EffectiveGasTip(rt.env.header.BaseFee)
		if err != nil {
			return errors.New("failed to calculate effective tip")
		}
		if rt.env.header.BaseFee != nil {
			effectiveTip.Add(effectiveTip, rt.env.header.BaseFee)
		}
		gasFee.Add(gasFee, new(big.Int).Mul(effectiveTip, new(big.Int).SetUint64(rcpt.GasUsed)))

		if tx.Type() == types.BlobTxType {
			gasFee.Add(gasFee, new(big.Int).Mul(rcpt.BlobGasPrice, new(big.Int).SetUint64(rcpt.BlobGasUsed)))
		}
	}
	if gasUsed == 0 {
		return nil
	}
	avg := new(big.Int).Div(gasFee, new(big.Int).SetUint64(gasUsed))
	if avg.Cmp(b.minGasPrice) < 0 {
		return fmt.Errorf("bid gas price is lower than min gas price, bid:%v, min:%v", avg, b.minGasPrice)
	}
	return nil
}

// commitAndCheck calls worker.commitTransaction and enforces success rules.
//   - mustSucceed: if true, fail the simulation when the receipt is Failed.
//     Use this for unRevertible body txs and for burn/payBid txs.
func commitAndCheck(w bidWorker, env *environment, tx *types.Transaction, mustSucceed bool) (*types.Receipt, error) {
	// Required by the worker path
	env.state.SetTxContext(tx.Hash(), env.tcount)

	// Delegate to worker
	if _, err := w.commitTransaction(env, tx, env.header.Coinbase); err != nil {
		return nil, err
	}
	if len(env.receipts) == 0 {
		return nil, errors.New("commitTransaction returned no receipt")
	}
	// Safe: worker appended the receipt on success
	rcpt := env.receipts[len(env.receipts)-1]

	// Enforce "must succeed" policy
	if mustSucceed && rcpt.Status == types.ReceiptStatusFailed {
		return rcpt, ErrUnrevertibleFailed
	}
	// Count only after success
	env.tcount++
	return rcpt, nil
}

func (b *bidSimulator) reportIssue(rt *BidRuntime, err error) {
	kind := issueKindFromError(err)
	b.incIssueMetrics(rt.bid.Builder, kind)

	cli := b.builders[rt.bid.Builder]
	if cli != nil {
		b.stats.issueReportSent.Inc(1)
		start := time.Now()
		repErr := cli.ReportIssue(context.Background(), &types2.BidIssue{
			Builder: rt.bid.Builder,
			BidHash: rt.bid.Hash(),
			Message: err.Error(),
		})
		b.stats.issueReportDuration.UpdateSince(start)

		if repErr != nil {
			b.stats.issueReportFail.Inc(1)
			b.snowCtx.Log.Warn("BidSimulator: failed to report issue",
				zap.Stringer("builder", rt.bid.Builder),
				zap.Error(repErr),
			)
		} else {
			b.stats.issueReportSuccess.Inc(1)
		}
	}
}

func issueKindFromError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "gas used exceeds gas limit"):
		return "gas_limit"
	case strings.Contains(msg, "invalid tx in bid"):
		return "invalid_tx"
	case strings.Contains(msg, "simulation abort"):
		return "aborted_better_bid"
	case strings.Contains(msg, "min gas price"):
		return "min_gas_price"
	case strings.Contains(msg, "burn expectation"):
		return "burn_expectation"
	case strings.Contains(msg, "validator expectation"):
		return "validator_expectation"
	case strings.Contains(msg, "invalid bid size"):
		return "size_limit"
	case strings.Contains(msg, "no bid found with this height"):
		return "fetch_height"
	default:
		return "other"
	}
}

func (b *bidSimulator) incIssueMetrics(builder common.Address, kind string) {
	b.stats.errorsTotal.Inc(1)
	b.stats.errorsRate.Mark(1)
	metrics.GetOrRegisterCounter(fmt.Sprintf("bid/errors_kind/%s", kind), nil).Inc(1)
	metrics.GetOrRegisterCounter(fmt.Sprintf("bid/errors_by_builder/%s", builder.Hex()), nil).Inc(1)
}

type BidRuntime struct {
	bid *types2.Bid

	env *environment

	expectedBlockReward     *big.Int
	expectedValidatorReward *big.Int
	expectedBurnShare       *big.Int

	packedBlockReward     *big.Int
	packedValidatorReward *big.Int
	packedBurnShare       *big.Int

	finished            chan struct{}
	duration            time.Duration
	balanceBefore       *big.Int
	balanceBeforeWallet *big.Int
	balanceBeforeBurn   *big.Int
}

func newBidRuntime(newBid *types2.Bid, validatorCommission uint64) (*BidRuntime, error) {
	// check the block reward and validator reward of the newBid
	expectedBurnShare := newBid.BurnAmounts
	expectedBlockReward := newBid.GasFee
	expectedValidatorReward := new(big.Int).Sub(newBid.MevRewards, expectedBurnShare)
	expectedValidatorReward.Mul(expectedValidatorReward, big.NewInt(int64(validatorCommission)))
	expectedValidatorReward.Div(expectedValidatorReward, big.NewInt(10000))

	if expectedValidatorReward.Cmp(newBid.ValidatorRewards) < 0 {
		return nil, fmt.Errorf("validator reward is less than promised, value: %s, promised: %s", expectedValidatorReward, newBid.ValidatorRewards)
	}

	if expectedValidatorReward.Cmp(big.NewInt(0)) < 0 {
		return nil, fmt.Errorf("validator reward is less than 0, value: %s, commissionConfig: %d", expectedValidatorReward, validatorCommission)
	}

	bidRuntime := &BidRuntime{
		bid:                     newBid,
		expectedBlockReward:     expectedBlockReward,
		expectedValidatorReward: expectedValidatorReward,
		expectedBurnShare:       expectedBurnShare,
		packedBlockReward:       big.NewInt(0),
		packedValidatorReward:   big.NewInt(0),
		packedBurnShare:         big.NewInt(0),
		finished:                make(chan struct{}),
	}

	return bidRuntime, nil
}

func (r *BidRuntime) validReward() bool {
	return r.packedBlockReward.Cmp(r.expectedBlockReward) >= 0
}

func (r *BidRuntime) validValidatorReward() bool {
	return r.packedValidatorReward.Cmp(r.expectedValidatorReward) >= 0
}

func (r *BidRuntime) validBurnShare() bool {
	return r.packedBurnShare.Cmp(r.expectedBurnShare) >= 0
}

func (r *BidRuntime) isExpectedBetterThan(other *BidRuntime) bool {
	return r.expectedBlockReward.Cmp(other.expectedBlockReward) > 0
}

// packReward calculates packedBlockReward
func (r *BidRuntime) packReward() {
	r.packedBlockReward = r.env.state.GetBalance(r.env.header.Coinbase).ToBig()
	r.packedBlockReward.Sub(r.packedBlockReward, r.balanceBefore)
}

// packValidatorReward calculates packedValidatorReward
func (r *BidRuntime) packValidatorReward(wallet common.Address) {
	r.packedValidatorReward = r.env.state.GetBalance(wallet).ToBig()
	r.packedValidatorReward.Sub(r.packedValidatorReward, r.balanceBeforeWallet)
}

// packBurnShare calculated packedBurnShare
func (r *BidRuntime) packBurnShare() {
	r.packedBurnShare = r.env.state.GetBalance(burnAddress).ToBig()
	r.packedBurnShare.Sub(r.packedBurnShare, r.balanceBeforeBurn)
}
