package neutrino

import (
	"fmt"

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
	msgs       []wire.Message
	utxosChan  chan *wire.MsgMwebUtxos
}

func (b *blockManager) getMwebUtxos(mwebHeader *wire.MwebHeader,
	newLeafset mweb.Leafset, lastHeight uint32, lastHash *chainhash.Hash) {

	log.Infof("Fetching set of mweb utxos from "+
		"height=%v, hash=%v", lastHeight, *lastHash)

	newNumLeaves := mwebHeader.OutputMMRSize
	dbLeafset, oldNumLeaves, err := b.cfg.MwebCoins.GetLeafSet()
	if err != nil {
		panic(fmt.Sprintf("couldn't read mweb coins db: %v", err))
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

	var queryMsgs []wire.Message
	for _, addLeaf := range addedLeaves {
		queryMsgs = append(queryMsgs,
			wire.NewMsgGetMwebUtxos(*lastHash, addLeaf.start,
				addLeaf.count, wire.MwebNetUtxoCompact))
	}

	// We'll also create an additional map that we'll use to
	// re-order the responses as we get them in.
	queryResponses := make(map[uint64]*wire.MsgMwebUtxos, len(queryMsgs))

	b.mwebUtxosCallbacksMtx.Lock()
	defer b.mwebUtxosCallbacksMtx.Unlock()

	batchesCount := len(queryMsgs)
	if batchesCount == 0 {
		b.purgeSpentMwebTxos(newLeafset, newNumLeaves, removedLeaves)
		return
	}

	log.Infof("Starting to query for mweb utxos from index=%v", addedLeaves[0].start)
	log.Infof("Attempting to query for %v mwebutxos batches", batchesCount)

	// With the set of messages constructed, we'll now request the batch
	// all at once. This message will distribute the mwebutxos requests
	// amongst all active peers, effectively sharding each query
	// dynamically.
	utxosChan := make(chan *wire.MsgMwebUtxos, len(queryMsgs))
	q := mwebUtxosQuery{
		blockMgr:   b,
		mwebHeader: mwebHeader,
		leafset:    newLeafset,
		msgs:       queryMsgs,
		utxosChan:  utxosChan,
	}

	// Hand the queries to the work manager, and consume the verified
	// responses as they come back.
	errChan := b.cfg.QueryDispatcher.Query(
		q.requests(), query.Cancel(b.quit),
	)

	// Keep waiting for more mwebutxos as long as we haven't received an
	// answer for our last getmwebutxos, and no error is encountered.
	totalUtxos := 0
	for i := 0; i < len(addedLeaves); {
		var r *wire.MsgMwebUtxos
		select {
		case r = <-utxosChan:
		case err := <-errChan:
			switch {
			case err == query.ErrWorkManagerShuttingDown:
				return
			case err != nil:
				log.Errorf("Query finished with error before "+
					"all responses received: %v", err)
				return
			}

			// The query did finish successfully, but continue to
			// allow picking up the last mwebutxos sent on the
			// utxosChan.
			continue

		case <-b.quit:
			return
		}

		// Find the first and last indices for the mweb utxos
		// represented by this message.
		startIndex := r.Utxos[0].LeafIndex
		lastIndex := r.Utxos[len(r.Utxos)-1].LeafIndex
		curIndex := addedLeaves[i].start

		log.Debugf("Got mwebutxos from index=%v to "+
			"index=%v, block hash=%v", startIndex,
			lastIndex, r.BlockHash)

		// If this is out of order but not yet written, we can
		// store them for later.
		if startIndex > curIndex {
			log.Debugf("Got response for mwebutxos at "+
				"index=%v, only at index=%v, stashing",
				startIndex, curIndex)
		}

		// If this is out of order stuff that's already been
		// written, we can ignore it.
		if lastIndex < curIndex {
			log.Debugf("Received out of order reply "+
				"lastIndex=%v, already written", lastIndex)
			continue
		}

		// Add the verified response to our cache.
		queryResponses[startIndex] = r

		// Then, we cycle through any cached messages, adding
		// them to the batch and deleting them from the cache.
		for i < len(addedLeaves) {
			// If we don't yet have the next response, then
			// we'll break out so we can wait for the peers
			// to respond with this message.
			curIndex = addedLeaves[i].start
			r, ok := queryResponses[curIndex]
			if !ok {
				break
			}

			// We have another response to write, so delete
			// it from the cache and write it.
			delete(queryResponses, curIndex)

			log.Debugf("Writing mwebutxos at index=%v", curIndex)

			err := b.cfg.MwebCoins.PutCoins(r.Utxos)
			if err != nil {
				panic(fmt.Sprintf("couldn't write mweb coins: %v", err))
			}

			for _, cb := range b.mwebUtxosCallbacks {
				cb(nil, r.Utxos)
			}

			totalUtxos += len(r.Utxos)

			// Update the next index to write.
			i++
		}
	}

	log.Infof("Successfully got %v mweb utxos", totalUtxos)

	b.purgeSpentMwebTxos(newLeafset, newNumLeaves, removedLeaves)
}

func (b *blockManager) purgeSpentMwebTxos(newLeafset mweb.Leafset,
	newNumLeaves uint64, removedLeaves []uint64) {

	if len(removedLeaves) > 0 {
		log.Infof("Purging %v spent mweb txos from db", len(removedLeaves))
	}

	err := b.cfg.MwebCoins.PutLeafSetAndPurge(
		newLeafset, newNumLeaves, removedLeaves)
	if err != nil {
		panic(fmt.Sprintf("couldn't purge mweb txos: %v", err))
	}

	for _, cb := range b.mwebUtxosCallbacks {
		cb(newLeafset, nil)
	}
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

// handleResponse is the internal response handler used for requests for this
// mwebutxos query.
func (m *mwebUtxosQuery) handleResponse(req, resp wire.Message,
	peerAddr string) query.Progress {

	r, ok := resp.(*wire.MsgMwebUtxos)
	if !ok {
		// We are only looking for mwebutxos messages.
		return query.Progress{
			Finished:   false,
			Progressed: false,
		}
	}

	q, ok := req.(*wire.MsgGetMwebUtxos)
	if !ok {
		// We sent a getmwebutxos message, so that's what we should be
		// comparing against.
		return query.Progress{
			Finished:   false,
			Progressed: false,
		}
	}

	// The response doesn't match the query.
	if !q.BlockHash.IsEqual(&r.BlockHash) ||
		q.StartIndex != r.StartIndex ||
		q.OutputFormat != r.OutputFormat ||
		q.NumRequested != uint16(len(r.Utxos)) {
		return query.Progress{
			Finished:   false,
			Progressed: false,
		}
	}

	if !mweb.VerifyUtxos(m.mwebHeader, m.leafset, r) {
		log.Warnf("Failed to verify mweb utxos at index %v!!!",
			r.StartIndex)

		// If the peer gives us a bad mwebutxos message,
		// then we'll ban the peer so we can re-allocate
		// the query elsewhere.
		err := m.blockMgr.cfg.BanPeer(
			peerAddr, banman.InvalidMwebUtxos,
		)
		if err != nil {
			log.Errorf("Unable to ban peer %v: %v", peerAddr, err)
		}

		return query.Progress{
			Finished:   false,
			Progressed: false,
		}
	}

	// At this point, the response matches the query,
	// so we'll deliver the verified utxos on the utxosChan.
	// We'll also return a Progress indicating the query
	// finished, that the peer looking for the answer to this
	// query can move on to the next query.
	select {
	case m.utxosChan <- r:
	case <-m.blockMgr.quit:
		return query.Progress{
			Finished:   false,
			Progressed: false,
		}
	}

	return query.Progress{
		Finished:   true,
		Progressed: true,
	}
}

func (b *blockManager) notifyAddedMwebUtxos(leafSet []byte) error {
	b.mwebUtxosCallbacksMtx.Lock()
	defer b.mwebUtxosCallbacksMtx.Unlock()

	dbLeafset, newNumLeaves, err := b.cfg.MwebCoins.GetLeafSet()
	if err != nil {
		return err
	}
	oldLeafset := mweb.Leafset(leafSet)
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
