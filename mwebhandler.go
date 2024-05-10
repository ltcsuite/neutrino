package neutrino

import (
	"time"

	"github.com/ltcmweb/ltcd/ltcutil/mweb"
	"github.com/ltcmweb/ltcd/wire"
)

// mwebHandler is the mweb download handler for the block manager. It must be
// run as a goroutine. It requests and processes mweb messages in a
// separate goroutine from the peer handlers.
func (b *blockManager) mwebHandler() {
	defer b.wg.Done()
	defer log.Trace("Mweb handler done")

	for {
		b.newHeadersSignal.L.Lock()
		for !b.BlockHeadersSynced() {
			b.newHeadersSignal.Wait()

			// While we're awake, we'll quickly check to see if we need to
			// quit early.
			select {
			case <-b.quit:
				b.newHeadersSignal.L.Unlock()
				return
			default:
			}
		}
		b.newHeadersSignal.L.Unlock()

		// Now that the block headers are finished, we'll grab the current
		// chain tip so we can base our mweb header sync off of that.
		lastHeader, lastHeight, err := b.cfg.BlockHeaders.ChainTip()
		if err != nil {
			log.Critical(err)
			return
		}

		rollbackHeight, err := b.cfg.MwebCoins.GetRollbackHeight()
		if err != nil {
			log.Critical(err)
			return
		}
		if rollbackHeight > 0 && rollbackHeight < lastHeight {
			if lastHeight-rollbackHeight > 10 {
				err = b.cfg.MwebCoins.PurgeCoins()
			} else {
				lastHeight = rollbackHeight
				lastHeader, err = b.cfg.BlockHeaders.
					FetchHeaderByHeight(lastHeight)
			}
			if err != nil {
				log.Critical(err)
				return
			}
		}

		err = b.cfg.MwebCoins.RollbackLeavesAtHeight(rollbackHeight)
		if err != nil {
			log.Critical(err)
			return
		}

		lastHash := lastHeader.BlockHash()

		log.Infof("Starting mweb sync at (block_height=%v, block_hash=%v)",
			lastHeight, lastHash)

		// Get a representative set of mweb headers up to this height.
		err = b.getMwebHeaders(lastHeight)
		if err == ErrShuttingDown {
			return
		} else if err != nil {
			log.Error(err)
			continue
		}

		gdmsg := wire.NewMsgGetData()
		gdmsg.AddInvVect(wire.NewInvVect(wire.InvTypeMwebHeader, &lastHash))
		gdmsg.AddInvVect(wire.NewInvVect(wire.InvTypeMwebLeafset, &lastHash))

		var (
			mwebHeader  *wire.MsgMwebHeader
			mwebLeafset *wire.MsgMwebLeafset
			verified    bool
		)
		b.cfg.queryAllPeers(
			gdmsg,
			func(sp *ServerPeer, resp wire.Message, quit chan<- struct{},
				peerQuit chan<- struct{}) {

				switch m := resp.(type) {
				case *wire.MsgMwebHeader:
					if m.Merkle.Header.BlockHash() != lastHash {
						return
					}
					if err := mweb.VerifyHeader(m); err != nil {
						log.Infof("Failed to verify mwebheader: %v", err)
						return
					}
					mwebHeader = m

				case *wire.MsgMwebLeafset:
					mwebLeafset = m

				default:
					return
				}

				if mwebHeader == nil || mwebLeafset == nil {
					return
				}

				err := mweb.VerifyLeafset(mwebHeader, mwebLeafset)
				if err != nil {
					log.Infof("Failed to verify mwebleafset: %v", err)
					return
				}

				verified = true

				close(quit)
				close(peerQuit)
			},
		)
		select {
		case <-b.quit:
			return
		default:
			if !verified {
				time.Sleep(time.Second)
				continue
			}
		}

		log.Infof("Verified mwebheader and mwebleafset at "+
			"(block_height=%v, block_hash=%v)", lastHeight, lastHash)

		leafset := &mweb.Leafset{
			Bits:   mwebLeafset.Leafset,
			Size:   mwebHeader.MwebHeader.OutputMMRSize,
			Height: lastHeight,
			Block:  lastHeader,
		}

		// Store the leaf count at this height.
		err = b.cfg.MwebCoins.PutLeavesAtHeight(map[uint32]uint64{
			leafset.Height: leafset.Size})
		if err != nil {
			log.Critical(err)
			return
		}

		// Get all the mweb utxos at this height.
		err = b.getMwebUtxos(&mwebHeader.MwebHeader, leafset, &lastHash)
		if err != nil {
			continue
		}

		err = b.cfg.MwebCoins.ClearRollbackHeight(rollbackHeight)
		if err != nil {
			log.Critical(err)
			return
		}
		b.mwebRollbackSignal.Broadcast()

		// Now we check the headers again. If the block headers are not yet
		// current, then we go back to the loop waiting for them to finish.
		if !b.BlockHeadersSynced() {
			continue
		}

		// If block headers are current, but the mweb sync was for an
		// earlier block, we also go back to the loop.
		b.newHeadersMtx.RLock()
		if lastHeight < b.headerTip {
			b.newHeadersMtx.RUnlock()
			continue
		}
		b.newHeadersMtx.RUnlock()

		log.Infof("Fully caught up with mweb at height "+
			"%v, waiting at tip for new blocks", lastHeight)

		// Now that we've been fully caught up to the tip of the current header
		// chain, we'll wait here for a signal that more blocks have been
		// connected. If this happens then we'll do another round to fetch the
		// new set of mweb utxos.

		// We'll wait until the header tip has advanced.
		b.newHeadersSignal.L.Lock()
		for lastHeight >= b.headerTip {
			// We'll wait here until we're woken up by the
			// broadcast signal.
			b.newHeadersSignal.Wait()

			// Before we proceed, we'll check if we need to exit at
			// all.
			select {
			case <-b.quit:
				b.newHeadersSignal.L.Unlock()
				return
			default:
			}
		}
		b.newHeadersSignal.L.Unlock()
	}
}
