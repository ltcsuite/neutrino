package chainsync

import (
	"fmt"

	"github.com/ltcsuite/ltcd/chaincfg"
	"github.com/ltcsuite/ltcd/chaincfg/chainhash"
	"github.com/ltcsuite/ltcd/wire"
)

// ErrCheckpointMismatch is returned if given filter headers don't pass our
// control check.
var ErrCheckpointMismatch = fmt.Errorf("checkpoint doesn't match")

// filterHeaderCheckpoints holds a mapping from heights to filter headers for
// various heights. We use them to check whether peers are serving us the
// expected filter headers.
var filterHeaderCheckpoints = map[wire.BitcoinNet]map[uint32]*chainhash.Hash{
	// Mainnet filter header checkpoints.
	// TODO: fill with litecoin cfilter hash
	chaincfg.MainNetParams.Net: map[uint32]*chainhash.Hash{
		1500: hashFromStr("841a2965955dd288cfa707a755d05a54e45f8bd476835ec9af4402a2b59a2967"),
		4032: hashFromStr("9ce90e427198fc0ef05e5905ce3503725b80e26afd35a987965fd7e3d9cf0846"),
		8064: hashFromStr("eb984353fc5190f210651f150c40b8a4bab9eeeff0b729fcb3987da694430d70"),
		16128: hashFromStr("602edf1859b7f9a6af809f1d9b0e6cb66fdc1d4d9dcd7a4bec03e12a1ccd153d"),
		23420: hashFromStr("d80fdf9ca81afd0bd2b2a90ac3a9fe547da58f2530ec874e978fce0b5101b507"),
		50000: hashFromStr("69dc37eb029b68f075a5012dcc0419c127672adb4f3a32882b2b3e71d07a20a6"),
		80000: hashFromStr("4fcb7c02f676a300503f49c764a89955a8f920b46a8cbecb4867182ecdb2e90a"),
		120000: hashFromStr("bd9d26924f05f6daa7f0155f32828ec89e8e29cee9e7121b026a7a3552ac6131"),
		161500: hashFromStr("dbe89880474f4bb4f75c227c77ba1cdc024991123b28b8418dbbf7798471ff43"),
		179620: hashFromStr("2ad9c65c990ac00426d18e446e0fd7be2ffa69e9a7dcb28358a50b2b78b9f709"),
		240000: hashFromStr("7140d1c4b4c2157ca217ee7636f24c9c73db39c4590c4e6eab2e3ea1555088aa"),
		383640: hashFromStr("2b6809f094a9215bafc65eb3f110a35127a34be94b7d0590a096c3f126c6f364"),
		409004: hashFromStr("487518d663d9f1fa08611d9395ad74d982b667fbdc0e77e9cf39b4f1355908a3"),
		456000: hashFromStr("bf34f71cc6366cd487930d06be22f897e34ca6a40501ac7d401be32456372004"),
		638902: hashFromStr("15238656e8ec63d28de29a8c75fcf3a5819afc953dcd9cc45cecc53baec74f38"),
		721000: hashFromStr("198a7b4de1df9478e2463bd99d75b714eab235a2e63e741641dc8a759a9840e5"),
	},

	// Testnet filter header checkpoints.
	chaincfg.TestNet4Params.Net: map[uint32]*chainhash.Hash{
		26115: hashFromStr("817d5b509e91ab5e439652eee2f59271bbc7ba85021d720cdb6da6565b43c14f"),
		43928: hashFromStr("7d86614c153f5ef6ad878483118ae523e248cd0dd0345330cb148e812493cbb4"),
		69296: hashFromStr("66c2f58da3cfd282093b55eb09c1f5287d7a18801a8ff441830e67e8771010df"),
		99949: hashFromStr("8dd471cb5aecf5ead91e7e4b1e932c79a0763060f8d93671b6801d115bfc6cde"),
		159256: hashFromStr("ab5b0b9968842f5414804591119d6db829af606864b1959a25d6f5c114afb2b7"),
	},
}

// ControlCFHeader controls the given filter header against our list of
// checkpoints. It returns ErrCheckpointMismatch if we have a checkpoint at the
// given height, and it doesn't match.
func ControlCFHeader(params chaincfg.Params, fType wire.FilterType,
	height uint32, filterHeader *chainhash.Hash) error {

	if fType != wire.GCSFilterRegular {
		return fmt.Errorf("unsupported filter type %v", fType)
	}

	control, ok := filterHeaderCheckpoints[params.Net]
	if !ok {
		return nil
	}

	hash, ok := control[height]
	if !ok {
		return nil
	}

	if *filterHeader != *hash {
		return ErrCheckpointMismatch
	}

	return nil
}

// hashFromStr makes a chainhash.Hash from a valid hex string. If the string is
// invalid, a nil pointer will be returned.
func hashFromStr(hexStr string) *chainhash.Hash {
	hash, _ := chainhash.NewHashFromStr(hexStr)
	return hash
}
