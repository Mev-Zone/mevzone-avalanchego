package miner

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"

	"github.com/ava-labs/avalanchego/graft/coreth/core"
	"github.com/ava-labs/avalanchego/graft/coreth/mev"
	"github.com/ava-labs/avalanchego/graft/coreth/mev/builderclient"
	bidTypes "github.com/ava-labs/avalanchego/graft/coreth/mev/types"
	"github.com/ava-labs/avalanchego/graft/coreth/params"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/snowtest"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/event"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
)

var (
	cChainID    = ids.GenerateTestID()
	chainID     = big.NewInt(43114)
	pk, _       = crypto.GenerateKey()
	builderAddr = crypto.PubkeyToAddress(pk.PublicKey)
	config      = mev.Config{
		Builders: []mev.BuilderConfig{
			{Address: builderAddr},
		},
		ValidatorCommission: 500,
		ValidatorWallet:     common.Address{},
	}
)

// --- mockSubscription ---

type mockSubscription struct {
	once  sync.Once
	errCh chan error
}

func newMockSubscription() *mockSubscription {
	return &mockSubscription{errCh: make(chan error)}
}

func (s *mockSubscription) Unsubscribe() {
	s.once.Do(func() { close(s.errCh) })
}

func (s *mockSubscription) Err() <-chan error { return s.errCh }

// --- mockChain ---
//
// Minimal chain that supports SubscribeChainHeadEvent and lets tests push heads.
type mockChain struct {
	mu     sync.Mutex
	subs   []*mockSubscription
	chans  []chan<- core.ChainHeadEvent
	closed bool
}

func newMockChain() *mockChain { return &mockChain{} }

// SubscribeChainHeadEvent registers a listener channel and returns a subscription.
func (mc *mockChain) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	sub := newMockSubscription()
	mc.subs = append(mc.subs, sub)
	mc.chans = append(mc.chans, ch)
	return sub
}

type mockEth struct {
	H *types.Header
}

func (m *mockEth) CurrentHeader() *types.Header { return m.H }

// Helpers

func newAddr(s string) common.Address { return common.HexToAddress(s) }

func rt(expectedBlk, packedBlk, packedValidator int64, parent common.Hash, blockNum uint64) *BidRuntime {
	return &BidRuntime{
		bid: &bidTypes.Bid{
			ParentHash:  parent,
			BlockNumber: blockNum,
			Builder:     builderAddr,
		},
		expectedBlockReward:   big.NewInt(expectedBlk),
		packedBlockReward:     big.NewInt(packedBlk),
		packedValidatorReward: big.NewInt(packedValidator),
		finished:              make(chan struct{}),
	}
}

func newBareSimulator() *bidSimulator {
	return &bidSimulator{
		builders:      make(map[common.Address]*builderclient.Client),
		simBidCh:      make(chan *simBidReq),
		newBidCh:      make(chan newBidPackage, 100),
		pending:       make(map[uint64]map[common.Address]map[common.Hash]struct{}),
		bestBid:       make(map[common.Hash]*BidRuntime),
		simulatingBid: make(map[common.Hash]*BidRuntime),
		chain:         newMockChain(),
		stats:         newStats(),
	}
}

// ---------- issueKindFromError ----------

func TestIssueKindFromError(t *testing.T) {
	cases := []struct {
		msg  string
		kind string
	}{
		{"gas used exceeds gas limit", "gas_limit"},
		{"invalid tx in bid, foo", "invalid_tx"},
		{"simulation abort due to better bid arrived", "aborted_better_bid"},
		{"bid gas price is lower than min gas price", "min_gas_price"},
		{"reward does not achieve the burn expectation", "burn_expectation"},
		{"reward does not achieve the validator expectation", "validator_expectation"},
		{"invalid bid size", "size_limit"},
		{"no bid found with this height", "fetch_height"},
		{"some other error", "other"},
	}

	for _, c := range cases {
		require.Equal(t, c.kind, issueKindFromError(errors.New(c.msg)), c.msg)
	}
}

// ---------- Pending map (CheckPending / AddPending) ----------

func TestCheckPendingAndAddPending(t *testing.T) {
	b := newBareSimulator()

	var (
		block uint64 = 100
		hash1        = common.HexToHash("0x1")
		hash2        = common.HexToHash("0x2")
		hash3        = common.HexToHash("0x3")
		hash4        = common.HexToHash("0x4")
	)

	// First three should be OK
	require.NoError(t, b.CheckPending(block, builderAddr, hash1))
	b.AddPending(block, builderAddr, hash1)

	require.NoError(t, b.CheckPending(block, builderAddr, hash2))
	b.AddPending(block, builderAddr, hash2)

	require.NoError(t, b.CheckPending(block, builderAddr, hash3))
	b.AddPending(block, builderAddr, hash3)

	// Duplicate
	require.Error(t, b.CheckPending(block, builderAddr, hash2))

	// Over the per-builder limit (maxBidPerBuilderPerBlock = 3)
	require.Error(t, b.CheckPending(block, builderAddr, hash4))
}

// ---------- Builders() copy + ExistBuilder ----------

func TestBuildersCopyAndExist(t *testing.T) {
	b := newBareSimulator()
	addr := newAddr("0xCAFE")
	// Simulate "added" builder without dialing (client can be nil)
	b.builders[addr] = nil

	// ExistBuilder returns true
	require.True(t, b.ExistBuilder(addr))

	// Builders returns a copy
	m := b.Builders()
	require.Len(t, m, 1)
	delete(m, addr)
	require.Len(t, b.builders, 1, "internal builders map must not be affected by external mutation")
}

// ---------- shouldSimulate logic ----------

func TestShouldSimulate_PreemptSimulatingIfBetter(t *testing.T) {
	b := newBareSimulator()
	parent := common.HexToHash("0xAAA")

	// An in-flight simulation exists
	sim := rt(10, 0, 0, parent, 1)
	b.SetSimulatingBid(parent, sim)

	// New is strictly better (expectedBlockReward)
	newRt := rt(11, 0, 0, parent, 1)

	should, ref := b.shouldSimulate(newRt)
	require.True(t, should)
	require.Equal(t, sim, ref)

	// New is not strictly better (expectedBlockReward)
	newRt = rt(10, 0, 0, parent, 1)
	should, ref = b.shouldSimulate(newRt)
	require.False(t, should)
	require.Equal(t, sim, ref)
}

func TestShouldSimulate_DontPreemptIfNotBetter(t *testing.T) {
	b := newBareSimulator()
	parent := common.HexToHash("0xBBB")

	sim := rt(10, 0, 0, parent, 1)
	b.SetSimulatingBid(parent, sim)

	newRt := rt(9, 0, 0, parent, 1)

	should, ref := b.shouldSimulate(newRt)
	require.False(t, should)
	require.Equal(t, sim, ref)
}

func TestShouldSimulate_VsBest(t *testing.T) {
	b := newBareSimulator()
	parent := common.HexToHash("0xCCC")

	best := rt(10, 0, 0, parent, 1)
	b.SetBestBid(parent, best)

	// Better than best => should simulate
	newRt := rt(11, 0, 0, parent, 1)
	should, ref := b.shouldSimulate(newRt)
	require.True(t, should)
	require.Equal(t, best, ref)

	// Worse than best => don't simulate
	worse := rt(9, 0, 0, parent, 1)
	should, ref = b.shouldSimulate(worse)
	require.False(t, should)
	require.Equal(t, best, ref)
}

func TestShouldSimulate_NoBestNoSimMeansYes(t *testing.T) {
	b := newBareSimulator()
	parent := common.HexToHash("0xDDD")
	newRt := rt(1, 0, 0, parent, 1)

	should, ref := b.shouldSimulate(newRt)
	require.True(t, should)
	require.Nil(t, ref)
}

// ---------- tryPublishBest behavior ----------

func TestTryPublishBest_SetsFirstAndReplacesIfBetter(t *testing.T) {
	b := newBareSimulator()
	parent := common.HexToHash("0xEEE")

	// First publish always wins
	first := rt(10, 10, 0, parent, 1)
	won := b.tryPublishBest(parent, first)
	require.True(t, won)
	require.Equal(t, first, b.GetBestBid(parent))

	// New with higher packedBlockReward replaces
	better := rt(5, 20, 0, parent, 1)
	won = b.tryPublishBest(parent, better)
	require.True(t, won)
	require.Equal(t, better, b.GetBestBid(parent))

	// Worse does not replace
	worse := rt(50, 1, 0, parent, 1)
	won = b.tryPublishBest(parent, worse)
	require.False(t, won)
	require.Equal(t, better, b.GetBestBid(parent))
}

func TestTryPublishBest_RequeuesPriorBestWhenQueueEmpty(t *testing.T) {
	b := newBareSimulator()
	parent := common.HexToHash("0xFFF")

	// Seed a best
	best := rt(10, 10, 0, parent, 1)
	require.True(t, b.tryPublishBest(parent, best))

	// Offer a "worse" runtime; it should NOT replace,
	worse := rt(9, 9, 0, parent, 1)
	require.False(t, b.tryPublishBest(parent, worse))

	select {
	case pkg := <-b.newBidCh:
		require.NotNil(t, pkg.bid)
		require.Equal(t, best.bid, pkg.bid)
	default:
		t.Fatalf("expected one re-enqueued best bid")
	}
}

func TestGetFinalBid_ShouldReturnNil(t *testing.T) {
	b := newBareSimulator()
	snowCtx := snowtest.Context(t, cChainID)

	mevBackend, errB := mev.NewBackend(snowCtx, config, &mockEth{
		H: &types.Header{
			Number: big.NewInt(100),
		},
	}, &params.ChainConfig{
		ChainID: chainID,
	})
	require.NoError(t, errB)
	mevBackend.SetBidSimulator(b)

	b.Init(context.Background(), mevBackend, config, snowCtx)

	parent := common.HexToHash("0xFFF")
	blockNumber := big.NewInt(100)
	h := &types.Header{ParentHash: parent, Number: blockNumber}
	currentBurn := uint256.NewInt(100)

	newBest := rt(10, 10, 90, parent, 1)
	won := b.tryPublishBest(parent, newBest)
	require.True(t, won)

	br := b.GetFinalBid(h, currentBurn)
	require.Nil(t, br)
}

func TestGetFinalBid_ShouldReturnBid(t *testing.T) {
	b := newBareSimulator()
	snowCtx := snowtest.Context(t, cChainID)

	mevBackend, errB := mev.NewBackend(snowCtx, config, &mockEth{
		H: &types.Header{
			Number: big.NewInt(100),
		},
	}, &params.ChainConfig{
		ChainID: chainID,
	})
	require.NoError(t, errB)
	mevBackend.SetBidSimulator(b)

	b.Init(context.Background(), mevBackend, config, snowCtx)

	parent := common.HexToHash("0xFFF")
	blockNumber := big.NewInt(100)
	h := &types.Header{ParentHash: parent, Number: blockNumber}
	currentBurn := uint256.NewInt(100)

	newBest := rt(10, 10, 91, parent, 1)
	won := b.tryPublishBest(parent, newBest)
	require.True(t, won)

	br := b.GetFinalBid(h, currentBurn)
	require.Equal(t, br, newBest)
}
