package integration_tests

// utils stolen from the interchaintests/conformance package by Strangelove
// https://github.com/strangelove-ventures/interchaintest/blob/v7/conformance/test.go
// we'll adapt them to our use case, and contribute back once we've made our mistakes here

import (
	"context"
	"fmt"
	transfertypes "github.com/cosmos/ibc-go/v7/modules/apps/transfer/types"
	"github.com/docker/docker/client"
	"github.com/strangelove-ventures/interchaintest/v7"
	"github.com/strangelove-ventures/interchaintest/v7/chain/cosmos"
	"github.com/strangelove-ventures/interchaintest/v7/ibc"
	"github.com/strangelove-ventures/interchaintest/v7/relayer"
	"github.com/strangelove-ventures/interchaintest/v7/testreporter"
	"github.com/strangelove-ventures/interchaintest/v7/testutil"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	userFaucetFund = int64(10_000_000_000)
	testCoinAmount = int64(1_000_000)
	pollHeightMax  = uint64(50)
)

type TxCache struct {
	Src []ibc.Tx
	Dst []ibc.Tx
}

type RelayerTestCase struct {
	Config RelayerTestCaseConfig
	// user on source chain
	Users []ibc.Wallet
	// temp storage in between test phases
	TxCache TxCache
}

type RelayerTestCaseConfig struct {
	Name string
	// which relayer capabilities are required to run this test
	RequiredRelayerCapabilities []relayer.Capability
	// function to run after the chains are started but before the relayer is started
	// e.g. send a transfer and wait for it to timeout so that the relayer will handle it once it is timed out
	PreRelayerStart func(context.Context, *testing.T, *RelayerTestCase, ibc.Chain, ibc.Chain, []ibc.ChannelOutput)
	// test after chains and relayers are started
	Test func(context.Context, *testing.T, *RelayerTestCase, *testreporter.Reporter, ibc.Chain, ibc.Chain, []ibc.ChannelOutput)
}

var relayerTestCaseConfigs = [...]RelayerTestCaseConfig{
	{
		Name:            "relay packet",
		PreRelayerStart: preRelayerStart_RelayPacket,
		Test:            testPacketRelaySuccess,
	},
	{
		Name:            "no timeout",
		PreRelayerStart: preRelayerStart_NoTimeout,
		Test:            testPacketRelaySuccess,
	},
	{
		Name:                        "height timeout",
		RequiredRelayerCapabilities: []relayer.Capability{relayer.HeightTimeout},
		PreRelayerStart:             preRelayerStart_HeightTimeout,
		Test:                        testPacketRelayFail,
	},
	{
		Name:                        "timestamp timeout",
		RequiredRelayerCapabilities: []relayer.Capability{relayer.TimestampTimeout},
		PreRelayerStart:             preRelayerStart_TimestampTimeout,
		Test:                        testPacketRelayFail,
	},
}

// requireCapabilities tracks skipping t, if the relayer factory cannot satisfy the required capabilities.
func requireCapabilities(t *testing.T, rep *testreporter.Reporter, rf interchaintest.RelayerFactory, reqCaps ...relayer.Capability) {
	t.Helper()

	missing := missingCapabilities(rf, reqCaps...)

	if len(missing) > 0 {
		rep.TrackSkip(t, "skipping due to missing relayer capabilities +%s", missing)
	}
}

func missingCapabilities(rf interchaintest.RelayerFactory, reqCaps ...relayer.Capability) []relayer.Capability {
	caps := rf.Capabilities()
	var missing []relayer.Capability
	for _, c := range reqCaps {
		if !caps[c] {
			missing = append(missing, c)
		}
	}
	return missing
}

func sendIBCTransfersFromBothChainsWithTimeout(
	ctx context.Context,
	t *testing.T,
	testCase *RelayerTestCase,
	srcChain ibc.Chain,
	dstChain ibc.Chain,
	channels []ibc.ChannelOutput,
	timeout *ibc.IBCTimeout,
) {
	srcChainCfg := srcChain.Config()
	srcUser := testCase.Users[0]

	dstChainCfg := dstChain.Config()
	dstUser := testCase.Users[1]

	// will send ibc transfers from user wallet on both chains to their own respective wallet on the other chain

	testCoinSrcToDst := ibc.WalletAmount{
		Address: srcUser.(*cosmos.CosmosWallet).FormattedAddressWithPrefix(dstChainCfg.Bech32Prefix),
		Denom:   srcChainCfg.Denom,
		Amount:  testCoinAmount,
	}
	testCoinDstToSrc := ibc.WalletAmount{
		Address: dstUser.(*cosmos.CosmosWallet).FormattedAddressWithPrefix(srcChainCfg.Bech32Prefix),
		Denom:   dstChainCfg.Denom,
		Amount:  testCoinAmount,
	}

	var eg errgroup.Group
	srcTxs := make([]ibc.Tx, len(channels))
	dstTxs := make([]ibc.Tx, len(channels))

	eg.Go(func() (err error) {
		for i, channel := range channels {
			srcChannelID := channel.ChannelID
			srcTxs[i], err = srcChain.SendIBCTransfer(ctx, srcChannelID, srcUser.KeyName(), testCoinSrcToDst, ibc.TransferOptions{Timeout: timeout})
			if err != nil {
				return fmt.Errorf("failed to send ibc transfer from source: %w", err)
			}
			if err := testutil.WaitForBlocks(ctx, 1, srcChain); err != nil {
				return err
			}
		}
		return nil
	})

	eg.Go(func() (err error) {
		for i, channel := range channels {
			dstChannelID := channel.Counterparty.ChannelID
			dstTxs[i], err = dstChain.SendIBCTransfer(ctx, dstChannelID, dstUser.KeyName(), testCoinDstToSrc, ibc.TransferOptions{Timeout: timeout})
			if err != nil {
				return fmt.Errorf("failed to send ibc transfer from destination: %w", err)
			}
			if err := testutil.WaitForBlocks(ctx, 1, dstChain); err != nil {
				return err
			}
		}
		return nil
	})

	require.NoError(t, eg.Wait())
	for _, srcTx := range srcTxs {
		require.NoError(t, srcTx.Validate(), "source ibc transfer tx is invalid")
	}
	for _, dstTx := range dstTxs {
		require.NoError(t, dstTx.Validate(), "destination ibc transfer tx is invalid")
	}

	testCase.TxCache = TxCache{
		Src: srcTxs,
		Dst: dstTxs,
	}
}

// TestChainPair runs the conformance tests for two chains and one relayer.
// This test asserts bidirectional behavior between both chains.
//
// Given 2 chains, Chain A and Chain B, this test asserts:
// 1. Successful IBC transfer from A -> B and B -> A.
// 2. Proper handling of no timeout from A -> B and B -> A.
// 3. Proper handling of height timeout from A -> B and B -> A.
// 4. Proper handling of timestamp timeout from A -> B and B -> A.
// If a non-nil relayerImpl is passed, it is assumed that the chains are already started.
func TestChainPair(
	t *testing.T,
	ctx context.Context,
	client *client.Client,
	network string,
	srcChain, dstChain ibc.Chain,
	rf interchaintest.RelayerFactory,
	rep *testreporter.Reporter,
	relayerImpl ibc.Relayer,
	pathNames ...string,
) {
	req := require.New(rep.TestifyT(t))

	var (
		preRelayerStartFuncs []func([]ibc.ChannelOutput)
		testCases            []*RelayerTestCase
		err                  error
	)

	randomSuffix := RandLowerCaseLetterString(4)

	for _, testCaseConfig := range relayerTestCaseConfigs {
		testCase := RelayerTestCase{
			Config: testCaseConfig,
		}
		testCases = append(testCases, &testCase)

		if len(missingCapabilities(rf, testCaseConfig.RequiredRelayerCapabilities...)) > 0 {
			// Do not add preRelayerStartFunc if capability missing.
			// Adding all preRelayerStartFuncs appears to cause test pollution which is why this step is necessary.
			continue
		}
		preRelayerStartFunc := func(channels []ibc.ChannelOutput) {
			// fund a user wallet on both chains, save on test case
			testCase.Users = interchaintest.GetAndFundTestUsers(t, ctx, strings.ReplaceAll(testCase.Config.Name, " ", "-")+"-"+randomSuffix, userFaucetFund, srcChain, dstChain)
			// run test specific pre relayer start action
			testCase.Config.PreRelayerStart(ctx, t, &testCase, srcChain, dstChain, channels)
		}
		preRelayerStartFuncs = append(preRelayerStartFuncs, preRelayerStartFunc)
	}

	if relayerImpl == nil {
		t.Logf("creating relayer: %s", rf.Name())
		// startup both chains.
		// creates wallets in the relayer for src and dst chain.
		// funds relayer src and dst wallets on respective chain in genesis.
		// creates a faucet account on the both chains (separate fullnode).
		// funds faucet accounts in genesis.
		relayerImpl, err = interchaintest.StartChainPair(t, ctx, rep, client, network, srcChain, dstChain, rf, preRelayerStartFuncs)
		req.NoError(err, "failed to StartChainPair")
	}

	// execute the pre relayer start functions, then start the relayer.
	channels, err := StopStartRelayerWithPreStartFuncs(
		t,
		ctx,
		srcChain.Config().ChainID,
		dstChain.Config().ChainID,
		relayerImpl,
		rep.RelayerExecReporter(t),
		preRelayerStartFuncs,
		pathNames...,
	)
	req.NoError(err, "failed to StopStartRelayerWithPreStartFuncs")

	t.Run("post_relayer_start", func(t *testing.T) {
		for _, testCase := range testCases {
			testCase := testCase
			t.Run(testCase.Config.Name, func(t *testing.T) {
				rep.TrackTest(t)
				requireCapabilities(t, rep, rf, testCase.Config.RequiredRelayerCapabilities...)
				rep.TrackParallel(t)
				testCase.Config.Test(ctx, t, testCase, rep, srcChain, dstChain, channels)
			})
		}
	})
}

func RandLowerCaseLetterString(length int) string {
	var chars = []byte("abcdefghijklmnopqrstuvwxyz")
	b := make([]byte, length)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

func StopStartRelayerWithPreStartFuncs(
	t *testing.T,
	ctx context.Context,
	srcChainID string,
	dstChainID string,
	relayerImpl ibc.Relayer,
	eRep *testreporter.RelayerExecReporter,
	preRelayerStartFuncs []func([]ibc.ChannelOutput),
	pathNames ...string,
) ([]ibc.ChannelOutput, error) {
	if err := relayerImpl.StopRelayer(ctx, eRep); err != nil {
		t.Logf("error stopping relayer: %v", err)
	}

	channels := make([]ibc.ChannelOutput, 0)
	channel, err := ibc.GetTransferChannel(ctx, relayerImpl, eRep, srcChainID, dstChainID)
	if err != nil {
		return nil, fmt.Errorf("failed to get transfer channel: %w", err)
	}
	channels = append(channels, *channel)

	wg := sync.WaitGroup{}
	for _, preRelayerStart := range preRelayerStartFuncs {
		if preRelayerStart == nil {
			continue
		}
		preRelayerStart := preRelayerStart
		wg.Add(1)
		go func() {
			preRelayerStart(channels)
			wg.Done()
		}()
	}
	wg.Wait()

	if len(pathNames) == 0 {
		return nil, fmt.Errorf("len(pathNames) must be > 0")
	} else {
		if err := relayerImpl.StartRelayer(ctx, eRep, pathNames...); err != nil {
			return nil, fmt.Errorf("failed to start relayer: %w", err)
		}
	}

	// TODO: cleanup since this will stack multiple StopRelayer calls for
	// multiple calls to this func, requires StopRelayer to be idempotent.
	t.Cleanup(func() {
		if err := relayerImpl.StopRelayer(ctx, eRep); err != nil {
			t.Logf("error stopping relayer: %v", err)
		}
	})

	// wait for relayer(s) to start up
	time.Sleep(5 * time.Second)

	return channels, nil
}

// PreRelayerStart methods for the RelayerTestCases

func preRelayerStart_RelayPacket(ctx context.Context, t *testing.T, testCase *RelayerTestCase, srcChain ibc.Chain, dstChain ibc.Chain, channels []ibc.ChannelOutput) {
	sendIBCTransfersFromBothChainsWithTimeout(ctx, t, testCase, srcChain, dstChain, channels, nil)
}

func preRelayerStart_NoTimeout(ctx context.Context, t *testing.T, testCase *RelayerTestCase, srcChain ibc.Chain, dstChain ibc.Chain, channels []ibc.ChannelOutput) {
	ibcTimeoutDisabled := ibc.IBCTimeout{Height: 0, NanoSeconds: 0}
	sendIBCTransfersFromBothChainsWithTimeout(ctx, t, testCase, srcChain, dstChain, channels, &ibcTimeoutDisabled)
	// TODO should we wait here to make sure it successfully relays a packet beyond the default timeout period?
	// would need to shorten the chain default timeouts somehow to make that a feasible test
}

func preRelayerStart_HeightTimeout(ctx context.Context, t *testing.T, testCase *RelayerTestCase, srcChain ibc.Chain, dstChain ibc.Chain, channels []ibc.ChannelOutput) {
	ibcTimeoutHeight := ibc.IBCTimeout{Height: 10}
	sendIBCTransfersFromBothChainsWithTimeout(ctx, t, testCase, srcChain, dstChain, channels, &ibcTimeoutHeight)
	// wait for both chains to produce 15 blocks to expire timeout
	require.NoError(t, testutil.WaitForBlocks(ctx, 15, srcChain, dstChain), "failed to wait for blocks")
}

func preRelayerStart_TimestampTimeout(ctx context.Context, t *testing.T, testCase *RelayerTestCase, srcChain ibc.Chain, dstChain ibc.Chain, channels []ibc.ChannelOutput) {
	ibcTimeoutTimestamp := ibc.IBCTimeout{NanoSeconds: uint64((1 * time.Second).Nanoseconds())}
	sendIBCTransfersFromBothChainsWithTimeout(ctx, t, testCase, srcChain, dstChain, channels, &ibcTimeoutTimestamp)
	// wait for 15 seconds to expire timeout
	time.Sleep(15 * time.Second)
}

// Ensure that a queued packet is successfully relayed.
func testPacketRelaySuccess(
	ctx context.Context,
	t *testing.T,
	testCase *RelayerTestCase,
	rep *testreporter.Reporter,
	srcChain ibc.Chain,
	dstChain ibc.Chain,
	channels []ibc.ChannelOutput,
) {
	req := require.New(rep.TestifyT(t))

	srcChainCfg := srcChain.Config()
	srcUser := testCase.Users[0]
	srcDenom := srcChainCfg.Denom

	dstChainCfg := dstChain.Config()

	// [BEGIN] assert on source to destination transfer
	for i, srcTx := range testCase.TxCache.Src {
		t.Logf("Asserting %s to %s transfer", srcChainCfg.ChainID, dstChainCfg.ChainID)
		// Assuming these values since the ibc transfers were sent in PreRelayerStart, so balances may have already changed by now
		srcInitialBalance := userFaucetFund
		dstInitialBalance := int64(0)

		srcAck, err := testutil.PollForAck(ctx, srcChain, srcTx.Height, srcTx.Height+pollHeightMax, srcTx.Packet)
		req.NoError(err, "failed to get acknowledgement on source chain")
		req.NoError(srcAck.Validate(), "invalid acknowledgement on source chain")

		// get ibc denom for src denom on dst chain
		srcDenomTrace := transfertypes.ParseDenomTrace(transfertypes.GetPrefixedDenom(channels[i].Counterparty.PortID, channels[i].Counterparty.ChannelID, srcDenom))
		dstIbcDenom := srcDenomTrace.IBCDenom()

		srcFinalBalance, err := srcChain.GetBalance(ctx, srcUser.(*cosmos.CosmosWallet).FormattedAddressWithPrefix(srcChainCfg.Bech32Prefix), srcDenom)
		req.NoError(err, "failed to get balance from source chain")

		dstFinalBalance, err := dstChain.GetBalance(ctx, srcUser.(*cosmos.CosmosWallet).FormattedAddressWithPrefix(dstChainCfg.Bech32Prefix), dstIbcDenom)
		req.NoError(err, "failed to get balance from dest chain")

		totalFees := srcChain.GetGasFeesInNativeDenom(srcTx.GasSpent)
		expectedDifference := testCoinAmount + totalFees

		req.True(srcFinalBalance == srcInitialBalance-expectedDifference)
		req.True(dstFinalBalance == dstInitialBalance+testCoinAmount)
	}

	// [END] assert on source to destination transfer

	// [BEGIN] assert on destination to source transfer
	for i, dstTx := range testCase.TxCache.Dst {
		t.Logf("Asserting %s to %s transfer", dstChainCfg.ChainID, srcChainCfg.ChainID)
		dstUser := testCase.Users[1]
		dstDenom := dstChainCfg.Denom
		// Assuming these values since the ibc transfers were sent in PreRelayerStart, so balances may have already changed by now
		srcInitialBalance := int64(0)
		dstInitialBalance := userFaucetFund

		dstAck, err := testutil.PollForAck(ctx, dstChain, dstTx.Height, dstTx.Height+pollHeightMax, dstTx.Packet)
		req.NoError(err, "failed to get acknowledgement on destination chain")
		req.NoError(dstAck.Validate(), "invalid acknowledgement on destination chain")

		// Even though we poll for the ack, there may be timing issues where balances are not fully reconciled yet.
		// So we have a small buffer here.
		require.NoError(t, testutil.WaitForBlocks(ctx, 5, srcChain, dstChain))

		// get ibc denom for dst denom on src chain
		dstDenomTrace := transfertypes.ParseDenomTrace(transfertypes.GetPrefixedDenom(channels[i].PortID, channels[i].ChannelID, dstDenom))
		srcIbcDenom := dstDenomTrace.IBCDenom()

		srcFinalBalance, err := srcChain.GetBalance(ctx, dstUser.(*cosmos.CosmosWallet).FormattedAddressWithPrefix(srcChainCfg.Bech32Prefix), srcIbcDenom)
		req.NoError(err, "failed to get balance from source chain")

		dstFinalBalance, err := dstChain.GetBalance(ctx, dstUser.(*cosmos.CosmosWallet).FormattedAddressWithPrefix(dstChainCfg.Bech32Prefix), dstDenom)
		req.NoError(err, "failed to get balance from dest chain")

		totalFees := dstChain.GetGasFeesInNativeDenom(dstTx.GasSpent)
		expectedDifference := testCoinAmount + totalFees

		req.True(srcFinalBalance == srcInitialBalance+testCoinAmount)
		req.True(dstFinalBalance == dstInitialBalance-expectedDifference)
	}
	//[END] assert on destination to source transfer
}

// Ensure that a queued packet that should not be relayed is not relayed.
func testPacketRelayFail(
	ctx context.Context,
	t *testing.T,
	testCase *RelayerTestCase,
	rep *testreporter.Reporter,
	srcChain ibc.Chain,
	dstChain ibc.Chain,
	channels []ibc.ChannelOutput,
) {
	req := require.New(rep.TestifyT(t))

	srcChainCfg := srcChain.Config()
	srcUser := testCase.Users[0]
	srcDenom := srcChainCfg.Denom

	dstChainCfg := dstChain.Config()
	dstUser := testCase.Users[1]
	dstDenom := dstChainCfg.Denom

	// [BEGIN] assert on source to destination transfer
	for i, srcTx := range testCase.TxCache.Src {
		// Assuming these values since the ibc transfers were sent in PreRelayerStart, so balances may have already changed by now
		srcInitialBalance := userFaucetFund
		dstInitialBalance := int64(0)

		timeout, err := testutil.PollForTimeout(ctx, srcChain, srcTx.Height, srcTx.Height+pollHeightMax, srcTx.Packet)
		req.NoError(err, "failed to get timeout packet on source chain")
		req.NoError(timeout.Validate(), "invalid timeout packet on source chain")

		// Even though we poll for the timeout, there may be timing issues where balances are not fully reconciled yet.
		// So we have a small buffer here.
		require.NoError(t, testutil.WaitForBlocks(ctx, 5, srcChain, dstChain))

		// get ibc denom for src denom on dst chain
		srcDenomTrace := transfertypes.ParseDenomTrace(transfertypes.GetPrefixedDenom(channels[i].Counterparty.PortID, channels[i].Counterparty.ChannelID, srcDenom))
		dstIbcDenom := srcDenomTrace.IBCDenom()

		srcFinalBalance, err := srcChain.GetBalance(ctx, srcUser.(*cosmos.CosmosWallet).FormattedAddressWithPrefix(srcChainCfg.Bech32Prefix), srcDenom)
		req.NoError(err, "failed to get balance from source chain")

		dstFinalBalance, err := dstChain.GetBalance(ctx, srcUser.(*cosmos.CosmosWallet).FormattedAddressWithPrefix(dstChainCfg.Bech32Prefix), dstIbcDenom)
		req.NoError(err, "failed to get balance from destination chain")

		totalFees := srcChain.GetGasFeesInNativeDenom(srcTx.GasSpent)

		req.True(srcFinalBalance == srcInitialBalance-totalFees)
		req.True(dstFinalBalance == dstInitialBalance)
	}
	// [END] assert on source to destination transfer

	// [BEGIN] assert on destination to source transfer
	for i, dstTx := range testCase.TxCache.Dst {
		// Assuming these values since the ibc transfers were sent in PreRelayerStart, so balances may have already changed by now
		srcInitialBalance := int64(0)
		dstInitialBalance := userFaucetFund

		timeout, err := testutil.PollForTimeout(ctx, dstChain, dstTx.Height, dstTx.Height+pollHeightMax, dstTx.Packet)
		req.NoError(err, "failed to get timeout packet on destination chain")
		req.NoError(timeout.Validate(), "invalid timeout packet on destination chain")

		// Even though we poll for the timeout, there may be timing issues where balances are not fully reconciled yet.
		// So we have a small buffer here.
		require.NoError(t, testutil.WaitForBlocks(ctx, 5, srcChain, dstChain))

		// get ibc denom for dst denom on src chain
		dstDenomTrace := transfertypes.ParseDenomTrace(transfertypes.GetPrefixedDenom(channels[i].PortID, channels[i].ChannelID, dstDenom))
		srcIbcDenom := dstDenomTrace.IBCDenom()

		srcFinalBalance, err := srcChain.GetBalance(ctx, dstUser.(*cosmos.CosmosWallet).FormattedAddressWithPrefix(srcChainCfg.Bech32Prefix), srcIbcDenom)
		req.NoError(err, "failed to get balance from source chain")

		dstFinalBalance, err := dstChain.GetBalance(ctx, dstUser.(*cosmos.CosmosWallet).FormattedAddressWithPrefix(dstChainCfg.Bech32Prefix), dstDenom)
		req.NoError(err, "failed to get balance from destination chain")

		totalFees := dstChain.GetGasFeesInNativeDenom(dstTx.GasSpent)

		req.True(srcFinalBalance == srcInitialBalance)
		req.True(dstFinalBalance == dstInitialBalance-totalFees)
	}
	// [END] assert on destination to source transfer
}
