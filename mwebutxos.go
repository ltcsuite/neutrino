package neutrino

import (
	"cmp"
	"slices"

	"github.com/ltcsuite/ltcd/chaincfg/chainhash"
	"github.com/ltcsuite/ltcd/wire"
	"github.com/ltcsuite/neutrino/banman"
	"github.com/ltcsuite/neutrino/mweb"
	"github.com/ltcsuite/neutrino/query"
)

// mwebUtxosQuery holds all information necessary to perform and
// handle a query for mweb utxos.
type mwebUtxosQuery struct {
	blockMgr   *blockManager
	mwebHeader *wire.MwebHeader
	leafset    mweb.Leafset
	lastHeight uint32
	msgs       []*wire.MsgGetMwebUtxos
	utxosChan  chan *wire.MsgMwebUtxos
}

func (b *blockManager) getMwebUtxos(mwebHeader *wire.MwebHeader,
	newLeafset mweb.Leafset, lastHeight uint32,
	lastHash *chainhash.Hash) error {

	log.Infof("Fetching set of mweb utxos from "+
		"height=%v, hash=%v", lastHeight, *lastHash)

	newNumLeaves := mwebHeader.OutputMMRSize
	dbLeafset, oldNumLeaves, err := b.cfg.MwebCoins.GetLeafset()
	if err != nil {
		log.Errorf("Couldn't read mweb coins db: %v", err)
		return err
	}
	oldLeafset := mweb.Leafset(dbLeafset)

	// Skip over common prefix
	var index uint64
	for index < uint64(len(oldLeafset)) &&
		index < uint64(len(newLeafset)) &&
		oldLeafset[index] == newLeafset[index] {
		index++
	}

	type span struct {
		start uint64
		count uint16
	}
	var addLeaf span
	var addedLeaves []span
	var removedLeaves []uint64
	addLeafSpan := func() {
		if addLeaf.count > 0 {
			addedLeaves = append(addedLeaves, addLeaf)
			addLeaf = span{}
		}
	}
	for index *= 8; index < oldNumLeaves || index < newNumLeaves; index++ {
		if oldLeafset.Contains(index) {
			addLeafSpan()
			if !newLeafset.Contains(index) {
				removedLeaves = append(removedLeaves, index)
			}
		} else if newLeafset.Contains(index) {
			if addLeaf.count == 0 {
				addLeaf.start = index
			}
			addLeaf.count++
			if addLeaf.count == wire.MaxMwebUtxosPerQuery {
				addLeafSpan()
			}
		}
	}
	addLeafSpan()

	b.mwebUtxosCallbacksMtx.Lock()
	defer b.mwebUtxosCallbacksMtx.Unlock()

	batchesCount := len(addedLeaves)
	if batchesCount == 0 {
		return b.purgeSpentMwebTxos(newLeafset, newNumLeaves, removedLeaves)
	}

	log.Infof("Starting to query for mweb utxos from index=%v", addedLeaves[0].start)
	log.Infof("Attempting to query for %v mwebutxos batches", batchesCount)

	// With the set of messages constructed, we'll now request the
	// batch all at once. This message will distribute the mwebutxos
	// requests amongst all active peers, effectively sharding each
	// query dynamically.
	q := &mwebUtxosQuery{
		blockMgr:   b,
		mwebHeader: mwebHeader,
		leafset:    newLeafset,
		lastHeight: lastHeight,
		utxosChan:  make(chan *wire.MsgMwebUtxos),
	}

	totalUtxos := 0
	for len(addedLeaves) > 0 {
		for _, addLeaf := range addedLeaves {
			q.msgs = append(q.msgs, wire.NewMsgGetMwebUtxos(*lastHash,
				addLeaf.start, addLeaf.count, wire.MwebNetUtxoCompact))
			if len(q.msgs) == 10 {
				break
			}
		}
		addedLeaves = addedLeaves[len(q.msgs):]

		count, err := b.getMwebUtxosBatch(q)
		if err != nil {
			return err
		}
		totalUtxos += count
	}

	log.Infof("Successfully got %v mweb utxos", totalUtxos)

	return b.purgeSpentMwebTxos(newLeafset, newNumLeaves, removedLeaves)
}

func (b *blockManager) getMwebUtxosBatch(q *mwebUtxosQuery) (int, error) {
	// Hand the queries to the work manager, and consume the
	// verified responses as they come back.
	errChan := b.cfg.QueryDispatcher.Query(
		q.requests(), query.Cancel(b.quit))

	// Load the block height to leaf count mapping so that we can
	// work out roughly when a utxo was included in a block.
	heightMap, err := b.cfg.MwebCoins.GetLeavesAtHeight()
	if err != nil {
		log.Errorf("Couldn't get leaves at height from db: %v", err)
		return 0, err
	}
	heights := make([]uint32, 0, len(heightMap))
	for height := range heightMap {
		heights = append(heights, height)
	}
	slices.Sort(heights)

	// Keep waiting for more mwebutxos as long as we haven't received an
	// answer for our last getmwebutxos, and no error is encountered.
	totalUtxos := 0
	for len(q.msgs) > 0 {
		var r *wire.MsgMwebUtxos
		select {
		case r = <-q.utxosChan:
		case err := <-errChan:
			switch {
			case err == query.ErrWorkManagerShuttingDown:
				return totalUtxos, ErrShuttingDown
			case err != nil:
				log.Errorf("Query finished with error before "+
					"all responses received: %v", err)
				return totalUtxos, err
			}

			// The query did finish successfully, but continue to allow
			// picking up the last mwebutxos sent on the utxosChan.
			continue

		case <-b.quit:
			return totalUtxos, ErrShuttingDown
		}

		// Find the first and last indices for the mweb utxos
		// represented by this message.
		startIndex := r.Utxos[0].LeafIndex
		lastIndex := r.Utxos[len(r.Utxos)-1].LeafIndex

		index, ok := slices.BinarySearchFunc(q.msgs, startIndex,
			func(msg *wire.MsgGetMwebUtxos, target uint64) int {
				return cmp.Compare(msg.StartIndex, target)
			})
		if !ok {
			continue
		}
		q.msgs = append(q.msgs[:index], q.msgs[index+1:]...)

		log.Debugf("Got mwebutxos from index=%v to index=%v, "+
			"block hash=%v", startIndex, lastIndex, r.BlockHash)

		// Calculate rough heights for each utxo.
		for _, utxo := range r.Utxos {
			index, _ := slices.BinarySearchFunc(heights, utxo.LeafIndex,
				func(height uint32, target uint64) int {
					return cmp.Compare(heightMap[height]-1, target)
				})
			if index < len(heights) {
				utxo.Height = int32(heights[index])
			} else {
				utxo.Height = int32(q.lastHeight)
			}
		}

		if err := b.cfg.MwebCoins.PutCoins(r.Utxos); err != nil {
			log.Errorf("Couldn't write mweb coins: %v", err)
			return totalUtxos, err
		}

		for _, cb := range b.mwebUtxosCallbacks {
			cb(nil, r.Utxos)
		}

		totalUtxos += len(r.Utxos)
	}

	return totalUtxos, nil
}

func (b *blockManager) purgeSpentMwebTxos(newLeafset mweb.Leafset,
	newNumLeaves uint64, removedLeaves []uint64) error {

	if len(removedLeaves) > 0 {
		log.Infof("Purging %v spent mweb txos from db", len(removedLeaves))
	}

	err := b.cfg.MwebCoins.PutLeafsetAndPurge(
		newLeafset, newNumLeaves, removedLeaves)
	if err != nil {
		log.Errorf("Couldn't purge mweb txos: %v", err)
		return err
	}

	for _, cb := range b.mwebUtxosCallbacks {
		cb(newLeafset, nil)
	}

	return nil
}

// requests creates the query.Requests for this mwebutxos query.
func (m *mwebUtxosQuery) requests() []*query.Request {
	reqs := make([]*query.Request, len(m.msgs))
	for idx, msg := range m.msgs {
		reqs[idx] = &query.Request{
			Req:        msg,
			HandleResp: m.handleResponse,
		}
	}
	return reqs
}

// handleResponse is the internal response handler used for requests
// for this mwebutxos query.
func (m *mwebUtxosQuery) handleResponse(req, resp wire.Message,
	peerAddr string) query.Progress {

	r, ok := resp.(*wire.MsgMwebUtxos)
	if !ok {
		// We are only looking for mwebutxos messages.
		return query.Progress{}
	}

	q, ok := req.(*wire.MsgGetMwebUtxos)
	if !ok {
		// We sent a getmwebutxos message, so that's what
		// we should be comparing against.
		return query.Progress{}
	}

	// The response doesn't match the query.
	if !q.BlockHash.IsEqual(&r.BlockHash) ||
		q.StartIndex != r.StartIndex ||
		q.OutputFormat != r.OutputFormat ||
		q.NumRequested != uint16(len(r.Utxos)) {
		return query.Progress{}
	}

	if !mweb.VerifyUtxos(m.mwebHeader, m.leafset, r) {
		log.Warnf("Failed to verify mweb utxos at index %v!!!",
			r.StartIndex)

		// If the peer gives us a bad mwebutxos message, then we'll
		// ban the peer so we can reallocate the query elsewhere.
		err := m.blockMgr.cfg.BanPeer(peerAddr, banman.InvalidMwebUtxos)
		if err != nil {
			log.Errorf("Unable to ban peer %v: %v", peerAddr, err)
		}

		return query.Progress{}
	}

	// At this point, the response matches the query,
	// so we'll deliver the verified utxos on the utxosChan.
	// We'll also return a Progress indicating the query
	// finished, that the peer looking for the answer to this
	// query can move on to the next query.
	select {
	case m.utxosChan <- r:
	case <-m.blockMgr.quit:
		return query.Progress{}
	}

	return query.Progress{Finished: true, Progressed: true}
}

func (b *blockManager) notifyAddedMwebUtxos(leafset []byte) error {
	b.mwebUtxosCallbacksMtx.Lock()
	defer b.mwebUtxosCallbacksMtx.Unlock()

	dbLeafset, newNumLeaves, err := b.cfg.MwebCoins.GetLeafset()
	if err != nil {
		return err
	}
	oldLeafset := mweb.Leafset(leafset)
	newLeafset := mweb.Leafset(dbLeafset)

	// Skip over common prefix
	var index uint64
	for index < uint64(len(oldLeafset)) &&
		index < uint64(len(newLeafset)) &&
		oldLeafset[index] == newLeafset[index] {
		index++
	}

	var addedLeaves []uint64
	for index *= 8; index < newNumLeaves; index++ {
		if !oldLeafset.Contains(index) &&
			newLeafset.Contains(index) {
			addedLeaves = append(addedLeaves, index)
		}
	}

	utxos, err := b.cfg.MwebCoins.FetchLeaves(addedLeaves)
	if err != nil {
		return err
	}

	for _, cb := range b.mwebUtxosCallbacks {
		cb(newLeafset, utxos)
	}

	return nil
}

func (b *blockManager) notifyMwebUtxos(outputs []*wire.MwebOutput) {
	b.mwebUtxosCallbacksMtx.Lock()
	defer b.mwebUtxosCallbacksMtx.Unlock()

	var utxos []*wire.MwebNetUtxo
	for _, output := range outputs {
		utxos = append(utxos, &wire.MwebNetUtxo{
			Output:   output,
			OutputId: output.Hash(),
		})
	}
	for _, cb := range b.mwebUtxosCallbacks {
		cb(nil, utxos)
	}
}
