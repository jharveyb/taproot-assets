package proof

import (
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taro/asset"
	"github.com/lightninglabs/taro/commitment"
	"github.com/lightninglabs/taro/taroscript"
	"github.com/stretchr/testify/require"
)

func TestAppendTransition(t *testing.T) {
	t.Parallel()

	// Start with a minted genesis asset.
	genesisProof, genesisPrivKey := genRandomGenesisWithProof(t)
	genesisBlob, err := encodeAsProofFile(&genesisProof)
	require.NoError(t, err)

	// Transfer the asset to a new owner.
	transferPrivKey := randPrivKey(t)
	transferScriptKey := txscript.ComputeTaprootKeyNoScript(
		transferPrivKey.PubKey(),
	)

	genesisOutpoint := wire.OutPoint{
		Hash:  genesisProof.AnchorTx.TxHash(),
		Index: genesisProof.InclusionProof.OutputIndex,
	}
	prevID := &asset.PrevID{
		OutPoint: genesisOutpoint,
		ID:       genesisProof.Asset.ID(),
		ScriptKey: asset.ToSerialized(
			genesisProof.Asset.ScriptKey.PubKey,
		),
	}
	newAsset := *genesisProof.Asset.Copy()
	newAsset.ScriptKey = pubToKeyDesc(schnorrKey(t, transferScriptKey))
	newAsset.PrevWitnesses = []asset.Witness{{
		PrevID: prevID,
	}}
	inputs := commitment.InputSet{
		*prevID: &genesisProof.Asset,
	}

	virtualTx, _, err := taroscript.VirtualTx(&newAsset, inputs)
	require.NoError(t, err)
	newWitness, err := taroscript.SignTaprootKeySpend(
		*genesisPrivKey, virtualTx, &genesisProof.Asset, 0,
	)
	require.NoError(t, err)
	newAsset.PrevWitnesses[0].TxWitness = *newWitness

	internalKey := schnorrPubKey(t, transferPrivKey)

	assetCommitment, err := commitment.NewAssetCommitment(&newAsset)
	require.NoError(t, err)
	taroCommitment, err := commitment.NewTaroCommitment(assetCommitment)
	require.NoError(t, err)

	tapscriptRoot := taroCommitment.TapscriptRoot(nil)
	taprootKey := txscript.ComputeTaprootOutputKey(
		internalKey, tapscriptRoot[:],
	)
	taprootScript := computeTaprootScript(t, taprootKey)

	chainTx := &wire.MsgTx{
		Version: 2,
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: wire.OutPoint{
				Hash:  genesisProof.AnchorTx.TxHash(),
				Index: 0,
			},
		}},
		TxOut: []*wire.TxOut{{
			PkScript: taprootScript,
			Value:    330,
		}},
		LockTime: 0,
	}
	merkleTree := blockchain.BuildMerkleTreeStore(
		[]*btcutil.Tx{btcutil.NewTx(chainTx)}, false,
	)
	merkleRoot := merkleTree[len(merkleTree)-1]
	genesisHash := genesisProof.BlockHeader.BlockHash()
	blockHeader := wire.NewBlockHeader(0, &genesisHash, merkleRoot, 0, 0)

	txMerkleProof, err := NewTxMerkleProof([]*wire.MsgTx{chainTx}, 0)
	require.NoError(t, err)

	transitionParams := &TransitionParams{
		BaseProofParams: BaseProofParams{
			Block: &wire.MsgBlock{
				Header:       *blockHeader,
				Transactions: []*wire.MsgTx{chainTx},
			},
			Tx:          chainTx,
			TxIndex:     0,
			OutputIndex: 0,
			InternalKey: internalKey,
			TaroRoot:    taroCommitment,
		},
		NewAsset: &newAsset,
	}

	// Append the new transition to the genesis blob.
	transitionBlob, transitionProof, err := AppendTransition(
		genesisBlob, transitionParams,
	)
	require.NoError(t, err)
	require.Greater(t, len(transitionBlob), len(genesisBlob))
	require.Equal(t, txMerkleProof, &transitionProof.TxMerkleProof)
}
