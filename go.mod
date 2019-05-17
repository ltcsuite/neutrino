module github.com/ltcsuite/neutrino

require (
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/davecgh/go-spew v1.1.1
	github.com/lightningnetwork/lnd/queue v1.0.1
	github.com/ltcsuite/ltcd v0.0.0-20190507171044-fbadf835b5c0
	github.com/ltcsuite/ltcutil v0.0.0-20190507133322-23cdfa9fcc3d
	github.com/ltcsuite/ltcwallet v0.0.0-20190105125346-3fa612e326e5
)

replace github.com/ltcsuite/ltcd => ../../ltcsuite/ltcd

replace github.com/ltcsuite/ltcwallet => ../../ltcsuite/ltcwallet

replace github.com/ltcsuite/neutrino => ../../ltcsuite/neutrino
