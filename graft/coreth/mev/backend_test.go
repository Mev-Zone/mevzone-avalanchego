package mev

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/ava-labs/avalanchego/graft/coreth/mev/types"
	"github.com/ava-labs/avalanchego/graft/coreth/params"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/snowtest"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/version"
	avalancheWarp "github.com/ava-labs/avalanchego/vms/platformvm/warp"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	types2 "github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/rlp"
	"github.com/stretchr/testify/require"
)

var (
	cChainID    = ids.GenerateTestID()
	chainID     = big.NewInt(43114)
	pk, _       = crypto.GenerateKey()
	builderAddr = crypto.PubkeyToAddress(pk.PublicKey)
	config      = Config{
		Builders: []BuilderConfig{
			{Address: builderAddr},
		},
		ValidatorCommission: 500,
		ValidatorWallet:     common.Address{},
	}

	errNoBidWithHeight = errors.New("no bid found with this height")
)

type mockEth struct {
	H *types2.Header
}

func (m *mockEth) CurrentHeader() *types2.Header { return m.H }

type mockBuilder func(ctx context.Context, out *types.BidArgs, args *types.HexParams) error

func (f mockBuilder) Bid(ctx context.Context, out *types.BidArgs, args *types.HexParams) error {
	return f(ctx, out, args)
}

type mockBidSim struct {
	builders map[common.Address]Builder
}

func (m *mockBidSim) ExistBuilder(builder common.Address) bool {
	_, ok := m.builders[builder]
	return ok
}
func (m *mockBidSim) CheckPending(uint64, common.Address, common.Hash) error {
	return nil
}
func (m *mockBidSim) SendBid(context.Context, *types.Bid) error { return nil }
func (m *mockBidSim) Builders() map[common.Address]Builder {
	return m.builders
}

func TestBackend_MevParams(t *testing.T) {
	t.Parallel()

	snowCtx := snowtest.Context(t, cChainID)

	mevBackend, errB := NewBackend(
		snowCtx,
		config,
		nil,
		&params.ChainConfig{
			ChainID: chainID,
		},
	)
	require.NoError(t, errB)

	p, err := mevBackend.MevParams()
	require.NoError(t, err)

	msg := verifyHexParams(t, snowCtx, p)

	var payload types.MevParams
	err = json.Unmarshal(msg.Payload, &payload)
	require.NoError(t, err)

	require.Equal(t, payload, types.MevParams{
		ValidatorCommission: config.ValidatorCommission,
		ValidatorWallet:     config.ValidatorWallet,
		Version:             version.Current.Semantic(),
	})
}

func TestBackend_FetchBids(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	snowCtx := snowtest.Context(t, cChainID)

	eth := &mockEth{
		H: &types2.Header{
			Number: big.NewInt(100),
		},
	}

	mevBackend, errB := NewBackend(
		snowCtx,
		config,
		eth,
		&params.ChainConfig{ChainID: chainID},
	)
	require.NoError(t, errB)

	tx := types2.NewTx(&types2.LegacyTx{
		Nonce:    1,
		GasPrice: big.NewInt(1),
		Gas:      21000,
		To:       &builderAddr,
		Value:    big.NewInt(1),
	})
	txSigned, err := types2.SignTx(tx, types2.LatestSignerForChainID(chainID), pk)
	require.NoError(t, err)

	txBytes, err := txSigned.MarshalBinary()
	require.NoError(t, err)

	okBid := types.BidArgs{
		RawBid: &types.RawBid{
			BlockNumber:       eth.H.Number.Uint64() + 1,
			ParentHash:        eth.H.Hash(),
			GasFee:            big.NewInt(1),
			GasUsed:           21000,
			Txs:               make([]hexutil.Bytes, 0),
			MevRewards:        big.NewInt(1),
			MevValidatorShare: big.NewInt(1),
			MevBurnShare:      big.NewInt(1),
		},
		BurnTx:          txBytes,
		PayBidTx:        txBytes,
		PayBidTxGasUsed: 21000,
	}

	bz, err := rlp.EncodeToBytes(okBid.RawBid)
	require.NoError(t, err)
	sig, err := crypto.Sign(crypto.Keccak256(bz), pk)
	require.NoError(t, err)

	okBid.Signature = sig

	bidsMap := make(map[int64]map[ids.NodeID]*types.BidArgs)
	bidsMap[eth.H.Number.Int64()+1] = make(map[ids.NodeID]*types.BidArgs)
	bidsMap[eth.H.Number.Int64()+1][snowCtx.NodeID] = &okBid

	mockBS := &mockBidSim{
		builders: map[common.Address]Builder{
			builderAddr: mockBuilder(func(_ context.Context, out *types.BidArgs, p *types.HexParams) error {
				msg := verifyHexParams(t, snowCtx, p)

				var payload types.BidParams
				err := json.Unmarshal(msg.Payload, &payload)
				require.NoError(t, err)
				if _, ok := bidsMap[payload.Height]; ok {
					*out = *bidsMap[payload.Height][snowCtx.NodeID]
					return nil
				}
				return errNoBidWithHeight
			}),
		},
	}
	mevBackend.SetBidSimulator(mockBS)

	errF := mevBackend.FetchBids(ctx, 12345)
	require.EqualError(t, errF.Err, "no bid found with this height")
	require.Equal(t, builderAddr, errF.Builder)

	errF = mevBackend.FetchBids(ctx, eth.H.Number.Int64()+1)
	require.NoError(t, errF.Err)
}

func TestBackend_InvalidCommission(t *testing.T) {
	t.Parallel()

	snowCtx := snowtest.Context(t, cChainID)

	eth := &mockEth{
		H: &types2.Header{
			Number: big.NewInt(100),
		},
	}

	c := Config{
		Builders: []BuilderConfig{
			{Address: builderAddr},
		},
		ValidatorCommission: 10001,
		ValidatorWallet:     common.Address{},
	}

	_, errB := NewBackend(
		snowCtx,
		c,
		eth,
		&params.ChainConfig{ChainID: chainID},
	)
	require.EqualError(t, errB, fmt.Sprintf("invalid validator commission: got %d, want in range [0, 10000]", c.ValidatorCommission))
}

func verifyHexParams(t *testing.T, snowCtx *snow.Context, p *types.HexParams) *avalancheWarp.UnsignedMessage {
	rawBytes, err := hex.DecodeString(p.HexMsg)
	require.NoError(t, err)
	msg, err := avalancheWarp.ParseUnsignedMessage(rawBytes)
	require.NoError(t, err)

	pkBytes, err := hex.DecodeString(p.PublicKey)
	require.NoError(t, err)
	pubKey, err := bls.PublicKeyFromCompressedBytes(pkBytes)
	require.NoError(t, err)
	require.Equal(t, pubKey, snowCtx.PublicKey)

	sigBytes, err := hex.DecodeString(p.Signature)
	require.NoError(t, err)
	sig, err := bls.SignatureFromBytes(sigBytes)
	require.NoError(t, err)
	require.True(t, bls.Verify(pubKey, sig, rawBytes))

	return msg
}
