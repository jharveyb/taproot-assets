package tapgarden_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/chanutils"
	"github.com/lightninglabs/taproot-assets/internal/test"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapdb"
	_ "github.com/lightninglabs/taproot-assets/tapdb" // Register relevant drivers.
	"github.com/lightninglabs/taproot-assets/tapgarden"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/stretchr/testify/require"
)

// Default to a large interval so the planter never actually ticks and only
// rely on our manual ticks.
var (
	defaultInterval = time.Hour * 24
	defaultTimeout  = time.Second * 5
	minterInterval  = time.Millisecond * 250
)

// newMintingStore creates a new instance of the TapAddressBook book.
func newMintingStore(t *testing.T) tapgarden.MintingStore {
	db := tapdb.NewTestDB(t)

	txCreator := func(tx *sql.Tx) tapdb.PendingAssetStore {
		return db.WithTx(tx)
	}

	assetDB := tapdb.NewTransactionExecutor(db, txCreator)
	return tapdb.NewAssetMintingStore(assetDB)
}

// mintingTestHarness holds and manages all the set of deplanes needed to
// create succinct and fully featured unit/systems tests for the batched asset
// minting process.
type mintingTestHarness struct {
	wallet *tapgarden.MockWalletAnchor

	chain *tapgarden.MockChainBridge

	store tapgarden.MintingStore

	keyRing *tapgarden.MockKeyRing

	genSigner *tapgarden.MockGenSigner

	ticker *ticker.Force

	planter *tapgarden.ChainPlanter

	batchKey *keychain.KeyDescriptor

	proofFiles *tapgarden.MockProofArchive

	*testing.T

	errChan chan error
}

// newMintingTestHarness creates a new test harness from an active minting
// store and an existing testing context.
func newMintingTestHarness(t *testing.T, store tapgarden.MintingStore,
	interval time.Duration) *mintingTestHarness {

	keyRing := tapgarden.NewMockKeyRing()
	genSigner := tapgarden.NewMockGenSigner(keyRing)

	return &mintingTestHarness{
		T:         t,
		store:     store,
		ticker:    ticker.NewForce(interval),
		wallet:    tapgarden.NewMockWalletAnchor(),
		chain:     tapgarden.NewMockChainBridge(),
		keyRing:   keyRing,
		genSigner: genSigner,
		errChan:   make(chan error, 10),
	}
}

// refreshChainPlanter creates a new test harness.
func (t *mintingTestHarness) refreshChainPlanter() {
	// If the old planter exists, then we'll stop it now to simulate a
	// normal shutdown.
	if t.planter != nil {
		require.NoError(t, t.planter.Stop())
	}

	t.planter = tapgarden.NewChainPlanter(tapgarden.PlanterConfig{
		GardenKit: tapgarden.GardenKit{
			Wallet:      t.wallet,
			ChainBridge: t.chain,
			Log:         t.store,
			KeyRing:     t.keyRing,
			GenSigner:   t.genSigner,
			ProofFiles:  t.proofFiles,
		},
		BatchTicker: t.ticker,
		ErrChan:     t.errChan,
	})
	require.NoError(t, t.planter.Start())
}

// newRandSeedlings creates numSeedlings amount of seedlings with random
// initialized values.
func (t *mintingTestHarness) newRandSeedlings(numSeedlings int) []*tapgarden.Seedling {
	seedlings := make([]*tapgarden.Seedling, numSeedlings)
	for i := 0; i < numSeedlings; i++ {
		var n [32]byte
		if _, err := rand.Read(n[:]); err != nil {
			t.Fatalf("unable to read str: %v", err)
		}

		assetName := hex.EncodeToString(n[:])
		seedlings[i] = &tapgarden.Seedling{
			AssetType: asset.Type(rand.Int31n(2)),
			AssetName: assetName,
			Meta: &proof.MetaReveal{
				Data: n[:],
			},
			EnableEmission: test.RandBool(),
		}
		if seedlings[i].AssetType == asset.Normal {
			seedlings[i].Amount = uint64(rand.Int31())
		} else {
			seedlings[i].Amount = 1
		}
	}

	return seedlings
}

func (t *mintingTestHarness) assertKeyDerived() *keychain.KeyDescriptor {
	t.Helper()

	key, err := chanutils.RecvOrTimeout(t.keyRing.ReqKeys, defaultTimeout)
	require.NoError(t, err)

	return *key
}

// queueSeedlingsInBatch adds the series of seedlings to the batch, an error is
// raised if any of the seedlings aren't accepted.
func (t *mintingTestHarness) queueSeedlingsInBatch(
	seedlings ...*tapgarden.Seedling) {

	for i, seedling := range seedlings {
		seedling := seedling

		// Queue the new seedling for a batch.
		//
		// TODO(roasbeef): map of update chans?
		updates, err := t.planter.QueueNewSeedling(seedling)
		require.NoError(t, err)

		// For the first seedlings sent, we should get a new request
		if i == 0 {
			t.batchKey = t.assertKeyDerived()
		}

		// We should get an update from the update channel that the
		// seedling is now pending.
		update, err := chanutils.RecvOrTimeout(updates, defaultTimeout)
		require.NoError(t, err)

		// Make sure the seedling was planted without error.
		require.NoError(t, update.Error)

		// The received update should be a state of MintingStateSeed.
		require.Equal(t, tapgarden.MintingStateSeed, update.NewState)
	}
}

// assertPendingBatchExists asserts that a pending batch is found and it has
// numSeedlings assets registered.
func (t *mintingTestHarness) assertPendingBatchExists(numSeedlings int) {
	t.Helper()

	batch, err := t.planter.PendingBatch()
	require.NoError(t, err)
	require.NotNil(t, batch)
	require.Len(t, batch.Seedlings, numSeedlings)
}

// assertNoActiveBatch asserts that no pending batch exists.
func (t *mintingTestHarness) assertNoPendingBatch() {
	t.Helper()

	batch, err := t.planter.PendingBatch()
	require.NoError(t, err)
	require.Nil(t, batch)
}

// tickMintingBatch first the ticker that forces the planter to create a new
// batch.
func (t *mintingTestHarness) tickMintingBatch(noBatch bool) *btcec.PublicKey {
	t.Helper()

	batchKey, err := t.planter.FinalizeBatch()
	if noBatch {
		require.ErrorContains(t, err, "no pending batch")
		require.Nil(t, batchKey)
		return nil
	}

	require.NoError(t, err)
	require.NotNil(t, batchKey)
	return batchKey
}

func (t *mintingTestHarness) cancelMintingBatch(noBatch bool) *btcec.PublicKey {
	t.Helper()

	batchKey, err := t.planter.CancelBatch()
	if noBatch {
		require.ErrorContains(t, err, "no pending batch")
		require.Nil(t, batchKey)
		return nil
	}

	if batchKey == nil {
		require.NotNil(t, err)
		return nil
	}

	if err != nil {
		require.ErrorContains(t, err, "batch not cancellable")
		require.NotNil(t, batchKey)
		return batchKey
	}

	require.NoError(t, err)
	require.NotNil(t, batchKey)
	return batchKey
}

// assertNumCaretakersActive asserts that the specified number of caretakers
// are active.
func (t *mintingTestHarness) assertNumCaretakersActive(n int) {
	t.Helper()

	err := wait.Predicate(func() bool {
		numBatches, err := t.planter.NumActiveBatches()
		require.NoError(t, err)
		return numBatches == n
	}, defaultTimeout)
	require.NoError(t, err)
}

// assertGenesisTxFunded asserts that a caretaker attempted to fund a new
// genesis transaction.
func (t *mintingTestHarness) assertGenesisTxFunded() *tapgarden.FundedPsbt {
	// In order to fund a transaction, we expect a call to estimate the
	// fee, followed by a request to fund a new PSBT packet.
	_, err := chanutils.RecvOrTimeout(
		t.chain.FeeEstimateSignal, defaultTimeout,
	)
	require.NoError(t, err)

	pkt, err := chanutils.RecvOrTimeout(
		t.wallet.FundPsbtSignal, defaultTimeout,
	)
	require.NoError(t, err)

	// Finally, we'll assert that the dummy output or a valid P2TR output
	// is found in the packet.
	var found bool
	for _, txOut := range (*pkt).Pkt.UnsignedTx.TxOut {
		txOut := txOut

		if txOut.Value == int64(tapgarden.GenesisAmtSats) {
			isP2TR := txscript.IsPayToTaproot(txOut.PkScript)
			isDummyScript := bytes.Equal(
				txOut.PkScript, tapgarden.GenesisDummyScript[:],
			)

			if isP2TR || isDummyScript {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("unable to find dummy tx out in genesis tx: %v",
			spew.Sdump(pkt))
	}

	return *pkt
}

// assertSeedlingsExist asserts that all the seedlings are present in the batch.
func (t *mintingTestHarness) assertSeedlingsExist(
	seedlings []*tapgarden.Seedling, batchKey *btcec.PublicKey) {

	t.Helper()
	var pendingBatch *tapgarden.MintingBatch

	pendingBatches, err := t.store.FetchNonFinalBatches(context.Background())
	require.NoError(t, err)
	require.True(t, len(pendingBatches) >= 1)

	matchingBatch := func(batch *tapgarden.MintingBatch) bool {
		targetKey := asset.ToSerialized(batchKey)
		candidateKey := asset.ToSerialized(batch.BatchKey.PubKey)

		return bytes.Equal(targetKey[:], candidateKey[:])
	}

	if batchKey == nil {
		isNotCancelledBatch := func(batch *tapgarden.MintingBatch) bool {
			return !isCancelledBatch(batch)
		}
		pendingBatch, err = chanutils.First(
			pendingBatches, isNotCancelledBatch,
		)
		require.NoError(t, err)
	}

	if batchKey != nil {
		var err error
		pendingBatch, err = chanutils.First(pendingBatches, matchingBatch)
		require.NoError(t, err)
	}

	// The seedlings should match up properly.
	require.Len(t, pendingBatch.Seedlings, len(seedlings))

	for _, seedling := range seedlings {
		batchSeedling, ok := pendingBatch.Seedlings[seedling.AssetName]
		if !ok {
			t.Fatalf("seedling %v not found", seedling.AssetName)
		}

		require.Equal(t, seedling.AssetType, batchSeedling.AssetType)
		require.Equal(t, seedling.AssetName, batchSeedling.AssetName)
		require.Equal(t, seedling.Meta, batchSeedling.Meta)
		require.Equal(t, seedling.Amount, batchSeedling.Amount)
		require.Equal(
			t, seedling.EnableEmission, batchSeedling.EnableEmission,
		)
	}
}

func isCancelledBatch(batch *tapgarden.MintingBatch) bool {
	return batch.BatchState == tapgarden.BatchStateSeedlingCancelled ||
		batch.BatchState == tapgarden.BatchStateSproutCancelled
}

func (t *mintingTestHarness) assertBatchState(batchKey *btcec.PublicKey,
	batchState tapgarden.BatchState) {

	t.Helper()

	batches, err := t.planter.ListBatches(batchKey)
	require.NoError(t, err)
	require.Len(t, batches, 1)

	batch := batches[0]
	require.Equal(t, batchState, batch.BatchState)
}

// assertSeedlingsMatchSprouts asserts that the seedlings were properly matched
// into actual assets.
func (t *mintingTestHarness) assertSeedlingsMatchSprouts(
	seedlings []*tapgarden.Seedling) {

	t.Helper()

	// The caretaker is async, so we'll spin here until the batch read is
	// in the expected state.
	var pendingBatch *tapgarden.MintingBatch
	err := wait.Predicate(func() bool {
		pendingBatches, err := t.store.FetchNonFinalBatches(
			context.Background(),
		)
		require.NoError(t, err)

		// Filter out any cancelled batches.
		isCommittedBatch := func(batch *tapgarden.MintingBatch) bool {
			return batch.BatchState == tapgarden.BatchStateCommitted
		}
		batch, err := chanutils.First(pendingBatches, isCommittedBatch)
		if err != nil {
			return false
		}

		pendingBatch = batch
		return true
	}, defaultTimeout)
	require.NoError(
		t, err, fmt.Errorf("unable to read pending batch: %v", err),
	)

	// The amount of assets committed to in the Taproot Asset commitment
	// should match up
	dbAssets := pendingBatch.RootAssetCommitment.CommittedAssets()
	require.Len(t, dbAssets, len(seedlings))

	assetsByTag := make(map[string]*asset.Asset)
	for _, asset := range dbAssets {
		assetsByTag[asset.Genesis.Tag] = asset
	}

	for _, seedling := range seedlings {
		assetSprout, ok := assetsByTag[seedling.AssetName]
		if !ok {
			t.Fatalf("asset for seedling %v not found",
				seedling.AssetName)
		}

		// We expect the seedling to have been properly mapped onto an
		// asset.
		require.Equal(t, seedling.AssetType, assetSprout.Type)
		require.Equal(t, seedling.AssetName, assetSprout.Genesis.Tag)
		require.Equal(
			t, seedling.Meta.MetaHash(), assetSprout.Genesis.MetaHash,
		)
		require.Equal(t, seedling.Amount, assetSprout.Amount)
		require.Equal(
			t, seedling.EnableEmission, assetSprout.GroupKey != nil,
		)
	}
}

// assertGenesisPsbtFinalized asserts that a request to finalize the genesis
// transaction has been requested by a caretaker.
func (t *mintingTestHarness) assertGenesisPsbtFinalized() {
	t.Helper()

	// Ensure that a request to finalize the PSBt has come across.
	_, err := chanutils.RecvOrTimeout(
		t.wallet.SignPsbtSignal, defaultTimeout,
	)
	require.NoError(t, err, "psbt sign req not sent")

	// TODO(jhb): fix for multibatch?
	// Next fetch the minting key of the batch.
	pendingBatches, err := t.store.FetchNonFinalBatches(
		context.Background(),
	)
	require.NoError(t, err)

	isNotCancelledBatch := func(batch *tapgarden.MintingBatch) bool {
		return !isCancelledBatch(batch)
	}
	pendingBatch, err := chanutils.First(pendingBatches, isNotCancelledBatch)
	require.NoError(t, err)

	// The minting key of the batch should match the public key
	// that was inserted into the wallet.
	batchKey, _, err := pendingBatch.MintingOutputKey()
	require.NoError(t, err)

	importedKey, err := chanutils.RecvOrTimeout(
		t.wallet.ImportPubKeySignal, defaultTimeout,
	)
	require.NoError(t, err, "pubkey import req not sent")
	require.True(t, (*importedKey).IsEqual(batchKey))
}

// assertTxPublished asserts that a transaction was published via the active
// chain bridge.
func (t *mintingTestHarness) assertTxPublished() *wire.MsgTx {
	t.Helper()

	tx, err := chanutils.RecvOrTimeout(t.chain.PublishReq, defaultTimeout)
	require.NoError(t, err)

	return *tx
}

// assertConfReqSent asserts that a confirmation request has been sent. If so,
// then a closure is returned that once called will send a confirmation
// notification.
func (t *mintingTestHarness) assertConfReqSent(tx *wire.MsgTx,
	block *wire.MsgBlock) func() {

	reqNo, err := chanutils.RecvOrTimeout(
		t.chain.ConfReqSignal, defaultTimeout,
	)
	require.NoError(t, err)

	return func() {
		t.chain.SendConfNtfn(*reqNo, &chainhash.Hash{}, 1, 0, block, tx)
	}
}

// assertNoError makes sure no error was sent on the global error channel.
func (t *mintingTestHarness) assertNoError() {
	select {
	case err := <-t.errChan:
		require.NoError(t, err)
	default:
	}
}

// testBasicAssetCreation tests that we're able to properly progress the state
// machine through the various stages of asset minting and creation.
//
// TODO(roasbeef): use wrapper/interceptor on the storage impl to have better
// assertions?
func testBasicAssetCreation(t *mintingTestHarness) {
	// First, create a new chain planter instance using the supplied test
	// harness.
	t.refreshChainPlanter()

	// Next make 5 new random seedlings, and queue each of them up within
	// the main state machine for batched minting.
	const numSeedlings = 5
	seedlings := t.newRandSeedlings(numSeedlings)
	t.queueSeedlingsInBatch(seedlings...)

	// At this point, there should be a single pending batch with 5
	// seedlings. The batch stored in the log should also match up exactly.
	t.assertPendingBatchExists(numSeedlings)
	t.assertSeedlingsExist(seedlings, nil)

	// Now we'll force a batch tick which should kick off a new caretaker
	// that starts to progress the batch all the way to broadcast.
	t.tickMintingBatch(false)

	// We'll now restart the planter to ensure that it's able to properly
	// resume all the caretakers. We need to sleep for a small amount to
	// allow the planter to get the batch tick signal.
	time.Sleep(time.Millisecond * 100)
	t.refreshChainPlanter()

	// Now that the planter is back up, a single caretaker should have been
	// launched as well. Next, assert that the caretaker has requested a
	// genesis tx to be funded.
	_ = t.assertGenesisTxFunded()
	t.assertNumCaretakersActive(1)

	// We'll now force yet another restart to ensure correctness of the
	// state machine, we expect the PSBT packet to be funded again as well,
	// since we didn't get a chance to write it to disk.
	t.refreshChainPlanter()
	_ = t.assertGenesisTxFunded()

	// For each seedling created above, we expect a new set of keys to be
	// created for the asset script key and an additional key if emission
	// was enabled.
	for i := 0; i < numSeedlings; i++ {
		t.assertKeyDerived()

		if seedlings[i].EnableEmission {
			t.assertKeyDerived()
		}
	}

	// Now that the batch has been ticked, and the caretaker started, there
	// should no longer be a pending batch.
	t.assertNoPendingBatch()

	// If we fetch the pending batch just created on disk, then it should
	// match up with the seedlings we specified, and also the genesis
	// transaction sent above.
	t.assertSeedlingsMatchSprouts(seedlings)

	// Before we proceed to the next phase, we'll restart the planter again
	// to ensure it can proceed with some bumps along the way.
	t.refreshChainPlanter()

	// We should now transition to the next state where we'll attempt to
	// sign this PSBT packet generated above.
	t.assertGenesisPsbtFinalized()

	// With the PSBT packet finalized for the caretaker, we should now
	// receive a request to publish a transaction followed by a
	// confirmation request.
	tx := t.assertTxPublished()

	// We'll now restart the daemon once again to simulate some downtime
	// after the transaction has been published.
	t.refreshChainPlanter()

	// Make sure any errors sent on the error channel from shutting down are
	// drained, so we don't see them later.
	select {
	case <-t.errChan:
	default:
	}

	// After the restart, the transaction should be published again.
	t.assertTxPublished()

	// With the transaction published, we should now receive a confirmation
	// request. To ensure the file proof is constructed properly, we'll
	// also make a "fake" block that includes our transaction.
	merkleTree := blockchain.BuildMerkleTreeStore(
		[]*btcutil.Tx{btcutil.NewTx(tx)}, false,
	)
	merkleRoot := merkleTree[len(merkleTree)-1]
	blockHeader := wire.NewBlockHeader(
		0, chaincfg.MainNetParams.GenesisHash, merkleRoot, 0, 0,
	)
	block := &wire.MsgBlock{
		Header:       *blockHeader,
		Transactions: []*wire.MsgTx{tx},
	}
	sendConfNtfn := t.assertConfReqSent(tx, block)

	// We'll now send the confirmation notification which should result in
	// the batch being finalized, and the caretaker being cleaned up.
	sendConfNtfn()

	// This time no error should be sent anywhere as we should've handled
	// all notifications.
	t.assertNoError()

	// At this point there should be no active caretakers.
	t.assertNumCaretakersActive(0)
}

func testMintingTicker(t *mintingTestHarness) {
	// First, create a new chain planter instance using the supplied test
	// harness.
	t.refreshChainPlanter()

	// Next make 5 new random seedlings, and queue each of them up within
	// the main state machine for batched minting.
	const numSeedlings = 5
	seedlings := t.newRandSeedlings(numSeedlings)
	t.queueSeedlingsInBatch(seedlings...)

	// At this point, there should be a single pending batch with 5
	// seedlings. The batch stored in the log should also match up exactly.
	t.assertPendingBatchExists(numSeedlings)
	t.assertSeedlingsExist(seedlings, nil)

	// If we cancel the current batch, the pending batch should be cleared,
	// but the seedlings should still exist on disk. Requesting batch
	// finalization should not change anything in the planter.
	t.cancelMintingBatch(false)
	t.assertNoPendingBatch()

	// Next, make another 5 seedlings and continue with minting.
	// One seedling is a duplicate of a seedling from the cancelled batch,
	// to ensure that we can store multiple versions of the same seedling.
	seedlings = t.newRandSeedlings(numSeedlings)
	t.queueSeedlingsInBatch(seedlings...)

	// Next, finalize the pending batch to continue with minting.
	t.tickMintingBatch(false)

	// A single caretaker should have been launched as well. Next, assert
	// that the caretaker has requested a genesis tx to be funded.
	_ = t.assertGenesisTxFunded()
	t.assertNumCaretakersActive(1)

	// For each seedling created above, we expect a new set of keys to be
	// created for the asset script key and an additional key if emission
	// was enabled.
	for i := 0; i < numSeedlings; i++ {
		t.assertKeyDerived()

		if seedlings[i].EnableEmission {
			t.assertKeyDerived()
		}
	}

	// Now that the batch has been ticked, and the caretaker started, there
	// should no longer be a pending batch.
	t.assertNoPendingBatch()

	// If we fetch the pending batch just created on disk, then it should
	// match up with the seedlings we specified, and also the genesis
	// transaction sent above.
	t.assertSeedlingsMatchSprouts(seedlings)

	// We should now transition to the next state where we'll attempt to
	// sign this PSBT packet generated above.
	t.assertGenesisPsbtFinalized()

	// With the PSBT packet finalized for the caretaker, we should now
	// receive a request to publish a transaction followed by a
	// confirmation request.
	tx := t.assertTxPublished()

	// With the transaction published, we should now receive a confirmation
	// request. To ensure the file proof is constructed properly, we'll
	// also make a "fake" block that includes our transaction.
	merkleTree := blockchain.BuildMerkleTreeStore(
		[]*btcutil.Tx{btcutil.NewTx(tx)}, false,
	)
	merkleRoot := merkleTree[len(merkleTree)-1]
	blockHeader := wire.NewBlockHeader(
		0, chaincfg.MainNetParams.GenesisHash, merkleRoot, 0, 0,
	)
	block := &wire.MsgBlock{
		Header:       *blockHeader,
		Transactions: []*wire.MsgTx{tx},
	}
	sendConfNtfn := t.assertConfReqSent(tx, block)

	// We'll now send the confirmation notification which should result in
	// the batch being finalized, and the caretaker being cleaned up.
	sendConfNtfn()

	// This time no error should be sent anywhere as we should've handled
	// all notifications.
	t.assertNoError()

	// At this point there should be no active caretakers.
	t.assertNumCaretakersActive(0)
}

// testBatchCancelFinalize tests that batches can be cancelled and finalized,
// and that the expected errors are returned when there is no pending batch
// or cancellation is not possible.
func testMintingCancelFinalize(t *mintingTestHarness) {
	// First, create a new chain planter instance using the supplied test
	// harness.
	t.refreshChainPlanter()

	// Next make 5 new random seedlings, and queue each of them up within
	// the main state machine for batched minting.
	const numSeedlings = 5
	seedlings := t.newRandSeedlings(numSeedlings)
	t.queueSeedlingsInBatch(seedlings...)
	firstSeedling := seedlings[0]

	// At this point, there should be a single pending batch with 5
	// seedlings. The batch stored in the log should also match up exactly.
	t.assertPendingBatchExists(numSeedlings)
	t.assertSeedlingsExist(seedlings, nil)

	// If we cancel the current batch, the pending batch should be cleared,
	// but the seedlings should still exist on disk.
	firstBatchKey := t.cancelMintingBatch(false)
	t.assertNoPendingBatch()
	t.assertBatchState(firstBatchKey, tapgarden.BatchStateSeedlingCancelled)
	t.assertSeedlingsExist(seedlings, firstBatchKey)

	// Requesting batch finalization or cancellation with no pending batch
	// should return an error without crashing the planter.
	t.tickMintingBatch(true)
	t.cancelMintingBatch(true)

	// Next, make another 5 random seedlings and continue with minting.
	seedlings = t.newRandSeedlings(numSeedlings)
	seedlings[0] = firstSeedling
	t.queueSeedlingsInBatch(seedlings...)

	t.assertPendingBatchExists(numSeedlings)
	t.assertSeedlingsExist(seedlings, nil)

	// If we attempt to queue a seedling with the same name as a pending
	// seedling, the planter should reject it.
	updates, err := t.planter.QueueNewSeedling(firstSeedling)
	require.NoError(t, err)
	planterErr := <-updates
	require.NotNil(t, planterErr.Error)
	require.ErrorContains(t, planterErr.Error, "already in batch")

	// Now, finalize the pending batch to continue with minting.
	t.tickMintingBatch(false)

	// A single caretaker should have been launched as well. Next, assert
	// that the caretaker has requested a genesis tx to be funded.
	_ = t.assertGenesisTxFunded()
	t.assertNumCaretakersActive(1)

	// For each seedling created above, we expect a new set of keys to be
	// created for the asset script key and an additional key if emission
	// was enabled.
	for i := 0; i < numSeedlings; i++ {
		t.assertKeyDerived()

		if seedlings[i].EnableEmission {
			t.assertKeyDerived()
		}
	}

	// We should be able to cancel the batch even after it has a caretaker,
	// and at this point the minting transaction is still being made.
	secondBatchKey := t.cancelMintingBatch(false)
	t.assertNoPendingBatch()
	t.assertBatchState(secondBatchKey, tapgarden.BatchStateSproutCancelled)

	// We can make another 5 random seedlings and continue with minting.
	seedlings = t.newRandSeedlings(numSeedlings)
	t.queueSeedlingsInBatch(seedlings...)

	t.assertPendingBatchExists(numSeedlings)
	t.assertSeedlingsExist(seedlings, nil)

	// Now, finalize the pending batch to continue with minting.
	thirdBatchKey := t.tickMintingBatch(false)
	require.NotNil(t, thirdBatchKey)

	_ = t.assertGenesisTxFunded()
	t.assertNumCaretakersActive(1)

	for i := 0; i < numSeedlings; i++ {
		t.assertKeyDerived()

		if seedlings[i].EnableEmission {
			t.assertKeyDerived()
		}
	}

	// Now that the batch has been ticked, and the caretaker started, there
	// should no longer be a pending batch.
	t.assertNoPendingBatch()

	// If we fetch the pending batch just created on disk, then it should
	// match up with the seedlings we specified, and also the genesis
	// transaction sent above.
	t.assertSeedlingsMatchSprouts(seedlings)

	// We should now transition to the next state where we'll attempt to
	// sign this PSBT packet generated above.
	t.assertGenesisPsbtFinalized()

	// With the PSBT packet finalized for the caretaker, we should now
	// receive a request to publish a transaction followed by a
	// confirmation request.
	tx := t.assertTxPublished()

	// With the transaction published, we should now receive a confirmation
	// request. To ensure the file proof is constructed properly, we'll
	// also make a "fake" block that includes our transaction.
	merkleTree := blockchain.BuildMerkleTreeStore(
		[]*btcutil.Tx{btcutil.NewTx(tx)}, false,
	)
	merkleRoot := merkleTree[len(merkleTree)-1]
	blockHeader := wire.NewBlockHeader(
		0, chaincfg.MainNetParams.GenesisHash, merkleRoot, 0, 0,
	)
	block := &wire.MsgBlock{
		Header:       *blockHeader,
		Transactions: []*wire.MsgTx{tx},
	}
	sendConfNtfn := t.assertConfReqSent(tx, block)

	// Once the minting transaction has been published, trying to cancel the
	// batch should fail with an error from the caretaker.
	t.assertBatchState(thirdBatchKey, tapgarden.BatchStateBroadcast)
	cancelResp := t.cancelMintingBatch(false)

	batchKeyEquality := func(a, b *btcec.PublicKey) bool {
		aBytes := asset.ToSerialized(a)
		bBytes := asset.ToSerialized(b)
		return bytes.Equal(aBytes[:], bBytes[:])
	}

	// Verify that the batch we attempted to cancel is the batch that was
	// just broadcast.
	require.True(t, batchKeyEquality(thirdBatchKey, cancelResp))

	// We'll now send the confirmation notification which should result in
	// the batch being finalized, and the caretaker being cleaned up.
	sendConfNtfn()

	// This time no error should be sent anywhere as we should've handled
	// all notifications.
	t.assertNoError()

	// At this point there should be no active caretakers.
	t.assertNumCaretakersActive(0)
}

// mintingStoreTestCase is used to programmatically run a series of test cases
// that are parametrized based on a fresh minting store.
type mintingStoreTestCase struct {
	name     string
	interval time.Duration
	testFunc func(t *mintingTestHarness)
}

// testCases houses the set of minting store test cases.
var testCases = []mintingStoreTestCase{
	{
		name:     "basic_asset_creation",
		interval: defaultInterval,
		testFunc: testBasicAssetCreation,
	},
	{
		name:     "creation_by_minting_ticker",
		interval: minterInterval,
		testFunc: testMintingTicker,
	},
	{
		name:     "minting_with_cancellation",
		interval: minterInterval,
		testFunc: testMintingCancelFinalize,
	},
}

// TestBatchedAssetIssuance runs a test of tests to ensure that the set of
// registered minting stores can be used to properly implement batched asset
// minting.
func TestBatchedAssetIssuance(t *testing.T) {
	t.Helper()

	for _, testCase := range testCases {
		mintingStore := newMintingStore(t)
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			mintTest := newMintingTestHarness(
				t, mintingStore, testCase.interval,
			)
			testCase.testFunc(mintTest)
		})
	}
}

func init() {
	rand.Seed(time.Now().Unix())

	logWriter := build.NewRotatingLogWriter()
	logger := logWriter.GenSubLogger(tapgarden.Subsystem, func() {})
	logWriter.RegisterSubLogger(tapgarden.Subsystem, logger)
	tapgarden.UseLogger(logger)
}
