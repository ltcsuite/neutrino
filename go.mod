module github.com/ltcsuite/neutrino

require (
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/davecgh/go-spew v1.1.1
	github.com/ltcsuite/lnd/queue v1.0.1
	github.com/ltcsuite/ltcd v0.20.1-beta
	github.com/ltcsuite/ltcutil v0.0.0-20191227053721-6bec450ea6ad
	github.com/ltcsuite/ltcwallet/wallet/txauthor v1.0.0
	github.com/ltcsuite/ltcwallet/walletdb v1.2.0
	github.com/ltcsuite/ltcwallet/wtxmgr v1.0.0
)

replace github.com/ltcsuite/lnd/queue => ../lnd/queue

replace github.com/ltcsuite/ltcwallet/wallet/txauthor => ../ltcwallet/wallet/txauthor

replace github.com/ltcsuite/ltcwallet/walletdb => ../ltcwallet/walletdb

replace github.com/ltcsuite/ltcwallet/wtxmgr => ../ltcwallet/wtxmgr

go 1.13
