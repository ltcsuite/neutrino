package neutrino

import (
	"sync"

	"github.com/ltcmweb/ltcd/chaincfg/chainhash"
	"github.com/ltcmweb/ltcd/ltcutil"
)

// Mempool is used when we are downloading unconfirmed transactions.
// We will use this object to track which transactions we've already
// downloaded so that we don't download them more than once.
type Mempool struct {
	blockManager  *blockManager
	downloadedTxs map[chainhash.Hash]bool
	mtx           sync.RWMutex
	callbacks     []func(*ltcutil.Tx)
	watchedAddrs  []ltcutil.Address
}

// NewMempool returns an initialized mempool
func NewMempool() *Mempool {
	return &Mempool{
		downloadedTxs: make(map[chainhash.Hash]bool),
		mtx:           sync.RWMutex{},
	}
}

// RegisterCallback will register a callback that will fire when a transaction
// matching a watched address enters the mempool.
func (mp *Mempool) RegisterCallback(onRecvTx func(*ltcutil.Tx)) {
	mp.mtx.Lock()
	defer mp.mtx.Unlock()
	mp.callbacks = append(mp.callbacks, onRecvTx)
}

// HaveTransaction returns whether or not the passed transaction already exists
// in the mempool.
func (mp *Mempool) HaveTransaction(hash *chainhash.Hash) bool {
	mp.mtx.RLock()
	defer mp.mtx.RUnlock()
	return mp.downloadedTxs[*hash]
}

// AddTransaction adds a new transaction to the mempool and
// maybe calls back if it matches any watched addresses.
func (mp *Mempool) AddTransaction(tx *ltcutil.Tx) {
	mp.mtx.Lock()
	mp.downloadedTxs[*tx.Hash()] = true

	ro := defaultRescanOptions()
	WatchAddrs(mp.watchedAddrs...)(ro)
	callbacks := mp.callbacks
	mp.mtx.Unlock()

	if ok, err := ro.paysWatchedAddr(tx); ok && err == nil {
		for _, cb := range callbacks {
			cb(tx)
		}
	}

	if mw := tx.MsgTx().Mweb; mw != nil {
		mp.blockManager.notifyMwebUtxos(mw.TxBody.Outputs)
	}
}

// Clear will remove all transactions from the mempool. This
// should be done whenever a new block is accepted.
func (mp *Mempool) Clear() {
	mp.mtx.Lock()
	defer mp.mtx.Unlock()
	mp.downloadedTxs = make(map[chainhash.Hash]bool)
}

// NotifyReceived stores addresses to watch
func (mp *Mempool) NotifyReceived(addrs []ltcutil.Address) {
	mp.mtx.Lock()
	defer mp.mtx.Unlock()
	mp.watchedAddrs = append(mp.watchedAddrs, addrs...)
}
