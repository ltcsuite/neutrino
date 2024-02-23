package neutrino

import (
	"slices"

	"github.com/ltcsuite/ltcd/wire"
	"github.com/ltcsuite/neutrino/banman"
	"github.com/ltcsuite/neutrino/mweb"
	"github.com/ltcsuite/neutrino/query"
)

// mwebHeadersQuery holds all information necessary to perform and
// handle a query for mweb headers.
type mwebHeadersQuery struct {
	blockMgr    *blockManager
	msgs        []wire.Message
	headersChan chan *wire.MsgMwebHeader
}

func (b *blockManager) getMwebHeaders(lastHeight uint32) error {
	var height uint32
	switch b.cfg.ChainParams.Net {
	case wire.MainNet:
		height = 2265984
	case wire.TestNet4:
		height = 2215584
	case wire.TestNet:
		height = 432
	}

	fetch := func(height, stride uint32) error {
		for height < lastHeight {
			toHeight := height + 1000*stride
			if toHeight > lastHeight {
				toHeight = lastHeight
			}
			err := b.getMwebHeaderBatch(height, toHeight, stride)
			if err != nil {
				return err
			}
			height = toHeight
		}
		return nil
	}

	if err := fetch(height, 100); err != nil {
		return err
	}
	if err := fetch(lastHeight-4000, 1); err != nil {
		return err
	}

	return nil
}

func (b *blockManager) getMwebHeaderBatch(
	fromHeight, toHeight, stride uint32) error {

	log.Infof("Fetching mweb headers from height %v to %v",
		fromHeight, toHeight)

	var queryMsgs []wire.Message
	var heights []uint32
	for height := fromHeight; height < toHeight; height += stride {
		header, err := b.cfg.BlockHeaders.FetchHeaderByHeight(height)
		if err != nil {
			return err
		}
		hash := header.BlockHash()

		var gdmsg *wire.MsgGetData
		if len(queryMsgs) > 0 {
			gdmsg = queryMsgs[len(queryMsgs)-1].(*wire.MsgGetData)
			if len(gdmsg.InvList) == 100 {
				gdmsg = nil
			}
		}
		if gdmsg == nil {
			gdmsg = wire.NewMsgGetData()
			queryMsgs = append(queryMsgs, gdmsg)
		}
		gdmsg.AddInvVect(wire.NewInvVect(wire.InvTypeMwebHeader, &hash))
		heights = append(heights, height)
	}

	// We'll also create an additional map that we'll use to
	// re-order the responses as we get them in.
	queryResponses := make(map[uint32]*wire.MsgMwebHeader, len(heights))

	batchesCount := len(queryMsgs)
	if batchesCount == 0 {
		return nil
	}

	log.Infof("Starting to query for mweb headers from height=%v", heights[0])

	// With the set of messages constructed, we'll now request the batch
	// all at once. This message will distribute the mwebheader requests
	// amongst all active peers, effectively sharding each query
	// dynamically.
	headersChan := make(chan *wire.MsgMwebHeader, len(heights))
	q := mwebHeadersQuery{
		blockMgr:    b,
		msgs:        queryMsgs,
		headersChan: headersChan,
	}

	// Hand the queries to the work manager, and consume the verified
	// responses as they come back.
	errChan := b.cfg.QueryDispatcher.Query(
		q.requests(), query.Cancel(b.quit),
	)

	// Keep waiting for more mweb headers as long as we haven't received an
	// answer for our last getdata message, and no error is encountered.
	for i := 0; i < len(heights); {
		var r *wire.MsgMwebHeader
		select {
		case r = <-headersChan:
		case err := <-errChan:
			switch {
			case err == query.ErrWorkManagerShuttingDown:
				return nil
			case err != nil:
				log.Errorf("Query finished with error before "+
					"all responses received: %v", err)
				return err
			}

			// The query did finish successfully, but continue to
			// allow picking up the last mwebheader sent on the
			// headersChan.
			continue

		case <-b.quit:
			return nil
		}

		height := uint32(r.MwebHeader.Height)
		blockHash := r.Merkle.Header.BlockHash()

		log.Debugf("Got mwebheader at height=%v, block hash=%v",
			height, blockHash)

		// If this is out of order but not yet written, we can
		// store them for later.
		if height > heights[i] {
			log.Debugf("Got response for mwebheader at "+
				"height=%v, only at height=%v, stashing",
				height, heights[i])
		}

		// If this is out of order stuff that's already been
		// written, we can ignore it.
		if height < heights[i] {
			log.Debugf("Received out of order reply "+
				"height=%v, already written", height)
			continue
		}

		// Add the verified response to our cache.
		queryResponses[height] = r

		// Then, we cycle through any cached messages, adding
		// them to the batch and deleting them from the cache.
		for i < len(heights) {
			// If we don't yet have the next response, then
			// we'll break out so we can wait for the peers
			// to respond with this message.
			r, ok := queryResponses[heights[i]]
			if !ok {
				break
			}

			// We have another response to write, so delete
			// it from the cache and write it.
			delete(queryResponses, heights[i])

			log.Debugf("Writing mwebheader at height=%v", heights[i])

			log.Infof("Height = %v, OutputMMRSize = %v", heights[i], r.MwebHeader.OutputMMRSize)

			// Update the next index to write.
			i++
		}
	}

	return nil
}

// requests creates the query.Requests for this mwebheader query.
func (m *mwebHeadersQuery) requests() []*query.Request {
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
// for this mwebheader query.
func (m *mwebHeadersQuery) handleResponse(req, resp wire.Message,
	peerAddr string) query.Progress {

	r, ok := resp.(*wire.MsgMwebHeader)
	if !ok {
		// We are only looking for mwebheader messages.
		return query.Progress{}
	}

	q, ok := req.(*wire.MsgGetData)
	if !ok {
		// We sent a getdata message, so that's what we should be
		// comparing against.
		return query.Progress{}
	}

	// The response doesn't match the query.
	blockHash := r.Merkle.Header.BlockHash()
	matchBlockHash := func(iv *wire.InvVect) bool {
		return iv.Hash.IsEqual(&blockHash)
	}
	if !slices.ContainsFunc(q.InvList, matchBlockHash) {
		return query.Progress{}
	}

	if err := mweb.VerifyHeader(r); err != nil {
		log.Warnf("Failed to verify mwebheader at block hash %v!!!",
			blockHash)

		// If the peer gives us a bad mwebheader message,
		// then we'll ban the peer so we can re-allocate
		// the query elsewhere.
		err := m.blockMgr.cfg.BanPeer(
			peerAddr, banman.InvalidMwebHeader,
		)
		if err != nil {
			log.Errorf("Unable to ban peer %v: %v", peerAddr, err)
		}

		return query.Progress{}
	}

	// At this point, the response matches the query,
	// so we'll deliver the verified header on the headersChan.
	select {
	case m.headersChan <- r:
	case <-m.blockMgr.quit:
		return query.Progress{}
	}

	q.InvList = slices.DeleteFunc(q.InvList, matchBlockHash)

	return query.Progress{
		Finished:   len(q.InvList) == 0,
		Progressed: true,
	}
}
