package tapfreighter

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/chanutils"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/mssmt"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapgarden"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightningnetwork/lnd/keychain"
)

// CommitmentConstraints conveys the constraints on the type of Taproot asset
// commitments needed to satisfy a send request. Typically, for Bitcoin we just
// care about the amount. In the case of Taproot Asset, we also need to worry
// about the asset ID, and also the type of asset we need.
//
// NOTE: Only the GroupKey or the AssetID should be set.
type CommitmentConstraints struct {
	// GroupKey is the required group key. This is an optional field, if
	// set then the asset returned may have a distinct asset ID to the one
	// specified below.
	GroupKey *btcec.PublicKey

	// AssetID is the asset ID that needs to be satisfied.
	AssetID *asset.ID

	// MinAmt is the minimum amount that an asset commitment needs to hold
	// to satisfy the constraints.
	MinAmt uint64
}

// AnchoredCommitment is the response to satisfying the set of
// CommitmentConstraints. This includes the asset itself, and also information
// needed to locate the asset on-chain and also prove its existence.
type AnchoredCommitment struct {
	// AnchorPoint is the outpoint that the Commitment below is anchored on
	// in the main chain.
	AnchorPoint wire.OutPoint

	// AnchorOutputValue is output value of the anchor output.
	AnchorOutputValue btcutil.Amount

	// InternalKey is the internal key that's used to anchor the commitment
	// in the above out point.
	InternalKey keychain.KeyDescriptor

	// TapscriptSibling is the tapscript sibling preimage of this asset.
	// This will usually be nil.
	TapscriptSibling *commitment.TapscriptPreimage

	// Commitment is the full Taproot Asset commitment anchored at the above
	// outpoint. This includes both the asset to be used as an input, along
	// with any other assets that might be collocated in this commitment.
	Commitment *commitment.TapCommitment

	// Asset is the asset that ratifies the above constraints, and should
	// be used as an input to a transaction.
	Asset *asset.Asset
}

var (
	// ErrMatchingAssetsNotFound is returned when an instance of
	// AssetStoreListCoins cannot satisfy the given asset identification
	// constraints.
	ErrMatchingAssetsNotFound = fmt.Errorf("failed to find coin(s) which" +
		"satisfy given constraints")
)

// CoinLister attracts over the coin selection process needed to be
// able to execute moving taproot assets on chain.
type CoinLister interface {
	// ListEligibleCoins takes the set of commitment constraints and returns
	// an AnchoredCommitment that returns all the information needed to use
	// the commitment as an input to an on chain Taproot Asset transaction.
	//
	// If coin selection cannot be completed, then ErrMatchingAssetsNotFound
	// should be returned.
	ListEligibleCoins(context.Context,
		CommitmentConstraints) ([]*AnchoredCommitment, error)
}

// MultiCommitmentSelectStrategy is an enum that describes the strategy that
// should be used when preferentially selecting multiple commitments.
type MultiCommitmentSelectStrategy uint8

const (
	// PreferMaxAmount is a strategy which considers commitments in order of
	// descending amounts and selects the first subset which cumulatively
	// sums to at least the minimum target amount.
	PreferMaxAmount MultiCommitmentSelectStrategy = iota
)

// CoinSelector is an interface that describes the functionality used in
// selecting coins during the asset send process.
type CoinSelector interface {
	CoinLister

	// SelectForAmount takes a set of commitments and a strategy, and
	// returns a subset of the commitments that satisfy the strategy and the
	// minimum total amount.
	SelectForAmount(minTotalAmount uint64,
		eligibleCommitments []*AnchoredCommitment,
		strategy MultiCommitmentSelectStrategy) ([]*AnchoredCommitment,
		error)
}

// TransferInput represents the database level input to an asset transfer.
type TransferInput struct {
	// PrevID contains the anchor point, ID and script key of the asset that
	// is being spent.
	asset.PrevID

	// Amount is the input amount that was spent.
	Amount uint64
}

// Anchor represents the database level representation of an anchor output.
type Anchor struct {
	// OutPoint is the chain location of the anchor output.
	OutPoint wire.OutPoint

	// Value is output value of the anchor output.
	Value btcutil.Amount

	// InternalKey is the new internal key that commits to the set of assets
	// anchored at the new outpoint.
	InternalKey keychain.KeyDescriptor

	// TaprootAssetRoot is the Taproot Asset commitment root hash of the
	// anchor output.
	TaprootAssetRoot []byte

	// MerkleRoot is the root of the tap script merkle tree that also
	// contains the Taproot Asset commitment of the anchor output. If there
	// is no tapscript sibling, then this is equal to the TaprootAssetRoot.
	MerkleRoot []byte

	// TapscriptSibling is the serialized preimage of the tapscript sibling
	// of the Taproot Asset commitment.
	TapscriptSibling []byte

	// NumPassiveAssets is the number of passive assets in the commitment
	// for this anchor output.
	NumPassiveAssets uint32
}

// TransferOutput represents the database level output to an asset transfer.
type TransferOutput struct {
	// Anchor is the new location of the Taproot Asset commitment referenced
	// by this transfer output.
	Anchor Anchor

	// Type indicates what type of output this is, which has an influence on
	// whether the asset is set or what witness type is expected to be
	// generated for the asset.
	Type tappsbt.VOutputType

	// ScriptKey is the new script key.
	ScriptKey asset.ScriptKey

	// ScriptKeyLocal indicates whether the script key is known to the lnd
	// node connected to this daemon. If this is false, then we won't create
	// a new asset entry in our database as we consider this to be an
	// outbound transfer.
	ScriptKeyLocal bool

	// Amount is the new amount for the asset.
	Amount uint64

	// WitnessData is the new witness data for this asset.
	WitnessData []asset.Witness

	// SplitCommitmentRoot is the root split commitment for this asset.
	// This will only be set if a split was required to complete the send.
	SplitCommitmentRoot mssmt.Node

	// ProofSuffix is the fully serialized proof suffix of the output which
	// includes all the proof information other than the final chain
	// information.
	ProofSuffix []byte
}

// OutboundParcel represents the database level delta of an outbound Taproot
// Asset parcel (outbound spend). A spend will destroy a series of assets listed
// as inputs, and re-create them as new outputs. Along the way some assets may
// have been split or sent to others. This is reflected in the set of
// TransferOutputs.
type OutboundParcel struct {
	// AnchorTx is the new transaction that commits to the set of Taproot
	// Assets found at the above NewAnchorPoint.
	AnchorTx *wire.MsgTx

	// AnchorTxHeightHint is a block height recorded before the anchor tx is
	// broadcast, used as a starting block height when registering for
	// confirmations.
	AnchorTxHeightHint uint32

	// TransferTime holds the timestamp of the outbound spend.
	TransferTime time.Time

	// ChainFees is the amount in sats paid in on-chain fees for the
	// anchor transaction.
	ChainFees int64

	// PassiveAssets is the set of passive assets that are re-anchored
	// during the parcel confirmation process.
	PassiveAssets []*PassiveAssetReAnchor

	// Inputs represents the list of previous assets that were spent with
	// this transfer.
	Inputs []TransferInput

	// Outputs represents the list of new assets that were created with this
	// transfer.
	Outputs []TransferOutput
}

// AssetConfirmEvent is used to mark a batched spend as confirmed on disk.
type AssetConfirmEvent struct {
	// AnchorTXID is the anchor transaction's hash that was previously
	// unconfirmed.
	AnchorTXID chainhash.Hash

	// BlockHash is the block hash that confirmed the above anchor point.
	BlockHash chainhash.Hash

	// BlockHeight is the height of the block hash above.
	BlockHeight int32

	// TxIndex is the location within the block that confirmed the anchor
	// point.
	TxIndex int32

	// FinalProofs is the set of final full proof chain files that are going
	// to be stored on disk, one for each output in the outbound parcel.
	FinalProofs map[asset.SerializedKey]*proof.AnnotatedProof

	// PassiveAssetProofFiles is the set of passive asset proof files that
	// are re-anchored during the parcel confirmation process.
	PassiveAssetProofFiles map[[32]byte]proof.Blob
}

// PassiveAssetReAnchor includes the information needed to re-anchor a passive
// asset during asset send delivery confirmation.
type PassiveAssetReAnchor struct {
	// VPacket is a virtual packet which describes the virtual transaction
	// which is used in re-anchoring the passive asset.
	VPacket *tappsbt.VPacket

	// GenesisID is the genesis ID of the passive asset.
	GenesisID asset.ID

	// PrevAnchorPoint is the previous anchor point of the passive asset
	// before re-anchoring. This field is used to identify the correct asset
	// to update.
	PrevAnchorPoint wire.OutPoint

	// ScriptKey is the previous script key of the passive asset before
	// re-anchoring. This field is used to identify the correct asset to
	// update.
	ScriptKey asset.ScriptKey

	// NewProof is the proof set of the re-anchored passive asset.
	NewProof *proof.Proof

	// NewWitnessData is the new witness set for this asset.
	NewWitnessData []asset.Witness
}

// ExportLog is used to track the state of outbound Taproot Asset parcels
// (batched spends). This log is used by the ChainPorter to mark pending
// outbound deliveries, and finally confirm the deliveries once they've been
// committed to the main chain.
type ExportLog interface {
	// LogPendingParcel marks an outbound parcel as pending on disk. This
	// commits the set of changes to disk (the asset deltas) but doesn't
	// mark the batched spend as being finalized.
	LogPendingParcel(context.Context, *OutboundParcel) error

	// PendingParcels returns the set of parcels that haven't yet been
	// finalized. This can be used to query the set of unconfirmed
	// transactions for re-broadcast.
	PendingParcels(context.Context) ([]*OutboundParcel, error)

	// ConfirmParcelDelivery marks a spend event on disk as confirmed. This
	// updates the on-chain reference information on disk to point to this
	// new spend.
	ConfirmParcelDelivery(context.Context, *AssetConfirmEvent) error
}

// ChainBridge aliases into the ChainBridge of the tapgarden package.
type ChainBridge = tapgarden.ChainBridge

// WalletAnchor aliases into the WalletAnchor of the taparden package.
type WalletAnchor interface {
	tapgarden.WalletAnchor

	// SignPsbt signs all the inputs it can in the passed-in PSBT packet,
	// returning a new one with updated signature/witness data.
	SignPsbt(ctx context.Context, packet *psbt.Packet) (*psbt.Packet, error)
}

// KeyRing aliases into the KeyRing of the tapgarden package.
type KeyRing = tapgarden.KeyRing

// Signer aliases into the Signer interface of the tapscript package.
type Signer = tapscript.Signer

// Porter is a high level interface that wraps the main caller execution point
// to the ChainPorter.
type Porter interface {
	// RequestShipment attempts to request that a new send be funneled
	// through the chain porter. If successful, an initial response will be
	// returned with the pending transfer information.
	RequestShipment(req Parcel) (*OutboundParcel, error)

	// Start signals that the asset minter should being operations.
	Start() error

	// Stop signals that the asset minter should attempt a graceful
	// shutdown.
	Stop() error

	// EventPublisher is a subscription interface that allows callers to
	// subscribe to events that are relevant to the Porter.
	chanutils.EventPublisher[chanutils.Event, bool]
}
