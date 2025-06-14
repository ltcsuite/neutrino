package filterdb

import (
	"fmt"

	"github.com/ltcsuite/ltcd/chaincfg"
	"github.com/ltcsuite/ltcd/chaincfg/chainhash"
	"github.com/ltcsuite/ltcd/ltcutil/gcs"
	"github.com/ltcsuite/ltcd/ltcutil/gcs/builder"
	"github.com/ltcsuite/ltcwallet/walletdb"
)

var (
	// filterBucket is the name of the root bucket for this package. Within
	// this bucket, sub-buckets are stored which themselves store the
	// actual filters.
	filterBucket = []byte("filter-store")

	// regBucket is the bucket that stores the regular filters.
	regBucket = []byte("regular")

	// ErrFilterNotFound is returned when a filter for a target block hash
	// is unable to be located.
	ErrFilterNotFound = fmt.Errorf("unable to find filter")
)

// FilterType is an enum-like type that represents the various filter types
// currently defined.
type FilterType uint8

const (
	// RegularFilter is the filter type of regular filters which contain
	// outputs and pkScript data pushes.
	RegularFilter FilterType = iota
)

// FilterData holds all the info about a filter required to store it.
type FilterData struct {
	// Filter is the actual filter to be stored.
	Filter *gcs.Filter

	// BlockHash is the block header hash of the block associated with the
	// Filter.
	BlockHash *chainhash.Hash

	// Type is the filter type.
	Type FilterType
}

// FilterDatabase is an interface which represents an object that is capable of
// storing and retrieving filters according to their corresponding block hash
// and also their filter type.
//
// TODO(roasbeef): similar interface for headerfs?
type FilterDatabase interface {
	// PutFilters stores a set of filters to persistent storage.
	PutFilters(...*FilterData) error

	// FetchFilter attempts to fetch a filter with the given hash and type
	// from persistent storage. In the case that a filter matching the
	// target block hash cannot be found, then ErrFilterNotFound is to be
	// returned.
	FetchFilter(*chainhash.Hash, FilterType) (*gcs.Filter, error)

	// PurgeFilters purge all filters with a given type from persistent
	// storage.
	PurgeFilters(FilterType) error
}

// FilterStore is an implementation of the FilterDatabase interface which is
// backed by boltdb.
type FilterStore struct {
	db walletdb.DB
}

// A compile-time check to ensure the FilterStore adheres to the FilterDatabase
// interface.
var _ FilterDatabase = (*FilterStore)(nil)

// New creates a new instance of the FilterStore given an already open
// database, and the target chain parameters.
func New(db walletdb.DB, params chaincfg.Params) (*FilterStore, error) {
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		// As part of our initial setup, we'll try to create the top
		// level filter bucket. If this already exists, then we can
		// exit early.
		filters, err := tx.CreateTopLevelBucket(filterBucket)
		if err != nil {
			return err
		}

		// If the main bucket doesn't already exist, then we'll need to
		// create the sub-buckets, and also initialize them with the
		// genesis filters.
		genesisBlock := params.GenesisBlock
		genesisHash := params.GenesisHash

		// First we'll create the bucket for the regular filters.
		regFilters, err := filters.CreateBucketIfNotExists(regBucket)
		if err != nil {
			return err
		}

		// With the bucket created, we'll now construct the initial
		// basic genesis filter and store it within the database.
		basicFilter, err := builder.BuildBasicFilter(genesisBlock, nil)
		if err != nil {
			return err
		}

		return putFilter(regFilters, genesisHash, basicFilter)
	})
	if err != nil && err != walletdb.ErrBucketExists {
		return nil, err
	}

	return &FilterStore{
		db: db,
	}, nil
}

// PurgeFilters purge all filters with a given type from persistent storage.
//
// NOTE: This method is a part of the FilterDatabase interface.
func (f *FilterStore) PurgeFilters(fType FilterType) error {
	return walletdb.Update(f.db, func(tx walletdb.ReadWriteTx) error {
		filters := tx.ReadWriteBucket(filterBucket)

		switch fType {
		case RegularFilter:
			err := filters.DeleteNestedBucket(regBucket)
			if err != nil {
				return err
			}

			_, err = filters.CreateBucket(regBucket)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown filter type: %v", fType)
		}

		return nil
	})
}

// putFilter stores a filter in the database according to the corresponding
// block hash. The passed bucket is expected to be the proper bucket for the
// passed filter type.
func putFilter(bucket walletdb.ReadWriteBucket, hash *chainhash.Hash,
	filter *gcs.Filter) error {

	if filter == nil {
		return bucket.Put(hash[:], nil)
	}

	bytes, err := filter.NBytes()
	if err != nil {
		return err
	}

	return bucket.Put(hash[:], bytes)
}

// PutFilters stores a set of filters to persistent storage.
//
// NOTE: This method is a part of the FilterDatabase interface.
func (f *FilterStore) PutFilters(filterList ...*FilterData) error {
	var updateErr error
	err := walletdb.Batch(f.db, func(tx walletdb.ReadWriteTx) error {
		filters := tx.ReadWriteBucket(filterBucket)
		regularFilterBkt := filters.NestedReadWriteBucket(regBucket)

		for _, filterData := range filterList {
			var targetBucket walletdb.ReadWriteBucket
			switch filterData.Type {
			case RegularFilter:
				targetBucket = regularFilterBkt
			default:
				updateErr = fmt.Errorf("unknown filter "+
					"type: %v", filterData.Type)

				return nil
			}

			err := putFilter(
				targetBucket, filterData.BlockHash,
				filterData.Filter,
			)
			if err != nil {
				return err
			}

			log.Tracef("Wrote filter for block %s, type %d",
				&filterData.BlockHash, filterData.Type)
		}

		return nil
	})
	if err != nil {
		return err
	}

	return updateErr
}

// FetchFilter attempts to fetch a filter with the given hash and type from
// persistent storage.
//
// NOTE: This method is a part of the FilterDatabase interface.
func (f *FilterStore) FetchFilter(blockHash *chainhash.Hash,
	filterType FilterType) (*gcs.Filter, error) {

	var filter *gcs.Filter

	err := walletdb.View(f.db, func(tx walletdb.ReadTx) error {
		filters := tx.ReadBucket(filterBucket)

		var targetBucket walletdb.ReadBucket
		switch filterType {
		case RegularFilter:
			targetBucket = filters.NestedReadBucket(regBucket)
		default:
			return fmt.Errorf("unknown filter type")
		}

		filterBytes := targetBucket.Get(blockHash[:])
		if filterBytes == nil {
			return ErrFilterNotFound
		}
		if len(filterBytes) == 0 {
			return nil
		}

		dbFilter, err := gcs.FromNBytes(
			builder.DefaultP, builder.DefaultM, filterBytes,
		)
		if err != nil {
			return err
		}

		filter = dbFilter
		return nil
	})
	if err != nil {
		return nil, err
	}

	return filter, nil
}
