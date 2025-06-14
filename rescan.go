// NOTE: THIS API IS UNSTABLE RIGHT NOW.

package neutrino

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ltcsuite/ltcd/btcjson"
	"github.com/ltcsuite/ltcd/chaincfg"
	"github.com/ltcsuite/ltcd/chaincfg/chainhash"
	"github.com/ltcsuite/ltcd/ltcutil"
	"github.com/ltcsuite/ltcd/ltcutil/gcs"
	"github.com/ltcsuite/ltcd/ltcutil/gcs/builder"
	"github.com/ltcsuite/ltcd/rpcclient"
	"github.com/ltcsuite/ltcd/txscript"
	"github.com/ltcsuite/ltcd/wire"
	"github.com/ltcsuite/neutrino/blockntfns"
	"github.com/ltcsuite/neutrino/headerfs"
)

var (
	// zeroOutPoint indicates that we should match on an output's script
	// when dispatching a spend notification.
	zeroOutPoint wire.OutPoint

	// ErrRescanExit is an error returned to the caller in case the ongoing
	// rescan exits.
	ErrRescanExit = errors.New("rescan exited")

	// errRetryBlock is an internal error used to signal to the rescan
	// should it should attempt to retry processing a block.
	errRetryBlock = errors.New("block must be retried")
)

// ChainSource is an interface that's in charge of retrieving information about
// the existing chain.
type ChainSource interface {
	// ChainParams returns the parameters of the current chain.
	ChainParams() chaincfg.Params

	// BestBlock retrieves the most recent block's height and hash where we
	// have both the header and filter header ready.
	BestBlock() (*headerfs.BlockStamp, error)

	// GetBlockHeaderByHeight returns the header of the block with the given
	// height.
	GetBlockHeaderByHeight(uint32) (*wire.BlockHeader, error)

	// GetBlockHeader returns the header of the block with the given hash.
	GetBlockHeader(*chainhash.Hash) (*wire.BlockHeader, uint32, error)

	// GetBlock returns the block with the given hash.
	GetBlock(chainhash.Hash, ...QueryOption) (*ltcutil.Block, error)

	// GetFilterHeaderByHeight returns the filter header of the block with
	// the given height.
	GetFilterHeaderByHeight(uint32) (*chainhash.Hash, error)

	// GetCFilter returns the filter of the given type for the block with
	// the given hash.
	GetCFilter(chainhash.Hash, wire.FilterType,
		...QueryOption) (*gcs.Filter, error)

	// Subscribe returns a block subscription that delivers block
	// notifications in order. The bestHeight parameter can be used to
	// signal that a backlog of notifications should be delivered from this
	// height. When providing a bestHeight of 0, a backlog will not be
	// delivered.
	//
	// TODO(wilmer): extend with best hash as well.
	Subscribe(bestHeight uint32) (*blockntfns.Subscription, error)

	// IsCurrent returns true if the backend chain thinks that its view of
	// the network is current.
	IsCurrent() bool
}

// ScanProgressHandler is used in rescanOptions to update the caller with the
// rescan progress.
type ScanProgressHandler func(lastProcessedBlock uint32)

// rescanOptions holds the set of functional parameters for Rescan.
type rescanOptions struct {
	queryOptions []QueryOption

	ntfn            rpcclient.NotificationHandlers
	progressHandler ScanProgressHandler

	startTime  time.Time
	startBlock *headerfs.BlockStamp

	endBlock *headerfs.BlockStamp

	watchAddrs  []ltcutil.Address
	watchInputs []InputWithScript
	watchList   [][]byte
	txIdx       uint32

	update <-chan *updateOptions
	quit   <-chan struct{}
}

// RescanOption is a functional option argument to any of the rescan and
// notification subscription methods. These are always processed in order, with
// later options overriding earlier ones.
type RescanOption func(ro *rescanOptions)

func defaultRescanOptions() *rescanOptions {
	return &rescanOptions{}
}

// QueryOptions pass onto the underlying queries.
func QueryOptions(options ...QueryOption) RescanOption {
	return func(ro *rescanOptions) {
		ro.queryOptions = options
	}
}

// NotificationHandlers specifies notification handlers for the rescan. These
// will always run in the same goroutine as the caller.
func NotificationHandlers(ntfn rpcclient.NotificationHandlers) RescanOption {
	return func(ro *rescanOptions) {
		ro.ntfn = ntfn
	}
}

// ProgressHandler specifies a handler to be used when the utxo
// scanner reports its progress.
// The passed handler should be non-blocking for the rescan to continue
// normally.
func ProgressHandler(
	handler ScanProgressHandler) RescanOption {

	return func(ro *rescanOptions) {
		ro.progressHandler = handler
	}
}

// StartBlock specifies the start block. The hash is checked first; if there's
// no such hash (zero hash avoids lookup), the height is checked next. If the
// height is 0 or the start block isn't specified, starts from the genesis
// block. This block is assumed to already be known, and no notifications will
// be sent for this block. The rescan uses the latter of StartBlock and
// StartTime.
func StartBlock(startBlock *headerfs.BlockStamp) RescanOption {
	return func(ro *rescanOptions) {
		ro.startBlock = startBlock
	}
}

// StartTime specifies the start time. The time is compared to the timestamp of
// each block, and the rescan only begins once the first block crosses that
// timestamp. When using this, it is advisable to use a margin of error and
// start rescans slightly earlier than required. The rescan uses the latter of
// StartBlock and StartTime.
func StartTime(startTime time.Time) RescanOption {
	return func(ro *rescanOptions) {
		ro.startTime = startTime
	}
}

// EndBlock specifies the end block. The hash is checked first; if there's no
// such hash (zero hash avoids lookup), the height is checked next. If the
// height is 0 or in the future or the end block isn't specified, the quit
// channel MUST be specified as Rescan will sync to the tip of the blockchain
// and continue to stay in sync and pass notifications. This is enforced at
// runtime.
func EndBlock(endBlock *headerfs.BlockStamp) RescanOption {
	return func(ro *rescanOptions) {
		ro.endBlock = endBlock
	}
}

// WatchAddrs specifies the addresses to watch/filter for. Each call to this
// function adds to the list of addresses being watched rather than replacing
// the list. Each time a transaction spends to the specified address, the
// outpoint is added to the WatchOutPoints list.
func WatchAddrs(watchAddrs ...ltcutil.Address) RescanOption {
	return func(ro *rescanOptions) {
		ro.watchAddrs = append(ro.watchAddrs, watchAddrs...)
	}
}

// InputWithScript couples an previous outpoint along with its input script.
// We'll use the prev script to match the filter itself, but then scan for the
// particular outpoint when we need to make a notification decision.
type InputWithScript struct {
	// OutPoint identifies the previous output to watch.
	OutPoint wire.OutPoint

	// PkScript is the script of the previous output.
	PkScript []byte
}

// WatchInputs specifies the outpoints to watch for on-chain spends. We also
// require the script as we'll match on the script, but then notify based on
// the outpoint. Each call to this function adds to the list of outpoints being
// watched rather than replacing the list.
func WatchInputs(watchInputs ...InputWithScript) RescanOption {
	return func(ro *rescanOptions) {
		ro.watchInputs = append(ro.watchInputs, watchInputs...)
	}
}

// TxIdx specifies a hint transaction index into the block in which the UTXO is
// created (eg, coinbase is 0, next transaction is 1, etc.)
func TxIdx(txIdx uint32) RescanOption {
	return func(ro *rescanOptions) {
		ro.txIdx = txIdx
	}
}

// QuitChan specifies the quit channel. This can be used by the caller to let
// an indefinite rescan (one with no EndBlock set) know it should gracefully
// shut down. If this isn't specified, an end block MUST be specified as Rescan
// must know when to stop. This is enforced at runtime.
func QuitChan(quit <-chan struct{}) RescanOption {
	return func(ro *rescanOptions) {
		ro.quit = quit
	}
}

// blockRetryQueue is a helper struct that maintains a queue of blocks for which
// we need to fetch filters.
type blockRetryQueue struct {
	blocks []*blockntfns.Connected
}

// newBlockRetryQueue constructs a new, empty retry block queue.
func newBlockRetryQueue() *blockRetryQueue {
	return &blockRetryQueue{}
}

// push enqueues a block at the end of the queue.
func (q *blockRetryQueue) push(block *blockntfns.Connected) {
	q.blocks = append(q.blocks, block)
}

// peek returns the next block to be retried but doesn't consume it.
func (q *blockRetryQueue) peek() *blockntfns.Connected {
	if len(q.blocks) == 0 {
		return nil
	}
	return q.blocks[0]
}

// pop returns and consumes the next block to be retried.
func (q *blockRetryQueue) pop() *blockntfns.Connected {
	if len(q.blocks) == 0 {
		return nil
	}
	block := q.blocks[0]
	q.blocks[0] = nil
	q.blocks = q.blocks[1:]
	return block
}

// remove removes the block from the queue and any others that follow it. If the
// block doesn't exit within the queue, then this acts as a NOP.
func (q *blockRetryQueue) remove(header wire.BlockHeader) {
	headerIdx := -1
	targetHash := header.BlockHash()
	for i, block := range q.blocks {
		blockHeader := block.Header()
		if blockHeader.BlockHash() == targetHash {
			headerIdx = i
			break
		}
	}
	if headerIdx != -1 {
		for i := headerIdx; i < len(q.blocks); i++ {
			q.blocks[i] = nil
		}
		q.blocks = q.blocks[:headerIdx]
	}
}

// clear clears the queue.
func (q *blockRetryQueue) clear() {
	q.blocks = nil
}

// updateChan specifies an update channel. This is for internal use by the
// Rescan.Update functionality.
func updateChan(update <-chan *updateOptions) RescanOption {
	return func(ro *rescanOptions) {
		ro.update = update
	}
}

// rescanState hold the state used throughout a rescan.
type rescanState struct {
	// chain is the backend chain that the rescan has access to.
	chain ChainSource

	// opts holds the various rescan configuration options.
	opts *rescanOptions

	// curHeader is the block header of our current position in the chain.
	curHeader wire.BlockHeader

	// curStamp is the block stamp of our current position in the chain.
	curStamp headerfs.BlockStamp

	// scanning is true if the current block should be scanned for filter
	// matches.
	scanning bool
}

// newRescanState constructs a new rescanState.
func newRescanState(chain ChainSource, options ...RescanOption) (*rescanState,
	error) {

	// First, we'll apply the set of default options, then serially apply
	// all the options that've been passed in.
	ro := defaultRescanOptions()
	ro.endBlock = &headerfs.BlockStamp{
		Hash:   chainhash.Hash{},
		Height: 0,
	}
	for _, option := range options {
		option(ro)
	}

	rs := &rescanState{
		chain: chain,
		opts:  ro,
	}

	// If we have something to watch, create a watch list. The watch list
	// can be composed of a set of scripts, outpoints, and txids.
	for _, addr := range ro.watchAddrs {
		script, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, err
		}

		ro.watchList = append(ro.watchList, script)
	}
	for _, input := range ro.watchInputs {
		ro.watchList = append(ro.watchList, input.PkScript)
	}

	// Check that we have either an end block or a quit channel.
	if ro.endBlock != nil {
		// If the end block hash is non-nil, then we'll query the
		// database to find out the stop height.
		if (ro.endBlock.Hash != chainhash.Hash{}) {
			_, height, err := chain.GetBlockHeader(
				&ro.endBlock.Hash,
			)
			if err != nil {
				ro.endBlock.Hash = chainhash.Hash{}
			} else {
				ro.endBlock.Height = int32(height)
			}
		}

		// If the ending hash it nil, then check to see if the target
		// height is non-nil. If not, then we'll use this to find the
		// stopping hash.
		if (ro.endBlock.Hash == chainhash.Hash{}) {
			if ro.endBlock.Height != 0 {
				header, err := chain.GetBlockHeaderByHeight(
					uint32(ro.endBlock.Height),
				)
				if err == nil {
					ro.endBlock.Hash = header.BlockHash()
				} else {
					ro.endBlock = &headerfs.BlockStamp{}
				}
			}
		}
	} else {
		ro.endBlock = &headerfs.BlockStamp{}
	}

	// If we don't have a quit channel, and the end height is still
	// unspecified, then we'll exit out here.
	if ro.quit == nil && ro.endBlock.Height == 0 {
		return nil, fmt.Errorf("rescan request must specify a quit " +
			"channel or valid end block")
	}

	// If no start block is specified, start the scan from our current best
	// block.
	if ro.startBlock == nil {
		bs, err := chain.BestBlock()
		if err != nil {
			return nil, err
		}
		ro.startBlock = bs
	}
	rs.curStamp = *ro.startBlock

	// To find our starting block, either the start hash should be set, or
	// the start height should be set. If neither is, then we'll be
	// starting from the genesis block.
	if (rs.curStamp.Hash != chainhash.Hash{}) {
		header, height, err := chain.GetBlockHeader(&rs.curStamp.Hash)
		if err == nil {
			rs.curHeader = *header
			rs.curStamp.Height = int32(height)
		} else {
			rs.curStamp.Hash = chainhash.Hash{}
		}
	}
	if (rs.curStamp.Hash == chainhash.Hash{}) {
		if rs.curStamp.Height == 0 {
			rs.curStamp.Hash = *chain.ChainParams().GenesisHash
		} else {
			header, err := chain.GetBlockHeaderByHeight(
				uint32(rs.curStamp.Height),
			)
			if err == nil {
				rs.curHeader = *header
				rs.curStamp.Hash = rs.curHeader.BlockHash()
			} else {
				chainParams := chain.ChainParams()
				rs.curHeader = chainParams.GenesisBlock.Header
				rs.curStamp.Hash = *chainParams.GenesisHash
				rs.curStamp.Height = 0
			}
		}
	}

	return rs, nil
}

// rescan is a single-threaded function that uses headers from the database and
// functional options as arguments.
func (rs *rescanState) rescan() error {
	chain := rs.chain
	ro := rs.opts

	// We'll need to ensure that the backing chain has actually caught up to
	// the rescan's starting height.
	if err := rs.waitForBlocks(func(_ chainhash.Hash, height uint32) bool {
		return height >= uint32(rs.curStamp.Height)
	}); err != nil {
		return err
	}

	// To ensure that we batch as many filter queries as possible, we also
	// wait for the header chain to either be current or for it to at least
	// have caught up with the specified end block.
	log.Debugf("Waiting for the chain source to be current or for the " +
		"rescan end height to be reached.")

	if err := rs.waitForBlocks(func(hash chainhash.Hash,
		height uint32) bool {

		// If the header chain is current, then there is no need to
		// wait.
		if chain.IsCurrent() {
			return true
		}

		// If an end height was specified then we wait until the
		// notification corresponding to that block height.
		if ro.endBlock.Height > 0 &&
			height >= uint32(ro.endBlock.Height) {

			return true
		}

		// If a block hash was specified, check if the notification is
		// for that block.
		return hash == ro.endBlock.Hash
	}); err != nil {
		return err
	}

	log.Debugf("Starting rescan from known block %d (%s)",
		rs.curStamp.Height, rs.curStamp.Hash)

	// Compare the start time to the start block. If the start time is
	// later, cycle through blocks until we find a block timestamp later
	// than the start time, and begin filter download at that block. Since
	// time is non-monotonic between blocks, we look for the first block to
	// trip the switch, and download filters from there, rather than
	// checking timestamps at each block.
	rs.scanning = ro.startTime.Before(rs.curHeader.Timestamp)

	// Even though we'll have multiple subscriptions, they'll always be
	// referred to by the same variable, so we only need to defer its
	// cancellation once at the end. Any intermediate subscriptions should
	// be properly canceled before registering a new one.
	var blockSubscription *blockntfns.Subscription
	defer func() {
		if blockSubscription != nil {
			blockSubscription.Cancel()
			blockSubscription = nil
		}
	}()

	var (
		blockRetrySignal <-chan time.Time
		// blockRetryInterval is the interval in which we'll continually
		// retry to fetch the latest filter from our peers.
		//
		// TODO(roasbeef): add exponential back-off
		blockRetryInterval = time.Millisecond * 100
		blockRetryQueue    = newBlockRetryQueue()
	)

	// We'll need to keep track of whether we are current with the chain in
	// order to properly recover from a re-org. We'll start by assuming that
	// we are not current in order to catch up from the starting point to
	// the tip of the chain.
	current := false

	// Loop through blocks, one at a time. This relies on the underlying
	// chain source to deliver notifications in the correct order.
rescanLoop:
	for {
		// If we've reached the ending height or hash for this rescan,
		// then we'll exit.
		if rs.curStamp.Hash == ro.endBlock.Hash ||
			(ro.endBlock.Height > 0 &&
				rs.curStamp.Height == ro.endBlock.Height) {

			return nil
		}

		// If we're current, we wait for notifications that will be
		// delivered each time a block is connecting, disconnecting, or
		// we can an update to the filter we should be looking for.
		switch current {
		case true:
			// Wait for a signal that we have a newly connected
			// header and cfheader, or a newly disconnected header;
			// alternatively, forward ourselves to the next block
			// if possible.
			select {
			case <-ro.quit:
				return ErrRescanExit

			// An update mesage has just come across, if it points
			// to a prior point in the chain, then we may need to
			// rewind a bit in order to provide the client all its
			// requested client.
			case update := <-ro.update:
				rewound, err := ro.updateFilter(
					chain, update, &rs.curStamp,
					&rs.curHeader,
				)
				if err != nil {
					return err
				}

				// If we have to rewind our state, then we'll
				// mark ourselves as not current so we can walk
				// forward in the chain again until we are
				// current. This is our way of doing a manual
				// rescan.
				if rewound {
					log.Tracef("Rewound to block %d (%s), "+
						"no longer current",
						rs.curStamp.Height,
						rs.curStamp.Hash)

					current = false
					blockSubscription.Cancel()
					blockSubscription = nil
				}

			case ntfn, ok := <-blockSubscription.Notifications:
				if !ok {
					return errors.New("rescan block " +
						"subscription was canceled")
				}

				switch ntfn := ntfn.(type) {
				case *blockntfns.Connected:
					// If we have any blocks to retry, we'll
					// defer processing this notification
					// until later.
					if blockRetryQueue.peek() != nil {
						log.Debugf("Stashing %v", ntfn)
						blockRetryQueue.push(ntfn)
						continue rescanLoop
					}

					err := rs.handleBlockConnected(ntfn)
					switch err {
					case nil:

					// We'll need to retry the block again
					// as we couldn't fetch its filter.
					case errRetryBlock:
						log.Debugf("Retrying %v after %v",
							ntfn, blockRetryInterval)
						blockRetryQueue.push(ntfn)
						blockRetrySignal = time.After(
							blockRetryInterval,
						)

					// Since we weren't able to successfully
					// process the block, we'll set
					// ourselves to not be current in order
					// to attempt catching up with the chain
					// ourselves.
					//
					// TODO(wilmer): determine if the error
					// is fatal and return it?
					default:
						log.Errorf("Unable to process "+
							"%v: %v", ntfn, err)
						current = false
					}

				case *blockntfns.Disconnected:
					// Check whether the block being
					// disconnected is one for which we've
					// queued up to retry. If it is, we'll
					// remove it and any others that follow
					// as they are now considered stale.
					blockRetryQueue.remove(ntfn.Header())

					rs.handleBlockDisconnected(ntfn)

				default:
					log.Warnf("Received unhandled block "+
						"notification: %T", ntfn)
				}

			// Our retry signal has fired, so we'll attempt to
			// refetch and notify the filter for our queued blocks.
			case <-blockRetrySignal:
				blockRetrySignal = nil

				// We'll go through all of our retry blocks in
				// order unless we need to retry any of them.
			retryLoop:
				for {
					retryBlock := blockRetryQueue.peek()
					if retryBlock == nil {
						continue rescanLoop
					}

					err := rs.handleBlockConnected(
						retryBlock,
					)
					switch err {
					// We successfully notified the block
					// this time, so we can remove it from
					// our queue and move on to the next.
					case nil:
						_ = blockRetryQueue.pop()
						continue retryLoop

					// We'll need to retry the block again
					// as we couldn't fetch its filter.
					case errRetryBlock:
						log.Debugf("Retrying %v after "+
							"%v", retryBlock,
							blockRetryInterval)
						blockRetrySignal = time.After(
							blockRetryInterval,
						)
						continue rescanLoop

					// Since we weren't able to successfully
					// process the block, we'll set
					// ourselves to not be current in order
					// to attempt catching up with the chain
					// ourselves.
					//
					// TODO(wilmer): determine if the error
					// is fatal and return it?
					default:
						log.Errorf("Unable to process "+
							"retry of %v: %v",
							retryBlock, err)
						current = false
						continue rescanLoop
					}
				}
			}

		// If we're not yet current, then we'll walk down the chain
		// until we reach the tip of the chain as we know it. At this
		// point, we'll be "current" again.
		case false:
			select {
			case <-ro.quit:
				return ErrRescanExit
			default:
			}

			// Apply all queued filter updates.
		updateFilterLoop:
			for {
				select {
				case update := <-ro.update:
					_, err := ro.updateFilter(
						chain, update, &rs.curStamp,
						&rs.curHeader,
					)
					if err != nil {
						return err
					}

				default:
					break updateFilterLoop
				}
			}

			bestBlock, err := chain.BestBlock()
			if err != nil {
				return err
			}

			// Since we're not current, we try to manually advance
			// the block. If the next height is above the best
			// height known to the chain service, then we mark
			// ourselves as current and follow notifications.
			nextHeight := rs.curStamp.Height + 1
			if nextHeight > bestBlock.Height {
				// Ensure we cancel the old subscription if
				// we're going back to scan for missed blocks.
				if blockSubscription != nil {
					blockSubscription.Cancel()
				}

				// Subscribe to block notifications.
				blockSubscription, err = chain.Subscribe(
					uint32(rs.curStamp.Height),
				)
				if err != nil {
					return fmt.Errorf("unable to register "+
						"block subscription: %v", err)
				}

				log.Debugf("Rescan became current at %d (%s), "+
					"subscribing to block notifications",
					rs.curStamp.Height, rs.curStamp.Hash)

				current = true
				blockRetryQueue.clear()

				continue rescanLoop
			}

			// If the next height is known to the chain service,
			// then we'll fetch the next block and send a
			// notification, maybe also scanning the filters for
			// the block.
			header, err := chain.GetBlockHeaderByHeight(
				uint32(nextHeight),
			)
			if err != nil {
				return err
			}

			rs.curHeader = *header
			rs.curStamp.Height++
			rs.curStamp.Hash = header.BlockHash()

			if !rs.scanning {
				rs.scanning = ro.startTime.Before(
					rs.curHeader.Timestamp,
				)
			}

			err = rs.notifyBlock()
			if err != nil {
				return err
			}
		}
	}
}

// waitForBlocks is a helper function that can be used to wait on block
// notifications until the given predicate returns true.
func (rs *rescanState) waitForBlocks(predicate func(hash chainhash.Hash,
	height uint32) bool) error {

	chain := rs.chain
	ro := rs.opts

	bestBlock, err := chain.BestBlock()
	if err != nil {
		return err
	}

	// Before subscribing to block notifications, first check if the
	// predicate is not already satisfied by the current best block.
	if predicate(bestBlock.Hash, uint32(bestBlock.Height)) {
		return nil
	}

	log.Debugf("Waiting to catch up to the rescan start height=%d "+
		"from height=%d", rs.curStamp.Height, bestBlock.Height)

	blockSubscription, err := chain.Subscribe(uint32(bestBlock.Height))
	if err != nil {
		return err
	}
	defer blockSubscription.Cancel()

	var updates []*updateOptions

waitUntilSynced:
	for {
		select {
		// We'll make sure to process any updates while we're syncing to
		// prevent blocking the client.
		case update := <-ro.update:
			updates = append(updates, update)

		// A new block notification for the tip of the chain has
		// arrived. We'll determine we've caught up to the rescan's
		// starting height by receiving a block connected notification
		// for the same height.
		case ntfn, ok := <-blockSubscription.Notifications:
			if !ok {
				return errors.New("rescan block subscription " +
					"was canceled while waiting to catch " +
					"up")
			}
			cNtfn, ok := ntfn.(*blockntfns.Connected)
			if !ok {
				continue
			}

			header := cNtfn.Header()
			if predicate(header.BlockHash(), cNtfn.Height()) {
				break waitUntilSynced
			}

		case <-ro.quit:
			return ErrRescanExit
		}

		// If any updates were queued while waiting to catch up to the
		// start height of the rescan, apply them now.
		for _, upd := range updates {
			_, err := ro.updateFilter(
				chain, upd, &rs.curStamp, &rs.curHeader,
			)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// notifyBlock calls appropriate listeners based on the block filter.
func (rs *rescanState) notifyBlock() error {
	chain := rs.chain
	ro := rs.opts

	// Find relevant transactions based on watch list. If scanning is
	// false, we can safely assume this block has no relevant transactions.
	var relevantTxs []*ltcutil.Tx
	if len(ro.watchList) != 0 && rs.scanning {
		// If we have a non-empty watch list, then we need to see if it
		// matches the rescan's filters, so we get the basic filter
		// from the DB or network.
		matched, filter, err := blockFilterMatches(
			chain, ro, &rs.curStamp.Hash,
		)
		if err != nil {
			return err
		}

		if matched {
			relevantTxs, err = extractBlockMatches(
				chain, ro, &rs.curStamp, filter,
			)
			if err != nil {
				return err
			}
		}
	}

	if ro.ntfn.OnFilteredBlockConnected != nil {
		ro.ntfn.OnFilteredBlockConnected(
			rs.curStamp.Height, &rs.curHeader, relevantTxs,
		)
	}

	if ro.ntfn.OnBlockConnected != nil { // nolint:staticcheck
		ro.ntfn.OnBlockConnected( // nolint:staticcheck
			&rs.curStamp.Hash, rs.curStamp.Height,
			rs.curHeader.Timestamp,
		)
	}

	return nil
}

// handleBlockConnected handles a new block connected notification.
func (rs *rescanState) handleBlockConnected(ntfn *blockntfns.Connected) error {
	chain := rs.chain
	ro := rs.opts

	// If we've somehow missed a header in the range, then we'll mark
	// ourselves as not current so we can walk down the chain and notify the
	// callers of blocks we may have missed.
	header := ntfn.Header()
	if header.PrevBlock != rs.curStamp.Hash {
		return fmt.Errorf("out of order block %v: expected PrevBlock "+
			"%v, got %v", header.BlockHash(), rs.curStamp.Hash,
			header.PrevBlock)
	}

	// Ensure the filter header still exists before attempting to fetch the
	// filter. This should usually succeed since notifications are delivered
	// once filter headers are synced.
	nextBlockHeight := uint32(rs.curStamp.Height + 1)
	_, err := chain.GetFilterHeaderByHeight(nextBlockHeight)
	if err != nil {
		return fmt.Errorf("unable to get filter header for new block "+
			"with height %v: %v", nextBlockHeight, err)
	}

	newStamp := headerfs.BlockStamp{
		Hash:      header.BlockHash(),
		Height:    int32(nextBlockHeight),
		Timestamp: header.Timestamp,
	}

	log.Tracef("Rescan got block %d (%s)", newStamp.Height, newStamp.Hash)

	// We're only scanning if the header is beyond the horizon of
	// our start time.
	if !rs.scanning {
		rs.scanning = ro.startTime.Before(header.Timestamp)
	}

	// If we're not scanning or our watch list is empty, then we can just
	// notify the block without fetching any filters/blocks.
	if !rs.scanning || len(ro.watchList) == 0 {
		if ro.ntfn.OnFilteredBlockConnected != nil {
			ro.ntfn.OnFilteredBlockConnected(
				newStamp.Height, &header, nil,
			)
		}
		if ro.ntfn.OnBlockConnected != nil { // nolint:staticcheck
			ro.ntfn.OnBlockConnected( // nolint:staticcheck
				&newStamp.Hash, newStamp.Height,
				header.Timestamp,
			)
		}

		rs.curHeader = header
		rs.curStamp = newStamp

		return nil
	}

	// Otherwise, we'll attempt to fetch the filter to retrieve the relevant
	// transactions and notify them.
	queryOptions := NumRetries(0)
	blockFilter, err := chain.GetCFilter(
		newStamp.Hash, wire.GCSFilterRegular, queryOptions,
	)
	if err != nil {
		// If the query failed, then this either means that we don't
		// have any peers to fetch this filter from, or the peer(s) that
		// we're trying to fetch from are in the progress of a re-org.
		log.Errorf("unable to get filter for hash=%v, retrying: %v",
			rs.curStamp.Hash, err)

		return errRetryBlock
	}

	err = rs.notifyBlockWithFilter(&header, &newStamp, blockFilter)
	if err != nil {
		return err
	}

	// With the block successfully notified, we'll advance our state to it.
	rs.curHeader = header
	rs.curStamp = newStamp

	return nil
}

// extractBlockMatches fetches the target block from the network, and filters
// out any relevant transactions found within the block.
func extractBlockMatches(chain ChainSource, ro *rescanOptions,
	curStamp *headerfs.BlockStamp, filter *gcs.Filter) ([]*ltcutil.Tx,
	error) {

	// We've matched. Now we actually get the block and cycle through the
	// transactions to see which ones are relevant.
	block, err := chain.GetBlock(curStamp.Hash, ro.queryOptions...)
	if err != nil {
		return nil, err
	}
	if block == nil {
		return nil, fmt.Errorf("couldn't get block %d (%s) from "+
			"network", curStamp.Height, curStamp.Hash)
	}

	// Before we go through the transactions, let's make sure the filter we
	// got from our peer is valid and includes all spent previous output
	// scripts. If there's a problem, the error returned here will be
	// interpreted by the block manager to disconnect/ban said peer.
	if _, err := VerifyBasicBlockFilter(filter, block); err != nil {
		return nil, fmt.Errorf("error verifying filter against "+
			"downloaded block %d (%s), possibly got invalid "+
			"filter from peer: %v", curStamp.Height, curStamp.Hash,
			err)
	}

	blockHeader := block.MsgBlock().Header
	blockDetails := btcjson.BlockDetails{
		Height: block.Height(),
		Hash:   block.Hash().String(),
		Time:   blockHeader.Timestamp.Unix(),
	}

	relevantTxs := make([]*ltcutil.Tx, 0, len(block.Transactions()))
	for txIdx, tx := range block.Transactions() {
		txDetails := blockDetails
		txDetails.Index = txIdx

		var relevant bool

		if ro.spendsWatchedInput(tx) {
			relevant = true
			if ro.ntfn.OnRedeemingTx != nil { // nolint:staticcheck
				ro.ntfn.OnRedeemingTx(tx, &txDetails) // nolint:staticcheck
			}
		}

		// Even though the transaction may already be known as relevant
		// and there might not be a notification callback, we need to
		// call paysWatchedAddr anyway as it updates the rescan
		// options.
		pays, err := ro.paysWatchedAddr(tx)
		if err != nil {
			return nil, err
		}

		if pays {
			relevant = true
			if ro.ntfn.OnRecvTx != nil { // nolint:staticcheck
				ro.ntfn.OnRecvTx(tx, &txDetails) // nolint:staticcheck
			}
		}

		if relevant {
			relevantTxs = append(relevantTxs, tx)

			chainSource, ok := chain.(*RescanChainSource)
			if ok {
				chainSource.broadcaster.MarkAsConfirmed(
					*tx.Hash(),
				)
			}
		}
	}

	return relevantTxs, nil
}

// handleBlockDisconnected handles a new block disconnected notification.
func (rs *rescanState) handleBlockDisconnected(ntfn *blockntfns.Disconnected) {
	ro := rs.opts

	blockDisconnected := ntfn.Header()
	log.Debugf("Rescan got disconnected block %d (%s)", ntfn.Height(),
		blockDisconnected.BlockHash())

	// Only deal with it if it's the current block we know about. Otherwise,
	// it's in the future.
	if blockDisconnected.BlockHash() != rs.curStamp.Hash {
		return
	}

	// Run through notifications. This is all single-threaded. We include
	// deprecated calls as they're still used, for now.
	if ro.ntfn.OnFilteredBlockDisconnected != nil {
		ro.ntfn.OnFilteredBlockDisconnected(
			rs.curStamp.Height, &rs.curHeader,
		)
	}
	if ro.ntfn.OnBlockDisconnected != nil { // nolint:staticcheck
		ro.ntfn.OnBlockDisconnected( // nolint:staticcheck
			&rs.curStamp.Hash, rs.curStamp.Height,
			rs.curHeader.Timestamp,
		)
	}

	rs.curHeader = ntfn.ChainTip()
	rs.curStamp.Hash = rs.curHeader.BlockHash()
	rs.curStamp.Height--
}

// notifyBlockWithFilter calls appropriate listeners based on the block filter.
// This differs from notifyBlock in that is expects the caller to already have
// obtained the target filter.
func (rs *rescanState) notifyBlockWithFilter(header *wire.BlockHeader,
	stamp *headerfs.BlockStamp, filter *gcs.Filter) error {

	chain := rs.chain
	ro := rs.opts

	// Based on what we find within the block or the filter, we'll be
	// sending out a set of notifications with transactions that are
	// relevant to the rescan.
	var relevantTxs []*ltcutil.Tx

	// If we actually have a filter, then we'll go ahead an attempt to
	// match the items within the filter to ensure we create any relevant
	// notifications.
	if filter != nil {
		matched, err := matchBlockFilter(ro, filter, &stamp.Hash)
		if err != nil {
			return err
		}

		if matched {
			relevantTxs, err = extractBlockMatches(
				chain, ro, stamp, filter,
			)
			if err != nil {
				return err
			}
		}
	}

	if ro.ntfn.OnFilteredBlockConnected != nil {
		ro.ntfn.OnFilteredBlockConnected(stamp.Height, header,
			relevantTxs)
	}

	if ro.ntfn.OnBlockConnected != nil { // nolint:staticcheck
		ro.ntfn.OnBlockConnected( // nolint:staticcheck
			&stamp.Hash, stamp.Height, header.Timestamp,
		)
	}

	return nil
}

// matchBlockFilter returns whether the block filter matches the watched items.
// If this returns false, it means the block is certainly not interesting to
// us. This method differs from blockFilterMatches in that it expects the
// filter to already be obtained, rather than fetching the filter from the
// network.
func matchBlockFilter(ro *rescanOptions, filter *gcs.Filter,
	blockHash *chainhash.Hash) (bool, error) {

	// Now that we have the filter as well as the block hash of the block
	// used to construct the filter, we'll check to see if the block
	// matches any items in our watch list.
	key := builder.DeriveKey(blockHash)
	matched, err := filter.MatchAny(key, ro.watchList)
	if err != nil {
		return false, err
	}

	return matched, nil
}

// blockFilterMatches returns whether the block filter matches the watched
// items. If this returns false, it means the block is certainly not interesting
// to us.
func blockFilterMatches(chain ChainSource, ro *rescanOptions,
	blockHash *chainhash.Hash) (bool, *gcs.Filter, error) {

	// TODO(roasbeef): need to ENSURE always get filter

	// Since this method is called when we are not current, and from the
	// utxoscanner, we expect more calls to follow for the subsequent
	// filters. To speed up the fetching, we make an optimistic batch
	// query.
	filter, err := chain.GetCFilter(
		*blockHash, wire.GCSFilterRegular, OptimisticBatch(),
	)
	if err != nil {
		if err == headerfs.ErrHashNotFound {
			// Block has been reorged out from under us.
			return false, nil, nil
		}
		return false, nil, err
	}

	// If we found the filter, then we'll check the items in the watch list
	// against it.
	if filter.N() != 0 {
		matched, err := matchBlockFilter(ro, filter, blockHash)
		return matched, filter, err
	}

	return false, nil, nil
}

// updateFilter atomically updates the filter and rewinds to the specified
// height if not 0.
func (ro *rescanOptions) updateFilter(chain ChainSource, update *updateOptions,
	curStamp *headerfs.BlockStamp, curHeader *wire.BlockHeader) (bool, error) {

	ro.watchAddrs = append(ro.watchAddrs, update.addrs...)
	ro.watchInputs = append(ro.watchInputs, update.inputs...)

	for _, addr := range update.addrs {
		script, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return false, err
		}

		ro.watchList = append(ro.watchList, script)
	}
	for _, input := range update.inputs {
		ro.watchList = append(ro.watchList, input.PkScript)
	}
	for _, txid := range update.txIDs {
		ro.watchList = append(ro.watchList, txid[:])
	}

	// If we don't need to rewind, then we can exit early.
	if update.rewind == 0 {
		return false, nil
	}

	var (
		header  *wire.BlockHeader
		height  uint32
		rewound bool
		err     error
	)

	// If we need to rewind, then we'll walk backwards in the chain until
	// we arrive at the block _just_ before the rewind.
	for curStamp.Height > int32(update.rewind) {
		if ro.ntfn.OnBlockDisconnected != nil && // nolint:staticcheck
			!update.disableDisconnectedNtfns {

			ro.ntfn.OnBlockDisconnected( // nolint:staticcheck
				&curStamp.Hash,
				curStamp.Height, curHeader.Timestamp,
			)
		}
		if ro.ntfn.OnFilteredBlockDisconnected != nil &&
			!update.disableDisconnectedNtfns {

			ro.ntfn.OnFilteredBlockDisconnected(
				curStamp.Height, curHeader,
			)
		}

		// We just disconnected a block above, so we're now in rewind
		// mode. We set this to true here so we properly send
		// notifications even if it was just a 1 block rewind.
		rewound = true

		// Rewind and continue.
		header, height, err = chain.GetBlockHeader(&curHeader.PrevBlock)
		if err != nil {
			return rewound, err
		}

		*curHeader = *header
		curStamp.Height = int32(height)
		curStamp.Hash = curHeader.BlockHash()
	}

	return rewound, nil
}

// spendsWatchedInput returns whether the transaction matches the filter by
// spending a watched input.
func (ro *rescanOptions) spendsWatchedInput(tx *ltcutil.Tx) bool {
	for _, in := range tx.MsgTx().TxIn {
		for _, input := range ro.watchInputs {
			switch {
			// If we're watching for a zero outpoint, then we should
			// match on the output script being spent instead.
			case input.OutPoint == zeroOutPoint:
				pkScript, err := txscript.ComputePkScript(
					in.SignatureScript, in.Witness,
				)
				if err != nil {
					continue
				}

				if bytes.Equal(pkScript.Script(), input.PkScript) {
					return true
				}

			// Otherwise, we'll match on the outpoint being spent.
			case in.PreviousOutPoint == input.OutPoint:
				return true
			}
		}
	}
	return false
}

// paysWatchedAddr returns whether the transaction matches the filter by having
// an output paying to a watched address. If that is the case, this also
// updates the filter to watch the newly created output going forward.
func (ro *rescanOptions) paysWatchedAddr(tx *ltcutil.Tx) (bool, error) {
	anyMatchingOutputs := false

txOutLoop:
	for outIdx, out := range tx.MsgTx().TxOut {
		pkScript := out.PkScript

		for _, addr := range ro.watchAddrs {
			// We'll convert the address into its matching pkScript
			// to in order to check for a match.
			addrScript, err := txscript.PayToAddrScript(addr)
			if err != nil {
				return false, err
			}

			// If the script doesn't match, we'll move onto the
			// next one.
			if !bytes.Equal(pkScript, addrScript) {
				continue
			}

			// At this state, we have a matching output so we'll
			// mark this transaction as matching.
			anyMatchingOutputs = true

			// Update the filter by also watching this created
			// outpoint for the event in the future that it's
			// spent.
			hash := tx.Hash()
			outPoint := wire.OutPoint{
				Hash:  *hash,
				Index: uint32(outIdx),
			}
			ro.watchInputs = append(ro.watchInputs, InputWithScript{
				PkScript: pkScript,
				OutPoint: outPoint,
			})
			ro.watchList = append(ro.watchList, pkScript)

			continue txOutLoop
		}
	}

	return anyMatchingOutputs, nil
}

// Rescan is an object that represents a long-running rescan/notification
// client with updateable filters. It's meant to be close to a drop-in
// replacement for the btcd rescan and notification functionality used in
// wallets. It only contains information about whether a goroutine is running.
type Rescan struct { // nolint:maligned
	started uint32 // To be used atomically.

	running    chan struct{}
	updateChan chan *updateOptions

	options []RescanOption

	chain ChainSource

	errMtx sync.Mutex
	err    error

	wg sync.WaitGroup
}

// NewRescan returns a rescan object that runs in another goroutine and has an
// updatable filter. It returns the long-running rescan object, and a channel
// which returns any error on termination of the rescan process.
func NewRescan(chain ChainSource, options ...RescanOption) *Rescan {
	return &Rescan{
		running:    make(chan struct{}),
		options:    options,
		updateChan: make(chan *updateOptions),
		chain:      chain,
	}
}

// WaitForShutdown waits until all goroutines associated with the rescan have
// exited. This method is to be called once the passed quitchan (if any) has
// been closed.
func (r *Rescan) WaitForShutdown() {
	r.wg.Wait()
}

// Start kicks off the rescan goroutine, which will begin to scan the chain
// according to the specified rescan options.
func (r *Rescan) Start() <-chan error {
	errChan := make(chan error, 1)

	if !atomic.CompareAndSwapUint32(&r.started, 0, 1) {
		errChan <- fmt.Errorf("rescan already started")
		return errChan
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		rescanArgs := append(r.options, updateChan(r.updateChan)) // nolint
		rs, err := newRescanState(r.chain, rescanArgs...)
		if err == nil {
			err = rs.rescan()
		}

		close(r.running)

		r.errMtx.Lock()
		r.err = err
		r.errMtx.Unlock()

		errChan <- err
	}()

	return errChan
}

// updateOptions are a set of functional parameters for Update.
type updateOptions struct {
	addrs                    []ltcutil.Address
	inputs                   []InputWithScript
	txIDs                    []chainhash.Hash
	rewind                   uint32
	disableDisconnectedNtfns bool
}

// UpdateOption is a functional option argument for the Rescan.Update method.
type UpdateOption func(uo *updateOptions)

func defaultUpdateOptions() *updateOptions {
	return &updateOptions{}
}

// AddAddrs adds addresses to the filter.
func AddAddrs(addrs ...ltcutil.Address) UpdateOption {
	return func(uo *updateOptions) {
		uo.addrs = append(uo.addrs, addrs...)
	}
}

// AddInputs adds inputs to watch to the filter.
func AddInputs(inputs ...InputWithScript) UpdateOption {
	return func(uo *updateOptions) {
		uo.inputs = append(uo.inputs, inputs...)
	}
}

// Rewind rewinds the rescan to the specified height (meaning, disconnects down
// to the block immediately after the specified height) and restarts it from
// that point with the (possibly) newly expanded filter. Especially useful when
// called in the same Update() as one of the previous three options.
func Rewind(height uint32) UpdateOption {
	return func(uo *updateOptions) {
		uo.rewind = height
	}
}

// DisableDisconnectedNtfns tells the rescan not to send `OnBlockDisconnected`
// and `OnFilteredBlockDisconnected` notifications when rewinding.
func DisableDisconnectedNtfns(disabled bool) UpdateOption {
	return func(uo *updateOptions) {
		uo.disableDisconnectedNtfns = disabled
	}
}

// Update sends an update to a long-running rescan/notification goroutine.
func (r *Rescan) Update(options ...UpdateOption) error {
	ro := defaultRescanOptions()
	for _, option := range r.options {
		option(ro)
	}

	uo := defaultUpdateOptions()
	for _, option := range options {
		option(uo)
	}

	select {
	case r.updateChan <- uo:
	case <-ro.quit:
		return ErrRescanExit

	case <-r.running:
		errStr := "Rescan is already done and cannot be updated."
		r.errMtx.Lock()
		if r.err != nil {
			errStr += fmt.Sprintf(" It returned error: %s", r.err)
		}
		r.errMtx.Unlock()
		return fmt.Errorf(errStr)
	}

	return nil
}

// SpendReport is a struct which describes the current spentness state of a
// particular output. In the case that an output is spent, then the spending
// transaction and related details will be populated. Otherwise, only the
// target unspent output in the chain will be returned.
type SpendReport struct {
	// SpendingTx is the transaction that spent the output that a spend
	// report was requested for.
	//
	// NOTE: This field will only be populated if the target output has
	// been spent.
	SpendingTx *wire.MsgTx

	// SpendingTxIndex is the input index of the transaction above which
	// spends the target output.
	//
	// NOTE: This field will only be populated if the target output has
	// been spent.
	SpendingInputIndex uint32

	// SpendingTxHeight is the hight of the block that included the
	// transaction  above which spent the target output.
	//
	// NOTE: This field will only be populated if the target output has
	// been spent.
	SpendingTxHeight uint32

	// Output is the raw output of the target outpoint.
	//
	// NOTE: This field will only be populated if the target is still
	// unspent.
	Output *wire.TxOut

	// BlockHash is the block hash of the block that includes the unspent
	// output.
	//
	// NOTE: This field will only be populated if the target is still
	// unspent.
	BlockHash *chainhash.Hash

	// BlockHeight is the height of the block that includes the unspent output.
	//
	// NOTE: This field will only be populated if the target is still
	// unspent.
	BlockHeight uint32

	// BlockIndex is the index of the output's transaction in its block.
	//
	// NOTE: This field will only be populated if the target is still
	// unspent.
	BlockIndex uint32
}

// GetUtxo gets the appropriate TxOut or errors if it's spent. The option
// WatchOutPoints (with a single outpoint) is required. StartBlock can be used
// to give a hint about which block the transaction is in, and TxIdx can be
// used to give a hint of which transaction in the block matches it (coinbase
// is 0, first normal transaction is 1, etc.).
//
// TODO(roasbeef): WTB utxo-commitments.
func (s *ChainService) GetUtxo(options ...RescanOption) (*SpendReport, error) {
	// Before we start we'll fetch the set of default options, and apply
	// any user specified options in a functional manner.
	ro := defaultRescanOptions()
	ro.startBlock = &headerfs.BlockStamp{
		Hash:   *s.chainParams.GenesisHash,
		Height: 0,
	}
	for _, option := range options {
		option(ro)
	}

	// As this is meant to fetch UTXO's, the options MUST specify exactly
	// one outpoint.
	if len(ro.watchInputs) != 1 {
		return nil, fmt.Errorf("must pass exactly one OutPoint")
	}

	req, err := s.utxoScanner.Enqueue(
		&ro.watchInputs[0], uint32(ro.startBlock.Height), ro.progressHandler,
	)
	if err != nil {
		return nil, err
	}

	// Wait for the result to be delivered by the rescan or until a shutdown
	// is signaled.
	report, err := req.Result(ro.quit)
	if err != nil {
		log.Debugf("Error finding spends for %s: %v",
			ro.watchInputs[0].OutPoint.String(), err)
		return nil, err
	}

	return report, nil
}
