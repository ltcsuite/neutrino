package mwebdb

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/ltcsuite/ltcd/chaincfg/chainhash"
	"github.com/ltcsuite/ltcd/ltcutil/mweb"
	"github.com/ltcsuite/ltcd/wire"
	"github.com/ltcsuite/ltcwallet/walletdb"
)

var (
	// rootBucket is the name of the root bucket for this package. Within
	// this bucket, sub-buckets are stored which themselves store the
	// actual coins.
	rootBucket = []byte("mweb-coindb")

	// heightBucket is the bucket that stores the mapping between
	// block heights and number of leaves.
	heightBucket = []byte("heights")

	// coinBucket is the bucket that stores the coins.
	coinBucket = []byte("coins")

	// leafBucket is the bucket that stores the mapping between
	// leaf indices and output IDs.
	leafBucket = []byte("leaves")

	// rollbackHeight is the key that stores the rollback height.
	rollbackHeight = []byte("rollbackHeight")
)

var (
	// ErrCoinNotFound is returned when a coin for an output ID is
	// unable to be located.
	ErrCoinNotFound = fmt.Errorf("unable to find coin")

	// ErrLeafNotFound is returned when a leaf is unable to be located.
	ErrLeafNotFound = fmt.Errorf("unable to find leaf")

	// ErrUnexpectedValueLen is returned when the bytes value is
	// of an unexpected length.
	ErrUnexpectedValueLen = fmt.Errorf("unexpected value length")
)

// CoinDatabase is an interface which represents an object that is capable of
// storing and retrieving coins according to their corresponding output ID.
type CoinDatabase interface {
	// Get rollback height.
	GetRollbackHeight() (uint32, error)

	// Set rollback height.
	PutRollbackHeight(uint32) error

	// Clear rollback height.
	ClearRollbackHeight(uint32) error

	// Get the block height to number of leaves mapping.
	GetLeavesAtHeight() (map[uint32]uint64, error)

	// Set the block height to number of leaves mapping.
	PutLeavesAtHeight(map[uint32]uint64) error

	// Get the leafset marking the unspent indices.
	GetLeafset() (*mweb.Leafset, error)

	// Set the leafset and purge the specified leaves and their
	// associated coins from persistent storage.
	PutLeafsetAndPurge(*mweb.Leafset, []uint64) error

	// PutCoins stores coins to persistent storage.
	PutCoins([]*wire.MwebNetUtxo) error

	// FetchCoin attempts to fetch a coin with the given output ID
	// from persistent storage. In the case that a coin matching the
	// target output ID cannot be found, then ErrCoinNotFound is to be
	// returned.
	FetchCoin(*chainhash.Hash) (*wire.MwebOutput, error)

	// FetchLeaves fetches the coins corresponding to the leaves specified.
	FetchLeaves([]uint64) ([]*wire.MwebNetUtxo, error)

	// PurgeCoins purges all coins from persistent storage.
	PurgeCoins() error
}

// CoinStore is an implementation of the CoinDatabase interface which is
// backed by boltdb.
type CoinStore struct {
	db walletdb.DB
}

// A compile-time check to ensure the CoinStore adheres to the CoinDatabase
// interface.
var _ CoinDatabase = (*CoinStore)(nil)

// NewCoinStore creates a new instance of the CoinStore given an
// already open database.
func NewCoinStore(db walletdb.DB) (*CoinStore, error) {
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		// As part of our initial setup, we'll try to create the top
		// level root bucket. If this already exists, then we can
		// exit early.
		rootBucket, err := tx.CreateTopLevelBucket(rootBucket)
		if err != nil {
			return err
		}

		// If the main bucket doesn't already exist, then we'll need to
		// create the sub-buckets.
		_, err = rootBucket.CreateBucketIfNotExists(heightBucket)
		if err != nil {
			return err
		}
		_, err = rootBucket.CreateBucketIfNotExists(coinBucket)
		if err != nil {
			return err
		}
		_, err = rootBucket.CreateBucketIfNotExists(leafBucket)
		return err
	})
	if err != nil && err != walletdb.ErrBucketExists {
		return nil, err
	}

	return &CoinStore{db: db}, nil
}

// Get rollback height.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) GetRollbackHeight() (height uint32, err error) {
	err = walletdb.View(c.db, func(tx walletdb.ReadTx) error {
		rootBucket := tx.ReadBucket(rootBucket)

		b := rootBucket.Get(rollbackHeight)
		if b != nil {
			if len(b) != 4 {
				return ErrUnexpectedValueLen
			}
			height = binary.LittleEndian.Uint32(b)
		}
		return nil
	})
	return
}

// Set rollback height.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) PutRollbackHeight(height uint32) error {
	return walletdb.Update(c.db, func(tx walletdb.ReadWriteTx) error {
		rootBucket := tx.ReadWriteBucket(rootBucket)

		b := binary.LittleEndian.AppendUint32(nil, height)
		return rootBucket.Put(rollbackHeight, b)
	})
}

// Clear rollback height.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) ClearRollbackHeight(height uint32) error {
	return walletdb.Update(c.db, func(tx walletdb.ReadWriteTx) error {
		rootBucket := tx.ReadWriteBucket(rootBucket)

		b := rootBucket.Get(rollbackHeight)
		if b == nil {
			return nil
		}
		if len(b) != 4 {
			return ErrUnexpectedValueLen
		}
		if binary.LittleEndian.Uint32(b) != height {
			return nil
		}
		return rootBucket.Delete(rollbackHeight)
	})
}

// Get the block height to number of leaves mapping.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) GetLeavesAtHeight() (map[uint32]uint64, error) {
	m := make(map[uint32]uint64)
	err := walletdb.View(c.db, func(tx walletdb.ReadTx) error {
		rootBucket := tx.ReadBucket(rootBucket)
		heightBucket := rootBucket.NestedReadBucket(heightBucket)

		return heightBucket.ForEach(func(k, v []byte) error {
			height := binary.LittleEndian.Uint32(k)
			m[height] = binary.LittleEndian.Uint64(v)
			return nil
		})
	})
	return m, err
}

// Set the block height to number of leaves mapping.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) PutLeavesAtHeight(m map[uint32]uint64) error {
	return walletdb.Update(c.db, func(tx walletdb.ReadWriteTx) error {
		rootBucket := tx.ReadWriteBucket(rootBucket)
		heightBucket := rootBucket.NestedReadWriteBucket(heightBucket)

		for height, numLeaves := range m {
			err := heightBucket.Put(
				binary.LittleEndian.AppendUint32(nil, height),
				binary.LittleEndian.AppendUint64(nil, numLeaves),
			)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// Get the leafset marking the unspent indices.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) GetLeafset() (leafset *mweb.Leafset, err error) {
	leafset = &mweb.Leafset{}
	err = walletdb.View(c.db, func(tx walletdb.ReadTx) error {
		rootBucket := tx.ReadBucket(rootBucket)

		b := rootBucket.Get([]byte("leafset"))
		if b == nil {
			return nil
		}
		return leafset.Deserialize(bytes.NewReader(b))
	})
	return
}

// Set the leafset and purge the specified leaves and their associated
// coins from persistent storage.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) PutLeafsetAndPurge(leafset *mweb.Leafset,
	removedLeaves []uint64) error {

	return walletdb.Update(c.db, func(tx walletdb.ReadWriteTx) error {
		rootBucket := tx.ReadWriteBucket(rootBucket)
		coinBucket := rootBucket.NestedReadWriteBucket(coinBucket)
		leafBucket := rootBucket.NestedReadWriteBucket(leafBucket)

		var buf bytes.Buffer
		err := leafset.Serialize(&buf)
		if err != nil {
			return err
		}
		err = rootBucket.Put([]byte("leafset"), buf.Bytes())
		if err != nil {
			return err
		}

		for _, leafIndex := range removedLeaves {
			leafIndex := binary.LittleEndian.AppendUint64(nil, leafIndex)
			outputId := leafBucket.Get(leafIndex)
			if outputId == nil {
				return ErrLeafNotFound
			}
			if err = coinBucket.Delete(outputId); err != nil {
				return err
			}
			if err = leafBucket.Delete(leafIndex); err != nil {
				return err
			}
		}

		return nil
	})
}

// PutCoins stores coins to persistent storage.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) PutCoins(coins []*wire.MwebNetUtxo) error {
	return walletdb.Update(c.db, func(tx walletdb.ReadWriteTx) error {
		rootBucket := tx.ReadWriteBucket(rootBucket)
		coinBucket := rootBucket.NestedReadWriteBucket(coinBucket)
		leafBucket := rootBucket.NestedReadWriteBucket(leafBucket)

		for _, coin := range coins {
			var buf bytes.Buffer
			err := binary.Write(&buf, binary.LittleEndian, coin.Height)
			if err != nil {
				return err
			}
			if err = coin.Output.Serialize(&buf); err != nil {
				return err
			}

			err = coinBucket.Put(coin.OutputId[:], buf.Bytes())
			if err != nil {
				return err
			}

			leafIndex := binary.LittleEndian.AppendUint64(nil, coin.LeafIndex)
			err = leafBucket.Put(leafIndex, coin.OutputId[:])
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// FetchCoin attempts to fetch a coin with the given output ID from
// persistent storage.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) FetchCoin(outputId *chainhash.Hash) (*wire.MwebOutput, error) {
	var coin wire.MwebOutput

	err := walletdb.View(c.db, func(tx walletdb.ReadTx) error {
		rootBucket := tx.ReadBucket(rootBucket)
		coinBucket := rootBucket.NestedReadBucket(coinBucket)

		coinBytes := coinBucket.Get(outputId[:])
		if coinBytes == nil {
			return ErrCoinNotFound
		}
		buf := bytes.NewReader(coinBytes)

		var height int32
		err := binary.Read(buf, binary.LittleEndian, &height)
		if err != nil {
			return err
		}
		return coin.Deserialize(buf)
	})
	if err != nil {
		return nil, err
	}

	return &coin, nil
}

// FetchLeaves fetches the coins corresponding to the leaves specified.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) FetchLeaves(leaves []uint64) ([]*wire.MwebNetUtxo, error) {
	var coins []*wire.MwebNetUtxo

	err := walletdb.View(c.db, func(tx walletdb.ReadTx) error {
		rootBucket := tx.ReadBucket(rootBucket)
		coinBucket := rootBucket.NestedReadBucket(coinBucket)
		leafBucket := rootBucket.NestedReadBucket(leafBucket)

		for _, leaf := range leaves {
			leafIndex := binary.LittleEndian.AppendUint64(nil, leaf)
			outputId := bytes.Clone(leafBucket.Get(leafIndex))
			if outputId == nil {
				return ErrLeafNotFound
			}

			coinBytes := coinBucket.Get(outputId)
			if coinBytes == nil {
				return ErrCoinNotFound
			}
			buf := bytes.NewReader(coinBytes)

			coin := &wire.MwebNetUtxo{
				LeafIndex: leaf,
				Output:    &wire.MwebOutput{},
				OutputId:  (*chainhash.Hash)(outputId),
			}
			coins = append(coins, coin)

			err := binary.Read(buf, binary.LittleEndian, &coin.Height)
			if err != nil {
				return err
			}
			if err = coin.Output.Deserialize(buf); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return coins, nil
}

// PurgeCoins purges all coins from persistent storage.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) PurgeCoins() error {
	return walletdb.Update(c.db, func(tx walletdb.ReadWriteTx) error {
		if err := tx.DeleteTopLevelBucket(rootBucket); err != nil {
			return err
		}
		rootBucket, err := tx.CreateTopLevelBucket(rootBucket)
		if err != nil {
			return err
		}

		if _, err := rootBucket.CreateBucket(heightBucket); err != nil {
			return err
		}
		if _, err := rootBucket.CreateBucket(coinBucket); err != nil {
			return err
		}
		if _, err := rootBucket.CreateBucket(leafBucket); err != nil {
			return err
		}

		return nil
	})
}
