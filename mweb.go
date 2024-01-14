package neutrino

import (
	"bytes"
	"encoding/binary"
	"math/bits"

	"github.com/ltcsuite/ltcd/chaincfg/chainhash"
	"github.com/ltcsuite/ltcd/ltcutil/bloom"
	"github.com/ltcsuite/ltcd/txscript"
	"github.com/ltcsuite/ltcd/wire"
	"lukechampine.com/blake3"
)

func verifyMwebHeader(
	mwebHeader *wire.MsgMwebHeader, mwebLeafset *wire.MsgMwebLeafset,
	lastHeight uint32, lastHash *chainhash.Hash) bool {

	if mwebHeader == nil || mwebLeafset == nil {
		return false
	}
	log.Infof("Got mwebheader and mwebleafset at (block_height=%v, block_hash=%v)",
		lastHeight, *lastHash)

	if mwebHeader.Merkle.Header.BlockHash() != *lastHash {
		log.Infof("Block hash mismatch, merkle header hash=%v, block hash=%v",
			mwebHeader.Merkle.Header.BlockHash(), *lastHash)
		return false
	}

	extractResult := bloom.VerifyMerkleBlock(&mwebHeader.Merkle)
	if !extractResult.Root.IsEqual(&mwebHeader.Merkle.Header.MerkleRoot) {
		log.Info("mwebheader merkle block is bad")
		return false
	}

	if !mwebHeader.Hogex.IsHogEx {
		log.Info("mwebheader hogex is not hogex")
		return false
	}

	// Validate that the hash of the HogEx transaction in the tx message
	// matches the hash in the merkleblock message, and that it’s the last
	// transaction committed to by the merkle root of the block.
	finalTx := extractResult.Match[len(extractResult.Match)-1]
	if mwebHeader.Hogex.TxHash() != *finalTx {
		log.Infof("Tx hash mismatch, hogex=%v, last merkle tx=%v",
			mwebHeader.Hogex.TxHash(), *finalTx)
		return false
	}
	finalTxPos := extractResult.Index[len(extractResult.Index)-1]
	if finalTxPos != mwebHeader.Merkle.Transactions-1 {
		log.Infof("Tx index mismatch, got=%v, expected=%v",
			finalTxPos, mwebHeader.Merkle.Transactions-1)
		return false
	}

	// Validate that the pubkey script of the first output contains the HogAddr,
	// which shall consist of <OP_8><0x20> followed by the 32-byte hash of the
	// MWEB header.
	mwebHeaderHash := mwebHeader.MwebHeader.Hash()
	script := append([]byte{txscript.OP_8, 0x20}, mwebHeaderHash[:]...)
	if !bytes.Equal(mwebHeader.Hogex.TxOut[0].PkScript, script) {
		log.Infof("HogAddr mismatch, hogex=%v, expected=%v",
			mwebHeader.Hogex.TxOut[0].PkScript, script)
		return false
	}

	// Verify that the hash of the leafset bitmap matches the
	// leafset_root value in the MWEB header.
	leafsetRoot := chainhash.Hash(blake3.Sum256(mwebLeafset.Leafset))
	if leafsetRoot != mwebHeader.MwebHeader.LeafsetRoot {
		log.Infof("Leafset root mismatch, leafset=%v, in header=%v",
			leafsetRoot, mwebHeader.MwebHeader.LeafsetRoot)
		return false
	}

	log.Infof("Verified mwebheader and mwebleafset at (block_height=%v, block_hash=%v)",
		lastHeight, *lastHash)
	return true
}

type (
	leafset []byte
	leafIdx uint64
	nodeIdx uint64
)

func (l leafset) contains(i leafIdx) bool {
	if int(i/8) >= len(l) {
		return false
	}
	return l[i/8]&(0x80>>(i%8)) > 0
}

func (i leafIdx) nodeIdx() nodeIdx {
	return nodeIdx(2*i) - nodeIdx(bits.OnesCount64(uint64(i)))
}

func (i nodeIdx) height() uint64 {
	height := uint64(i)
	h := 64 - bits.LeadingZeros64(uint64(i))
	for peakSize := uint64(1<<h - 1); peakSize > 0; peakSize >>= 1 {
		if height >= peakSize {
			height -= peakSize
		}
	}
	return height
}

func (i nodeIdx) leafIdx() leafIdx {
	leafIndex := uint64(0)
	numLeft := uint64(i)
	h := 64 - bits.LeadingZeros64(uint64(i))
	for peakSize := uint64(1<<h - 1); peakSize > 0; peakSize >>= 1 {
		if numLeft >= peakSize {
			leafIndex += (peakSize + 1) / 2
			numLeft -= peakSize
		}
	}
	return leafIdx(leafIndex)
}

func (i nodeIdx) left(height uint64) nodeIdx {
	return i - (1 << height)
}

func (i nodeIdx) right() nodeIdx {
	return i - 1
}

func (i nodeIdx) hash(data []byte) *chainhash.Hash {
	h := blake3.New(32, nil)
	binary.Write(h, binary.LittleEndian, uint64(i))
	wire.WriteVarBytes(h, 0, data)
	hash := &chainhash.Hash{}
	h.Sum(hash[:0])
	return hash
}

func (i nodeIdx) parentHash(left, right []byte) *chainhash.Hash {
	h := blake3.New(32, nil)
	binary.Write(h, binary.LittleEndian, uint64(i))
	h.Write(left)
	h.Write(right)
	hash := &chainhash.Hash{}
	h.Sum(hash[:0])
	return hash
}

func calcPeaks(nodes uint64) (peaks []nodeIdx) {
	sumPrevPeaks := uint64(0)
	h := 64 - bits.LeadingZeros64(nodes)
	for peakSize := uint64(1<<h - 1); peakSize > 0; peakSize >>= 1 {
		if nodes >= peakSize {
			peaks = append(peaks, nodeIdx(sumPrevPeaks+peakSize-1))
			sumPrevPeaks += peakSize
			nodes -= peakSize
		}
	}
	return
}

type verifyMwebUtxosVars struct {
	mwebUtxos                 *wire.MsgMwebUtxos
	leafset                   leafset
	firstLeafIdx, lastLeafIdx leafIdx
	leavesUsed, hashesUsed    int
	isProofHash               map[nodeIdx]bool
}

func (v *verifyMwebUtxosVars) nextLeaf() (leafIndex leafIdx, hash *chainhash.Hash) {
	if v.leavesUsed == len(v.mwebUtxos.Utxos) {
		return
	}
	utxo := v.mwebUtxos.Utxos[v.leavesUsed]
	leafIndex = leafIdx(utxo.LeafIndex)
	hash = &utxo.OutputId
	v.leavesUsed++
	return
}

func (v *verifyMwebUtxosVars) nextHash(nodeIdx nodeIdx) (hash *chainhash.Hash) {
	if v.hashesUsed == len(v.mwebUtxos.ProofHashes) {
		return
	}
	hash = v.mwebUtxos.ProofHashes[v.hashesUsed]
	v.hashesUsed++
	v.isProofHash[nodeIdx] = true
	return
}

func (v *verifyMwebUtxosVars) calcNodeHash(nodeIdx nodeIdx, height uint64) *chainhash.Hash {
	if nodeIdx < v.firstLeafIdx.nodeIdx() || v.isProofHash[nodeIdx] {
		return v.nextHash(nodeIdx)
	}
	if height == 0 {
		leafIdx := nodeIdx.leafIdx()
		if !v.leafset.contains(leafIdx) {
			return nil
		}
		leafIdx2, outputId := v.nextLeaf()
		if leafIdx != leafIdx2 || outputId == nil {
			return nil
		}
		return nodeIdx.hash(outputId[:])
	}
	left := v.calcNodeHash(nodeIdx.left(height), height-1)
	var right *chainhash.Hash
	if v.lastLeafIdx.nodeIdx() <= nodeIdx.left(height) {
		right = v.nextHash(nodeIdx.right())
	} else {
		right = v.calcNodeHash(nodeIdx.right(), height-1)
	}
	switch {
	case left == nil && right == nil:
		return nil
	case left == nil:
		if left = v.nextHash(nodeIdx.left(height)); left == nil {
			return nil
		}
	case right == nil:
		if right = v.nextHash(nodeIdx.right()); right == nil {
			return nil
		}
	}
	return nodeIdx.parentHash(left[:], right[:])
}

func verifyMwebUtxos(mwebHeader *wire.MsgMwebHeader,
	mwebLeafset *wire.MsgMwebLeafset, mwebUtxos *wire.MsgMwebUtxos) bool {

	if mwebUtxos.StartIndex == 0 &&
		len(mwebUtxos.Utxos) == 0 &&
		len(mwebUtxos.ProofHashes) == 0 &&
		mwebHeader.MwebHeader.OutputRoot.IsEqual(&chainhash.Hash{}) &&
		mwebHeader.MwebHeader.OutputMMRSize == 0 {
		return true
	} else if len(mwebUtxos.Utxos) == 0 ||
		mwebHeader.MwebHeader.OutputMMRSize == 0 {
		return false
	}

	v := &verifyMwebUtxosVars{
		mwebUtxos:    mwebUtxos,
		leafset:      leafset(mwebLeafset.Leafset),
		firstLeafIdx: leafIdx(mwebUtxos.StartIndex),
		lastLeafIdx:  leafIdx(mwebUtxos.StartIndex),
		isProofHash:  make(map[nodeIdx]bool),
	}
	nextLeafIdx := leafIdx(mwebHeader.MwebHeader.OutputMMRSize)

	for i := 0; ; i++ {
		if !v.leafset.contains(v.lastLeafIdx) {
			return false
		}
		if leafIdx(mwebUtxos.Utxos[i].LeafIndex) != v.lastLeafIdx {
			return false
		}
		if i == len(mwebUtxos.Utxos)-1 {
			break
		}
		for {
			v.lastLeafIdx++
			if v.lastLeafIdx == nextLeafIdx {
				return false
			}
			if v.leafset.contains(v.lastLeafIdx) {
				break
			}
		}
	}

	peaks := calcPeaks(uint64(nextLeafIdx.nodeIdx()))
	var peakHashes []*chainhash.Hash
	for i := 0; i < 2; i++ {
		peakHashes = nil
		v.leavesUsed = 0
		v.hashesUsed = 0

		for _, peakNodeIdx := range peaks {
			peakHash := v.calcNodeHash(peakNodeIdx, peakNodeIdx.height())
			if peakHash == nil {
				return false
			}
			peakHashes = append(peakHashes, peakHash)
			if v.lastLeafIdx.nodeIdx() <= peakNodeIdx {
				if peakNodeIdx != peaks[len(peaks)-1] {
					baggedPeak := v.nextHash(nextLeafIdx.nodeIdx())
					if baggedPeak == nil {
						return false
					}
					peakHashes = append(peakHashes, baggedPeak)
				}
				break
			}
		}
		if v.leavesUsed != len(v.mwebUtxos.Utxos) ||
			v.hashesUsed != len(v.mwebUtxos.ProofHashes) {
			return false
		}
	}

	baggedPeak := peakHashes[len(peakHashes)-1]
	for i := len(peakHashes) - 2; i >= 0; i-- {
		baggedPeak = nextLeafIdx.nodeIdx().parentHash(peakHashes[i][:], baggedPeak[:])
	}
	return baggedPeak.IsEqual(&mwebHeader.MwebHeader.OutputRoot)
}
