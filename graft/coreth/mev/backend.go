package mev

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	types3 "github.com/ava-labs/avalanchego/graft/coreth/mev/types"
	"github.com/ava-labs/avalanchego/graft/coreth/params"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
	"github.com/ava-labs/libevm/common"
	types2 "github.com/ava-labs/libevm/core/types"
	ethparams "github.com/ava-labs/libevm/params"
	"go.uber.org/zap"
)

type errBuilder struct {
	Err     error
	Builder common.Address
}

type BuilderConfig struct {
	Address common.Address `json:"address"`
	URL     string         `json:"url"`
}

type Config struct {
	Builders            []BuilderConfig `json:"builders"`            // The list of builders
	ValidatorCommission uint64          `json:"validatorCommission"` // 100 means the validator claims 1% from block reward
	ValidatorWallet     common.Address  `json:"validatorWallet"`     // The wallet of the validator that gets the rewards
}

type Backend interface {
	SetBidSimulator(client BidSimulatorClient)
	MevParams() (*types3.HexParams, error)
	FetchBids(ctx context.Context, height int64) errBuilder
}

type EthereumClient interface {
	CurrentHeader() *types2.Header
}

type Builder interface {
	Bid(ctx context.Context, result *types3.BidArgs, args *types3.HexParams) error
}

type BidSimulatorClient interface {
	ExistBuilder(builder common.Address) bool
	CheckPending(blockNumber uint64, builder common.Address, bidHash common.Hash) error
	SendBid(ctx context.Context, bid *types3.Bid) error
	Builders() map[common.Address]Builder
}

type backend struct {
	ctx          *snow.Context
	config       Config
	b            EthereumClient
	bidSimulator BidSimulatorClient
	chainConfig  *params.ChainConfig
}

func NewBackend(
	ctx *snow.Context,
	config Config,
	eth EthereumClient,
	chainConfig *params.ChainConfig,
) (Backend, error) {
	if config.ValidatorCommission < 0 || config.ValidatorCommission > 10_000 {
		return nil, fmt.Errorf(
			"invalid validator commission: got %d, want in range [0, 10000]",
			config.ValidatorCommission,
		)
	}
	return &backend{
		ctx:         ctx,
		config:      config,
		b:           eth,
		chainConfig: chainConfig,
	}, nil
}

func (m *backend) FetchBids(ctx context.Context, height int64) errBuilder {
	builders := m.bidSimulator.Builders()
	if len(builders) == 0 {
		return errBuilder{Err: errors.New("no registered builders")}
	}

	p := &types3.BidParams{Height: height}

	msg, err := m.sign(p)
	if err != nil {
		return errBuilder{Err: err}
	}

	bidsCh := make(chan types3.BidArgs, len(builders))
	errCh := make(chan errBuilder, len(builders))

	var wg sync.WaitGroup
	wg.Add(len(builders))

	for addr, b := range builders {
		addr := addr
		b := b

		go func() {
			defer wg.Done()

			bctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			var result types3.BidArgs
			if err := b.Bid(bctx, &result, msg); err != nil {
				errCh <- errBuilder{Err: err, Builder: addr}
				return
			}
			bidsCh <- result
		}()
	}

	// Close channels once all bidders finished.
	go func() {
		wg.Wait()
		close(bidsCh)
		close(errCh)
	}()

	var (
		bids   []types3.BidArgs
		errors []errBuilder
	)
	for bid := range bidsCh {
		bids = append(bids, bid)
	}
	for e := range errCh {
		errors = append(errors, e)
	}

	sendErrs := make(chan errBuilder, len(bids))
	var sendWG sync.WaitGroup
	sendWG.Add(len(bids))

	for _, bid := range bids {
		bid := bid
		go func() {
			defer sendWG.Done()
			sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if err := m.sendBid(sctx, bid); err != nil {
				sendErrs <- errBuilder{Err: err}
			}
		}()
	}
	go func() {
		sendWG.Wait()
		close(sendErrs)
	}()

	for e := range sendErrs {
		errors = append(errors, e)
	}

	if len(errors) > 0 {
		for _, err := range errors {
			m.ctx.Log.Warn("FetchBids additional error",
				zap.String("builder", err.Builder.Hex()),
				zap.Error(err.Err))
		}
		return errors[0]
	}

	return errBuilder{}
}

// SendBid receives bid from the builders.
// If mev is not running or bid is invalid, return error.
// Otherwise, creates a builder bid for the given argument, submit it to the miner.
func (m *backend) sendBid(ctx context.Context, args types3.BidArgs) error {
	var (
		rawBid        = args.RawBid
		currentHeader = m.b.CurrentHeader()
	)

	if rawBid == nil {
		return types3.NewInvalidBidError("rawBid should not be nil")
	}

	// only support bidding for the next block not for the future block
	if rawBid.BlockNumber != currentHeader.Number.Uint64()+1 {
		return types3.NewInvalidBidError("stale block number or block in future")
	}

	if rawBid.ParentHash != currentHeader.Hash() {
		return types3.NewInvalidBidError(
			fmt.Sprintf("non-aligned parent hash: %v", currentHeader.Hash()))
	}

	if rawBid.GasFee == nil || rawBid.GasFee.Sign() <= 0 || rawBid.GasUsed == 0 {
		return types3.NewInvalidBidError("empty gasFee or empty gasUsed")
	}

	if rawBid.MevRewards == nil || rawBid.MevRewards.Sign() <= 0 {
		return types3.NewInvalidBidError("empty mevRewards")
	}

	if rawBid.MevValidatorShare == nil || rawBid.MevValidatorShare.Sign() <= 0 {
		return types3.NewInvalidBidError("empty mevValidatorShare")
	}

	if rawBid.MevBurnShare == nil || rawBid.MevBurnShare.Sign() <= 0 {
		return types3.NewInvalidBidError("empty mevBurnShare")
	}

	if len(args.BurnTx) == 0 {
		return types3.NewInvalidPayBidTxError("burnTx are must-have")
	}

	if len(args.PayBidTx) == 0 || args.PayBidTxGasUsed == 0 {
		return types3.NewInvalidPayBidTxError("payBidTx and payBidTxGasUsed are must-have")
	}

	if args.PayBidTxGasUsed > ethparams.TxGas {
		return types3.NewInvalidBidError(
			fmt.Sprintf("transfer tx gas used must be no more than %v", ethparams.TxGas))
	}

	builder, err := args.EcrecoverSender()
	if err != nil {
		return types3.NewInvalidBidError(fmt.Sprintf("invalid signature:%v", err))
	}

	if !m.bidSimulator.ExistBuilder(builder) {
		return types3.NewInvalidBidError("builder is not registered")
	}

	err = m.bidSimulator.CheckPending(args.RawBid.BlockNumber, builder, args.RawBid.Hash())
	if err != nil {
		return err
	}

	signer := types2.MakeSigner(m.chainConfig, big.NewInt(int64(args.RawBid.BlockNumber)), uint64(time.Now().Unix()))
	bid, err := args.ToBid(builder, signer)
	if err != nil {
		return types3.NewInvalidBidError(fmt.Sprintf("fail to convert bidArgs to bid, %v", err))
	}

	return m.bidSimulator.SendBid(ctx, bid)
}

func (m *backend) MevParams() (*types3.HexParams, error) {
	p := &types3.MevParams{
		ValidatorCommission: m.config.ValidatorCommission,
		ValidatorWallet:     m.config.ValidatorWallet,
		Version:             version.Current.Semantic(),
	}

	return m.sign(p)
}

func (m *backend) sign(data any) (*types3.HexParams, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	msg, err := warp.NewUnsignedMessage(m.ctx.NetworkID, m.ctx.ChainID, payload)
	if err != nil {
		return nil, err
	}

	sign, err := m.ctx.WarpSigner.Sign(msg)
	if err != nil {
		return nil, err
	}

	pkBytes := bls.PublicKeyToCompressedBytes(m.ctx.PublicKey)

	return &types3.HexParams{
		HexMsg:    hex.EncodeToString(msg.Bytes()),
		Signature: hex.EncodeToString(sign),
		PublicKey: hex.EncodeToString(pkBytes),
		SubnetID:  m.ctx.SubnetID.String(),
	}, nil
}

func (m *backend) SetBidSimulator(client BidSimulatorClient) {
	m.bidSimulator = client
}
