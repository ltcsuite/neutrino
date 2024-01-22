package mwebdb

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/ltcsuite/ltcd/chaincfg/chainhash"
	"github.com/ltcsuite/ltcd/wire"
	"github.com/ltcsuite/ltcwallet/walletdb"
)

var (
	// rootBucket is the name of the root bucket for this package. Within
	// this bucket, sub-buckets are stored which themselves store the
	// actual coins.
	rootBucket = []byte("mweb-coindb")

	// coinBucket is the bucket that stores the coins.
	coinBucket = []byte("coins")
)

var (
	// ErrCoinNotFound is returned when a coin for an output ID is
	// unable to be located.
	ErrCoinNotFound = fmt.Errorf("unable to find coin")

	// ErrUnexpectedValueLen is returned when the bytes value is
	// of an unexpected length.
	ErrUnexpectedValueLen = fmt.Errorf("unexpected value length")
)

// CoinDatabase is an interface which represents an object that is capable of
// storing and retrieving coins according to their corresponding output ID.
type CoinDatabase interface {
	// Get and set the leafset marking the unspent indices.
	GetLeafSet() (leafset []byte, numLeaves uint64, err error)
	PutLeafSet(leafset []byte, numLeaves uint64) error

	// PutCoin stores a coin with the given output ID to persistent
	// storage.
	PutCoin(*chainhash.Hash, *wire.MwebOutput) error

	// PutCoins stores coins to persistent storage.
	PutCoins([]*wire.MwebOutput) error

	// FetchCoin attempts to fetch a coin with the given output ID
	// from persistent storage. In the case that a coin matching the
	// target output ID cannot be found, then ErrCoinNotFound is to be
	// returned.
	FetchCoin(*chainhash.Hash) (*wire.MwebOutput, error)

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

// New creates a new instance of the CoinStore given an already open
// database.
func New(db walletdb.DB) (*CoinStore, error) {
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
		_, err = rootBucket.CreateBucketIfNotExists(coinBucket)
		return err
	})
	if err != nil && err != walletdb.ErrBucketExists {
		return nil, err
	}

	return &CoinStore{db: db}, nil
}

func (c *CoinStore) GetLeafSet() (leafset []byte, numLeaves uint64, err error) {
	err = walletdb.View(c.db, func(tx walletdb.ReadTx) error {
		rootBucket := tx.ReadBucket(rootBucket)

		leafset = rootBucket.Get([]byte("leafset"))
		if leafset == nil {
			return nil
		}

		b := rootBucket.Get([]byte("numLeaves"))
		if b == nil {
			leafset = nil
			return nil
		}
		if len(b) != 8 {
			return ErrUnexpectedValueLen
		}
		numLeaves = binary.LittleEndian.Uint64(b)
		return nil
	})
	return
}

func (c *CoinStore) PutLeafSet(leafset []byte, numLeaves uint64) error {
	return walletdb.Update(c.db, func(tx walletdb.ReadWriteTx) error {
		rootBucket := tx.ReadWriteBucket(rootBucket)

		err := rootBucket.Put([]byte("leafset"), leafset)
		if err != nil {
			return err
		}

		b := binary.LittleEndian.AppendUint64(nil, numLeaves)
		return rootBucket.Put([]byte("numLeaves"), b)
	})
}

// PutCoin stores a coin with the given output ID to persistent
// storage.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) PutCoin(outputId *chainhash.Hash,
	coin *wire.MwebOutput) error {

	return walletdb.Update(c.db, func(tx walletdb.ReadWriteTx) error {
		rootBucket := tx.ReadWriteBucket(rootBucket)
		coinBucket := rootBucket.NestedReadWriteBucket(coinBucket)

		if coin == nil {
			return coinBucket.Put(outputId[:], nil)
		}

		var buf bytes.Buffer
		if err := coin.Serialize(&buf); err != nil {
			return err
		}

		return coinBucket.Put(outputId[:], buf.Bytes())
	})
}

// PutCoins stores coins to persistent storage.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) PutCoins(coins []*wire.MwebOutput) error {
	return walletdb.Update(c.db, func(tx walletdb.ReadWriteTx) error {
		rootBucket := tx.ReadWriteBucket(rootBucket)
		coinBucket := rootBucket.NestedReadWriteBucket(coinBucket)

		var buf bytes.Buffer
		for _, coin := range coins {
			if err := coin.Serialize(&buf); err != nil {
				return err
			}

			outputId := coin.Hash()
			err := coinBucket.Put(outputId[:], buf.Bytes())
			if err != nil {
				return err
			}
			buf.Reset()
		}

		return nil
	})
}

// FetchCoin attempts to fetch a coin with the given output ID from
// persistent storage.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) FetchCoin(outputId *chainhash.Hash) (*wire.MwebOutput, error) {
	var coin *wire.MwebOutput

	err := walletdb.View(c.db, func(tx walletdb.ReadTx) error {
		rootBucket := tx.ReadBucket(rootBucket)
		coinBucket := rootBucket.NestedReadBucket(coinBucket)

		coinBytes := coinBucket.Get(outputId[:])
		if coinBytes == nil {
			return ErrCoinNotFound
		}
		if len(coinBytes) == 0 {
			return nil
		}

		coin = new(wire.MwebOutput)
		return coin.Deserialize(bytes.NewBuffer(coinBytes))
	})
	if err != nil {
		return nil, err
	}

	return coin, nil
}

// PurgeCoins purges all coins from persistent storage.
//
// NOTE: This method is a part of the CoinDatabase interface.
func (c *CoinStore) PurgeCoins() error {
	return walletdb.Update(c.db, func(tx walletdb.ReadWriteTx) error {
		rootBucket := tx.ReadWriteBucket(rootBucket)

		if err := rootBucket.DeleteNestedBucket(coinBucket); err != nil {
			return err
		}
		if _, err := rootBucket.CreateBucket(coinBucket); err != nil {
			return err
		}

		return nil
	})
}
