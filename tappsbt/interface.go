package tappsbt

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/chanutils"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightningnetwork/lnd/keychain"
)

// We define a set of Taproot Asset (TAP) specific global, input and output PSBT
// key types here that correspond to the custom types defined in the VPacket
// below. We start at 0x70 because that is sufficiently high to not conflict
// with any of the keys specified in BIP-0174. Also, 7 is leet speak for "t" as
// in Taproot Assets. It would perhaps make sense to wrap these values in the
// BIP-0174 defined proprietary types to make 100% sure that no parser removes
// them. But the BIP also mentions to not remove unknown keys, so we should be
// fine like this as well.
var (
	PsbtKeyTypeGlobalTapIsVirtualTx    = []byte{0x70}
	PsbtKeyTypeGlobalTapChainParamsHRP = []byte{0x71}
	PsbtKeyTypeGlobalTapPsbtVersion    = []byte{0x72}

	PsbtKeyTypeInputTapPrevID                             = []byte{0x70}
	PsbtKeyTypeInputTapAnchorValue                        = []byte{0x71}
	PsbtKeyTypeInputTapAnchorPkScript                     = []byte{0x72}
	PsbtKeyTypeInputTapAnchorSigHashType                  = []byte{0x73}
	PsbtKeyTypeInputTapAnchorInternalKey                  = []byte{0x74}
	PsbtKeyTypeInputTapAnchorMerkleRoot                   = []byte{0x75}
	PsbtKeyTypeInputTapAnchorOutputBip32Derivation        = []byte{0x76}
	PsbtKeyTypeInputTapAnchorOutputTaprootBip32Derivation = []byte{0x77}
	PsbtKeyTypeInputTapAnchorTapscriptSibling             = []byte{0x78}
	PsbtKeyTypeInputTapAsset                              = []byte{0x79}
	PsbtKeyTypeInputTapAssetProof                         = []byte{0x7a}

	PsbtKeyTypeOutputTapType                               = []byte{0x70}
	PsbtKeyTypeOutputTapIsInteractive                      = []byte{0x71}
	PsbtKeyTypeOutputTapAnchorOutputIndex                  = []byte{0x72}
	PsbtKeyTypeOutputTapAnchorOutputInternalKey            = []byte{0x73}
	PsbtKeyTypeOutputTapAnchorOutputBip32Derivation        = []byte{0x74}
	PsbtKeyTypeOutputTapAnchorOutputTaprootBip32Derivation = []byte{0x75}
	PsbtKeyTypeOutputTapAsset                              = []byte{0x76}
	PsbtKeyTypeOutputTapSplitAsset                         = []byte{0x77}
	PsbtKeyTypeOutputTapAnchorTapscriptSibling             = []byte{0x78}
)

// The following keys are used as custom fields on the BTC level anchor
// transaction PSBTs only. They are defined here for completeness' sake but are
// not directly used by the tappsbt package.
var (
	PsbtKeyTypeInputTapProof = []byte{0x70}

	PsbtKeyTypeOutputTapProof = []byte{0x70}
)

// VOutPredicate is a function that can be used to filter virtual outputs.
type VOutPredicate func(*VOutput) bool

// bip32DerivationPredicate is a function that can be used to filter BIP-0032
// derivation paths.
type bip32DerivationPredicate func(*psbt.Bip32Derivation) bool

// bip32DerivationPredicate is a function that can be used to filter Taproot
// BIP-0032 derivation paths.
type taprootBip32DerivationPredicate func(*psbt.TaprootBip32Derivation) bool

var (
	// VOutIsSplitRoot is a predicate that returns true if the virtual
	// output is a split root output.
	VOutIsSplitRoot = func(o *VOutput) bool {
		return o.Type.IsSplitRoot()
	}

	// VOutIsNotSplitRoot is a predicate that returns true if the virtual
	// output is NOT a split root output.
	VOutIsNotSplitRoot = func(o *VOutput) bool {
		return !o.Type.IsSplitRoot()
	}

	// VOutIsInteractive is a predicate that returns true if the virtual
	// transaction is interactive.
	VOutIsInteractive = func(o *VOutput) bool {
		return o.Interactive
	}

	// bip32DerivationKeyEqual returns a predicate that returns true if the
	// BIP-0032 derivation path's public key matches the given target.
	bip32DerivationKeyEqual = func(target []byte) bip32DerivationPredicate {
		return func(d *psbt.Bip32Derivation) bool {
			return bytes.Equal(d.PubKey, target)
		}
	}

	// taprootBip32DerivationKeyEqual returns a predicate that returns true
	// if the Taproot BIP-0032 derivation path's public key matches the
	// given target.
	taprootBip32DerivationKeyEqual = func(
		target []byte) taprootBip32DerivationPredicate {

		return func(d *psbt.TaprootBip32Derivation) bool {
			return bytes.Equal(d.XOnlyPubKey, target)
		}
	}
)

// VPacket is a PSBT extension packet for a virtual transaction. It represents
// the virtual asset state transition as it will be validated by the Taproot
// Asset VM. Some elements within the virtual packet may refer to on-chain
// elements (such as the anchor BTC transaction that was used to anchor the
// input that is spent). But in general a virtual transaction does NOT directly
// map onto a BTC transaction. It is entirely possible that multiple virtual
// transactions will be merged into a single BTC transaction. Thus, each asset
// state transfer is represented in a virtual TX and multiple asset state
// transfers can be anchored within a single BTC transaction.
//
// NOTE: A virtual transaction only carries the asset state transition for a
// single asset ID. If multiple inputs are given, they must all belong to the
// same asset ID (which means those asset UTXOs are being merged and possibly
// split again in this virtual transaction). Therefore, if an anchor output
// carries commitments for multiple assets, a virtual transaction needs to be
// created, signed and then anchored for each asset ID separately.
//
// TODO(guggero): Actually support merging multiple virtual transactions into a
// single BTC transaction.
type VPacket struct {
	// Inputs is the list of asset inputs that are being spent.
	Inputs []*VInput

	// Outputs is the list of new asset outputs that are created by the
	// virtual transaction.
	Outputs []*VOutput

	// ChainParams are the Taproot Asset chain parameters that are used to
	// encode and decode certain contents of the virtual packet.
	ChainParams *address.ChainParams

	// Version is the version of the virtual transaction. This is currently
	// unused but can be used to signal a new version of the virtual PSBT
	// format in the future.
	Version uint8
}

// SetInputAsset sets the input asset that is being spent.
func (p *VPacket) SetInputAsset(index int, a *asset.Asset, proof []byte) {
	if index >= len(p.Inputs) {
		p.Inputs = append(p.Inputs, &VInput{})
	}
	p.Inputs[index].asset = a.Copy()
	p.Inputs[index].proof = proof
	p.Inputs[index].serializeScriptKey(
		a.ScriptKey, p.ChainParams.HDCoinType,
	)
}

// HasSplitCommitment determines if this transaction results in an asset split.
// This is either the case if the value of the input asset is split or if one of
// the outputs is non-interactive (in which case we need to have a zero value
// tombstone asset in the change output).
func (p *VPacket) HasSplitCommitment() (bool, error) {
	for idx := range p.Outputs {
		vOut := p.Outputs[idx]

		// We skip the split root output as it doesn't carry a split
		// commitment. And since it might only carry passive assets, the
		// nil check below would trigger, which is not what we want.
		if vOut.Type.IsSplitRoot() {
			continue
		}

		// If any of the recipient asset is nil, this virtual
		// transaction hasn't been prepared correctly, and we cannot
		// determine if there is a split commitment.
		if vOut.Asset == nil {
			return false, fmt.Errorf("recipient asset %d is nil",
				idx)
		}

		// If there is a split commitment witness present, we know there
		// is a split going on.
		if vOut.Asset.HasSplitCommitmentWitness() {
			return true, nil
		}
	}

	return false, nil
}

// HasSplitRootOutput determines if this virtual transaction has a split root
// output.
func (p *VPacket) HasSplitRootOutput() bool {
	return chanutils.Any(p.Outputs, VOutIsSplitRoot)
}

// HasInteractiveOutput determines if this virtual transaction has an
// interactive output.
func (p *VPacket) HasInteractiveOutput() bool {
	return chanutils.Any(p.Outputs, VOutIsInteractive)
}

// SplitRootOutput returns the split root output in the virtual transaction, or
// an error if there is none or more than one.
func (p *VPacket) SplitRootOutput() (*VOutput, error) {
	count := chanutils.Count(p.Outputs, VOutIsSplitRoot)
	if count != 1 {
		return nil, fmt.Errorf("expected 1 split root output, got %d",
			count)
	}

	return chanutils.First(p.Outputs, VOutIsSplitRoot)
}

// FirstNonSplitRootOutput returns the first non-change output in the virtual
// transaction.
func (p *VPacket) FirstNonSplitRootOutput() (*VOutput, error) {
	result, err := chanutils.First(p.Outputs, VOutIsNotSplitRoot)
	if err != nil {
		return nil, fmt.Errorf("no non split root output found")
	}

	return result, nil
}

// FirstInteractiveOutput returns the first interactive output in the virtual
// transaction.
func (p *VPacket) FirstInteractiveOutput() (*VOutput, error) {
	result, err := chanutils.First(p.Outputs, VOutIsInteractive)
	if err != nil {
		return nil, fmt.Errorf("no interactive output found")
	}

	return result, nil
}

// Anchor is a struct that contains all the information about an anchor output.
type Anchor struct {
	// Value is output value of the anchor output.
	Value btcutil.Amount

	// PkScript is the output script of the anchor output.
	PkScript []byte

	// SigHashType is the signature hash type that should be used to sign
	// the anchor output spend.
	SigHashType txscript.SigHashType

	// InternalKey is the internal key of the anchor output that the input
	// is spending the asset from.
	InternalKey *btcec.PublicKey

	// MerkleRoot is the root of the tap script merkle tree that also
	// contains the Taproot Asset commitment of the anchor output.
	MerkleRoot []byte

	// TapscriptSibling is the tapscript sibling of the Taproot Asset
	// commitment.
	TapscriptSibling []byte

	// Bip32Derivation is the BIP-0032 derivation of the anchor output's
	// internal key.
	Bip32Derivation []*psbt.Bip32Derivation

	// TrBip32Derivation is the Taproot BIP-0032 derivation of the anchor
	// output's internal key.
	TrBip32Derivation []*psbt.TaprootBip32Derivation
}

// VInput represents an input to a virtual asset state transition transaction.
type VInput struct {
	// PInput is the embedded default PSBT input struct that is used for
	// asset related input data.
	psbt.PInput

	// PrevID is the asset previous ID of the asset being spent.
	PrevID asset.PrevID

	// Anchor contains the information about the BTC level anchor
	// transaction that committed to the asset being spent.
	Anchor Anchor

	// asset is the full instance of the asset being spent. It is not
	// exported because the assets script key must be encoded in the PSBT
	// input struct for the signing to work correctly.
	asset *asset.Asset

	// proof is the proof blob that proves the asset being spent was
	// committed to in the anchor transaction above. This cannot be of type
	// proof.Proof directly because that would cause a circular dependency.
	proof []byte
}

// Asset returns the input's asset that's being spent.
func (i *VInput) Asset() *asset.Asset {
	return i.asset
}

// Proof returns the proof blob that the asset being spent was committed to in
// the anchor transaction.
func (i *VInput) Proof() []byte {
	return i.proof
}

// serializeScriptKey serializes the input asset's script key as the PSBT
// derivation information on the virtual input.
func (i *VInput) serializeScriptKey(key asset.ScriptKey, coinType uint32) {
	if key.TweakedScriptKey == nil {
		return
	}

	bip32Derivation, trBip32Derivation := Bip32DerivationFromKeyDesc(
		key.RawKey, coinType,
	)

	i.Bip32Derivation = []*psbt.Bip32Derivation{
		bip32Derivation,
	}
	i.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{
		trBip32Derivation,
	}
	i.TaprootInternalKey = trBip32Derivation.XOnlyPubKey
	i.TaprootMerkleRoot = key.Tweak
}

// deserializeScriptKey deserializes the PSBT derivation information on the
// input into the input asset's script key.
func (i *VInput) deserializeScriptKey() error {
	if i.asset == nil || len(i.TaprootInternalKey) == 0 ||
		len(i.Bip32Derivation) == 0 {

		return nil
	}

	bip32Derivation := i.Bip32Derivation[0]
	rawKeyDesc, err := KeyDescFromBip32Derivation(bip32Derivation)
	if err != nil {
		return fmt.Errorf("error decoding script key derivation info: "+
			"%w", err)
	}

	i.asset.ScriptKey.TweakedScriptKey = &asset.TweakedScriptKey{
		RawKey: rawKeyDesc,
		Tweak:  i.TaprootMerkleRoot,
	}

	return nil
}

// VOutputType represents the type of virtual output.
type VOutputType uint8

const (
	// TypeSimple is a plain full-value or split output that is not a split
	// root and does not carry passive assets. In case of a split, the asset
	// of this output has a split commitment.
	TypeSimple VOutputType = 0

	// TypeSplitRoot is a split root output that carries the change from a
	// split or a tombstone from a non-interactive full value send output.
	// In either case, the asset of this output has a tx witness.
	TypeSplitRoot VOutputType = 1

	// TypePassiveAssetsOnly indicates that this output only carries passive
	// assets and therefore the asset in this output is nil. The passive
	// assets themselves are signed in their own virtual transactions and
	// are not present in this packet.
	TypePassiveAssetsOnly VOutputType = 2

	// TypePassiveSplitRoot is a split root output that carries the change
	// from a split or a tombstone from a non-interactive full value send
	// output, as well as passive assets.
	TypePassiveSplitRoot VOutputType = 3
)

// IsSplitRoot returns true if the output type is a split root, indicating that
// the asset has a tx witness instead of a split witness.
func (t VOutputType) IsSplitRoot() bool {
	return t == TypeSplitRoot || t == TypePassiveSplitRoot
}

// CanBeInteractive returns true if the output type is compatible with being an
// interactive send.
func (t VOutputType) CanBeInteractive() bool {
	switch t {
	case TypeSimple, TypeSplitRoot, TypePassiveAssetsOnly,
		TypePassiveSplitRoot:

		return true

	default:
		return false
	}
}

// CanCarryPassive returns true if the output type is compatible with carrying
// passive assets.
func (t VOutputType) CanCarryPassive() bool {
	switch t {
	case TypePassiveAssetsOnly, TypePassiveSplitRoot:
		return true

	default:
		return false
	}
}

// String returns a human-readable string representation of the output type.
func (t VOutputType) String() string {
	switch t {
	case TypeSimple:
		return "simple"

	case TypeSplitRoot:
		return "split_root"

	case TypePassiveAssetsOnly:
		return "passive_assets_only"

	case TypePassiveSplitRoot:
		return "passive_split_root"

	default:
		return fmt.Sprintf("unknown <%d>", t)
	}
}

// InputCommitments is a map from virtual package input index to its
// associated Taproot Asset commitment.
type InputCommitments = map[int]*commitment.TapCommitment

// VOutput represents an output of a virtual asset state transition.
type VOutput struct {
	// Amount is the amount of units of the asset that this output is
	// creating. This can be zero in case of an asset tombstone in a
	// non-interactive full value send scenario. When serialized, this will
	// be stored as the value of the wire.TxOut of the PSBT's unsigned TX.
	Amount uint64

	// Type indicates what type of output this is, which has an influence on
	// whether the asset is set or what witness type is expected to be
	// generated for the asset.
	Type VOutputType

	// Interactive, when set to true, indicates that the receiver of the
	// output is aware of the asset transfer and can therefore receive a
	// full value send directly and without a tombstone in the change
	// output.
	Interactive bool

	// AnchorOutputIndex indicates in which output index of the BTC
	// transaction this asset output should be committed to. Multiple asset
	// outputs can be committed to within the same BTC transaction output.
	AnchorOutputIndex uint32

	// AnchorOutputInternalKey is the internal key of the anchor output that
	// will be used to create the anchor Taproot output key to which this
	// asset output will be committed to.
	AnchorOutputInternalKey *btcec.PublicKey

	// AnchorOutputBip32Derivation is the BIP-0032 derivation of the anchor
	// output's internal key.
	AnchorOutputBip32Derivation []*psbt.Bip32Derivation

	// AnchorOutputTaprootBip32Derivation is the Taproot BIP-0032 derivation
	// of the anchor output's internal key.
	AnchorOutputTaprootBip32Derivation []*psbt.TaprootBip32Derivation

	// AnchorOutputTapscriptSibling is the preimage of the tapscript sibling
	// of the Taproot Asset commitment.
	AnchorOutputTapscriptSibling *commitment.TapscriptPreimage

	// Asset is the actual asset (including witness or split commitment
	// data) that this output will commit to on chain. This asset will be
	// included in the proof sent to the recipient of this output.
	Asset *asset.Asset

	// SplitAsset is the original split asset that was created when creating
	// the split commitment.
	//
	// NOTE: This is only set if the above Asset is the root asset of a
	// split. Compared to the root asset, this does not have a split
	// commitment root and no TX witness, but instead has the split
	// commitment set.
	SplitAsset *asset.Asset

	// ScriptKey is the new script key of the recipient of the asset. When
	// serialized, this will be stored in the TaprootInternalKey and
	// TaprootDerivationPath fields of the PSBT output.
	ScriptKey asset.ScriptKey
}

// SplitLocator creates a split locator from the output. The asset ID is passed
// in for cases in which the asset is not yet set on the output.
func (o *VOutput) SplitLocator(assetID asset.ID) commitment.SplitLocator {
	return commitment.SplitLocator{
		OutputIndex: o.AnchorOutputIndex,
		AssetID:     assetID,
		ScriptKey:   asset.ToSerialized(o.ScriptKey.PubKey),
		Amount:      o.Amount,
	}
}

// SetAnchorInternalKey sets the internal key and derivation path of the anchor
// output based on the given key descriptor and coin type.
func (o *VOutput) SetAnchorInternalKey(keyDesc keychain.KeyDescriptor,
	coinType uint32) {

	bip32Derivation, trBip32Derivation := Bip32DerivationFromKeyDesc(
		keyDesc, coinType,
	)
	o.AnchorOutputInternalKey = keyDesc.PubKey
	o.AnchorOutputBip32Derivation = append(
		o.AnchorOutputBip32Derivation, bip32Derivation,
	)
	o.AnchorOutputTaprootBip32Derivation = append(
		o.AnchorOutputTaprootBip32Derivation, trBip32Derivation,
	)
}

// AnchorKeyToDesc attempts to extract the key descriptor of the anchor output
// from the anchor output BIP-0032 derivation information.
func (o *VOutput) AnchorKeyToDesc() (keychain.KeyDescriptor, error) {
	if len(o.AnchorOutputBip32Derivation) == 0 {
		return keychain.KeyDescriptor{}, fmt.Errorf("anchor output " +
			"bip32 derivation is missing")
	}

	if len(o.AnchorOutputBip32Derivation) > 1 {
		return keychain.KeyDescriptor{}, fmt.Errorf("multiple anchor " +
			"output bip32 derivations found, only one supported " +
			"currently")
	}

	return KeyDescFromBip32Derivation(o.AnchorOutputBip32Derivation[0])
}

// KeyDescFromBip32Derivation attempts to extract the key descriptor from the
// given public key and BIP-0032 derivation information.
func KeyDescFromBip32Derivation(
	bip32Derivation *psbt.Bip32Derivation) (keychain.KeyDescriptor, error) {

	if len(bip32Derivation.PubKey) == 0 {
		return keychain.KeyDescriptor{}, fmt.Errorf("pubkey is missing")
	}

	pubKey, err := btcec.ParsePubKey(bip32Derivation.PubKey)
	if err != nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("error parsing "+
			"pubkey: %w", err)
	}

	keyLocator, err := extractLocatorFromPath(bip32Derivation.Bip32Path)
	if err != nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("unable to "+
			"extract locator from path: %w", err)
	}

	return keychain.KeyDescriptor{
		PubKey:     pubKey,
		KeyLocator: keyLocator,
	}, nil
}

// Bip32DerivationFromKeyDesc returns the default and Taproot BIP-0032 key
// derivation information from the given key descriptor information.
func Bip32DerivationFromKeyDesc(keyDesc keychain.KeyDescriptor,
	coinType uint32) (*psbt.Bip32Derivation, *psbt.TaprootBip32Derivation) {

	bip32Derivation := &psbt.Bip32Derivation{
		PubKey: keyDesc.PubKey.SerializeCompressed(),
		Bip32Path: []uint32{
			keychain.BIP0043Purpose + hdkeychain.HardenedKeyStart,
			coinType + hdkeychain.HardenedKeyStart,
			uint32(keyDesc.Family) +
				uint32(hdkeychain.HardenedKeyStart),
			0,
			keyDesc.Index,
		},
	}

	return bip32Derivation, &psbt.TaprootBip32Derivation{
		XOnlyPubKey:          bip32Derivation.PubKey[1:],
		MasterKeyFingerprint: bip32Derivation.MasterKeyFingerprint,
		Bip32Path:            bip32Derivation.Bip32Path,
		LeafHashes:           make([][]byte, 0),
	}
}

// AddBip32Derivation adds the given target BIP-0032 derivation to the list of
// derivations if it is not already present.
func AddBip32Derivation(derivations []*psbt.Bip32Derivation,
	target *psbt.Bip32Derivation) []*psbt.Bip32Derivation {

	if target == nil {
		return derivations
	}

	predicate := bip32DerivationKeyEqual(target.PubKey)
	if chanutils.Any(derivations, predicate) {
		return derivations
	}

	return append(derivations, target)
}

// AddTaprootBip32Derivation adds the given target Taproot BIP-0032 derivation
// to the list of derivations if it is not already present.
func AddTaprootBip32Derivation(derivations []*psbt.TaprootBip32Derivation,
	target *psbt.TaprootBip32Derivation) []*psbt.TaprootBip32Derivation {

	if target == nil {
		return derivations
	}

	predicate := taprootBip32DerivationKeyEqual(target.XOnlyPubKey)
	if chanutils.Any(derivations, predicate) {
		return derivations
	}

	return append(derivations, target)
}

// extractLocatorFromPath extracts the key family and index from the given
// BIP-0032 derivation path. The derivation path is expected to be of the form:
//
//	m/1017'/coin_type'/key_family'/0/index.
func extractLocatorFromPath(path []uint32) (keychain.KeyLocator, error) {
	loc := keychain.KeyLocator{}
	if len(path) != 5 {
		return loc, fmt.Errorf("invalid bip32 derivation path length: "+
			"%d", len(path))
	}

	if path[0] != keychain.BIP0043Purpose+hdkeychain.HardenedKeyStart {
		return loc, fmt.Errorf("invalid purpose, expected internal "+
			"purpose, got %d", path[0])
	}

	if path[2] < hdkeychain.HardenedKeyStart {
		return loc, fmt.Errorf("key family must be hardened")
	}

	loc.Family = keychain.KeyFamily(path[2] - hdkeychain.HardenedKeyStart)
	loc.Index = path[4]

	return loc, nil
}

// serializeTweakedScriptKey serializes a script key as the PSBT derivation
// information on the PSBT output.
func serializeTweakedScriptKey(key *asset.TweakedScriptKey,
	coinType uint32) psbt.POutput {

	pOut := psbt.POutput{}
	if key == nil {
		return pOut
	}

	bip32Derivation, trBip32Derivation := Bip32DerivationFromKeyDesc(
		key.RawKey, coinType,
	)

	// If we have a non-empty tweak of the script key, it means we don't
	// have a BIP-0086 key, so we need to add the tweak to the derivation
	// path as a leaf hash (since the tweak will represent the root hash of
	// the script tree). Unfortunately outputs don't have the
	// TaprootMerkleRoot field as inputs have.
	if len(key.Tweak) > 0 {
		trBip32Derivation.LeafHashes = [][]byte{key.Tweak}
	}

	pOut.Bip32Derivation = []*psbt.Bip32Derivation{bip32Derivation}
	pOut.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{
		trBip32Derivation,
	}
	pOut.TaprootInternalKey = trBip32Derivation.XOnlyPubKey

	return pOut
}

// deserializeTweakedScriptKey deserializes the PSBT derivation information on
// the PSBT output into the script key.
func deserializeTweakedScriptKey(pOut psbt.POutput) (*asset.TweakedScriptKey,
	error) {

	// The fields aren't mandatory.
	if len(pOut.TaprootInternalKey) == 0 || len(pOut.Bip32Derivation) == 0 {
		return nil, nil
	}

	bip32Derivation := pOut.Bip32Derivation[0]
	rawKeyDesc, err := KeyDescFromBip32Derivation(bip32Derivation)
	if err != nil {
		return nil, fmt.Errorf("error decoding script key derivation "+
			"info: %w", err)
	}

	var tweak []byte
	if len(pOut.TaprootBip32Derivation) > 0 &&
		len(pOut.TaprootBip32Derivation[0].LeafHashes) > 0 {

		tweak = pOut.TaprootBip32Derivation[0].LeafHashes[0]
	}

	return &asset.TweakedScriptKey{
		RawKey: rawKeyDesc,
		Tweak:  tweak,
	}, nil
}
