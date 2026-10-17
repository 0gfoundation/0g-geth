// Copyright 2021 The go-ethereum Authors
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

package gasprice

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// These tests build a small chain and check what eth_feeHistory reports for its
// empty blocks. The chain is built so the fee history inputs are exact:
//
//   - a constant block gas limit of 20M, the 0G floor (params.MinGasLimitPostFork),
//     with a chosen gas usage per block;
//   - the gas usage is spent by calling a contract that loops forever, so every
//     transaction burns exactly the gas it is given and the percentile weighting
//     sees exactly the intended ratio;
//   - a base fee of 1 gwei and a fee cap far above it, so the effective tip of a
//     transaction is its tip cap.

const (
	feeHistoryGasLimit = uint64(20_000_000) // 0G refuses a lower block gas limit
	feeHistoryFeeCap   = 100 * params.GWei
	feeHistoryStart    = params.GWei // the gas price the node is started with
)

// feeHistoryBurner loops forever, so a call to it consumes all the gas it is handed.
var feeHistoryBurner = common.Address{0xbb}

// feeHistoryBlock describes one generated block: the gas it must use and the tip of
// each transaction.
type feeHistoryBlock struct {
	gasUsed uint64
	tips    []uint64 // effective tip per transaction, in wei
}

// feeHistoryBackend serves the oracle from a generated chain, always at that chain's
// current head.
type feeHistoryBackend struct {
	chain *core.BlockChain
}

func (b *feeHistoryBackend) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error) {
	if number == rpc.LatestBlockNumber {
		return b.chain.CurrentHeader(), nil
	}
	if number < 0 {
		return nil, nil
	}
	return b.chain.GetHeaderByNumber(uint64(number)), nil
}

func (b *feeHistoryBackend) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	if number == rpc.LatestBlockNumber {
		return b.chain.GetBlockByNumber(b.chain.CurrentHeader().Number.Uint64()), nil
	}
	if number < 0 {
		return nil, nil
	}
	return b.chain.GetBlockByNumber(uint64(number)), nil
}

func (b *feeHistoryBackend) GetReceipts(ctx context.Context, hash common.Hash) (types.Receipts, error) {
	return b.chain.GetReceiptsByHash(hash), nil
}

func (b *feeHistoryBackend) Pending() (*types.Block, types.Receipts, *state.StateDB) {
	return nil, nil, nil
}

func (b *feeHistoryBackend) ChainConfig() *params.ChainConfig {
	return b.chain.Config()
}

func (b *feeHistoryBackend) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return nil
}

// newFeeHistoryChain generates a chain holding one block per spec, in block order, and
// checks that every block really did use the gas its spec asked for.
func newFeeHistoryChain(t *testing.T, specs []feeHistoryBlock) *core.BlockChain {
	t.Helper()

	var (
		key, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr   = crypto.PubkeyToAddress(key.PublicKey)
		config = *params.TestChainConfig // needs copy because it is modified below
	)
	config.LondonBlock = big.NewInt(0)
	config.ArrowGlacierBlock = big.NewInt(0)
	config.GrayGlacierBlock = big.NewInt(0)

	var (
		gspec = &core.Genesis{
			Config:   &config,
			GasLimit: feeHistoryGasLimit,
			BaseFee:  big.NewInt(params.GWei),
			Alloc: types.GenesisAlloc{
				addr:             {Balance: new(big.Int).Lsh(big.NewInt(1), 128)},
				feeHistoryBurner: {Code: []byte{0x5b, 0x60, 0x00, 0x56}}, // JUMPDEST PUSH1 0 JUMP
			},
		}
		signer = types.LatestSigner(gspec.Config)
	)
	db, blocks, _ := core.GenerateChainWithGenesis(gspec, beacon.New(ethash.NewFaker()), len(specs), func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.Address{1})

		spec := specs[i]
		n := len(spec.tips)
		if n == 0 {
			return
		}
		// Spread the block's gas usage over its transactions; the first one takes the remainder.
		perTx, rem := spec.gasUsed/uint64(n), spec.gasUsed%uint64(n)
		if perTx < 21000 {
			t.Fatalf("block %d: %d gas is too little for %d transactions", i+1, spec.gasUsed, n)
		}
		for j, tip := range spec.tips {
			gas := perTx
			if j == 0 {
				gas += rem
			}
			tx := types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
				ChainID:   gspec.Config.ChainID,
				Nonce:     b.TxNonce(addr),
				To:        &feeHistoryBurner,
				Gas:       gas,
				GasFeeCap: big.NewInt(feeHistoryFeeCap),
				GasTipCap: new(big.Int).SetUint64(tip),
			})
			b.AddTx(tx)
		}
	})

	chain, err := core.NewBlockChain(db, &core.CacheConfig{TrieCleanNoPrefetch: true}, gspec, nil, beacon.New(ethash.NewFaker()), vm.Config{}, nil)
	if err != nil {
		t.Fatalf("failed to create local chain, %v", err)
	}
	if i, err := chain.InsertChain(blocks); err != nil {
		t.Fatalf("error inserting block %d: %v", i, err)
	}
	t.Cleanup(chain.Stop)

	// The comparison is only meaningful if the generated blocks use the gas they were asked for.
	for i, spec := range specs {
		block := chain.GetBlockByNumber(uint64(i + 1))
		if got := block.GasUsed(); got != spec.gasUsed {
			t.Fatalf("block %d used %d gas, want %d", i+1, got, spec.gasUsed)
		}
	}
	return chain
}

// failingFeeHistoryBackend fails BlockByNumber for one block number, so the gas price
// suggestion cannot be computed.
type failingFeeHistoryBackend struct {
	*feeHistoryBackend
	failNumber uint64
}

func (b *failingFeeHistoryBackend) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	if number >= 0 && uint64(number) == b.failNumber {
		return nil, errors.New("block unavailable")
	}
	return b.feeHistoryBackend.BlockByNumber(ctx, number)
}

// TestFeeHistoryEmptyBlockRewards checks that an empty block's reward row reports the value
// eth_maxPriorityFeePerGas would return at request time, instead of plain zeros, while
// blocks holding transactions keep their sampled values.
func TestFeeHistoryEmptyBlockRewards(t *testing.T) {
	const gwei = uint64(params.GWei)

	// Three busy blocks (three txs each paying tips 1g/3g/5g, so the weighted
	// percentiles 10/50/90 sample exactly those tips) followed by two empty ones.
	specs := []feeHistoryBlock{
		{gasUsed: 12_000_000, tips: []uint64{gwei, 3 * gwei, 5 * gwei}},
		{gasUsed: 12_000_000, tips: []uint64{gwei, 3 * gwei, 5 * gwei}},
		{gasUsed: 12_000_000, tips: []uint64{gwei, 3 * gwei, 5 * gwei}},
		{gasUsed: 0}, // empty block
		{gasUsed: 0}, // empty block
	}
	chain := newFeeHistoryChain(t, specs)
	oracle := NewOracle(&feeHistoryBackend{chain: chain}, Config{Blocks: 5, Percentile: 60, MaxHeaderHistory: 5, MaxBlockHistory: 5}, big.NewInt(feeHistoryStart))

	_, reward, _, _, _, _, err := oracle.FeeHistory(context.Background(), 5, rpc.LatestBlockNumber, []float64{10, 50, 90})
	if err != nil {
		t.Fatalf("FeeHistory failed: %v", err)
	}
	tip, err := oracle.SuggestTipCap(context.Background())
	if err != nil {
		t.Fatalf("SuggestTipCap failed: %v", err)
	}
	// Empty blocks report the suggested tip in every percentile tier.
	for _, i := range []int{3, 4} {
		for j, got := range reward[i] {
			if got.Cmp(tip) != 0 {
				t.Fatalf("empty block %d: reward[%d] = %d wei, want the suggested tip %d wei", i+1, j, got, tip)
			}
		}
	}
	// Blocks with transactions keep their sampled values.
	want := []*big.Int{big.NewInt(int64(gwei)), big.NewInt(3 * int64(gwei)), big.NewInt(5 * int64(gwei))}
	for i := 0; i < 3; i++ {
		for j, w := range want {
			if reward[i][j].Cmp(w) != 0 {
				t.Fatalf("busy block %d: reward[%d] = %d wei, want %d wei", i+1, j, reward[i][j], w)
			}
		}
	}
}

// TestFeeHistoryEmptyBlockFallbackOnError checks that when the suggested tip cannot be
// computed, FeeHistory still succeeds and empty blocks keep their zero rows.
func TestFeeHistoryEmptyBlockFallbackOnError(t *testing.T) {
	const gwei = uint64(params.GWei)

	// One busy block followed by four empty ones; the requested range (blocks 2-4)
	// excludes the head block (5), which is the one that fails to load.
	specs := []feeHistoryBlock{
		{gasUsed: 12_000_000, tips: []uint64{gwei, 3 * gwei, 5 * gwei}},
		{gasUsed: 0},
		{gasUsed: 0},
		{gasUsed: 0},
		{gasUsed: 0},
	}
	chain := newFeeHistoryChain(t, specs)
	backend := &failingFeeHistoryBackend{feeHistoryBackend: &feeHistoryBackend{chain: chain}, failNumber: 5}
	// A one-block suggestion window makes SuggestTipCap hit exactly the failing head.
	oracle := NewOracle(backend, Config{Blocks: 1, Percentile: 60, MaxHeaderHistory: 5, MaxBlockHistory: 5}, big.NewInt(feeHistoryStart))

	if _, err := oracle.SuggestTipCap(context.Background()); err == nil {
		t.Fatalf("SuggestTipCap should have failed on the unavailable block")
	}
	_, reward, _, _, _, _, err := oracle.FeeHistory(context.Background(), 3, rpc.BlockNumber(4), []float64{10, 50, 90})
	if err != nil {
		t.Fatalf("FeeHistory should succeed even when the suggestion fails: %v", err)
	}
	for i, row := range reward {
		for j, got := range row {
			if got.Sign() != 0 {
				t.Fatalf("block %d: reward[%d] = %d wei, want 0 (suggestion unavailable)", i+2, j, got)
			}
		}
	}
}
