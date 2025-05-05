module github.com/ltcsuite/neutrino

require (
	github.com/alitto/pond/v2 v2.2.0
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/davecgh/go-spew v1.1.1
	github.com/ltcsuite/lnd/queue v1.1.0
	github.com/ltcsuite/ltcd v0.23.6
	github.com/ltcsuite/ltcd/btcec/v2 v2.3.2
	github.com/ltcsuite/ltcd/chaincfg/chainhash v1.0.2
	github.com/ltcsuite/ltcwallet v0.13.1
	github.com/ltcsuite/ltcwallet/wallet/txauthor v1.1.0
	github.com/ltcsuite/ltcwallet/walletdb v1.3.5
	github.com/ltcsuite/neutrino/cache v1.1.0
	github.com/stretchr/testify v1.8.3
)

require (
	github.com/ltcsuite/ltcd/ltcutil v1.1.4-0.20250505084124-c37ac1524e04
	github.com/ltcsuite/ltcwallet/wtxmgr v1.5.0
)

require (
	github.com/aead/siphash v1.0.1 // indirect
	github.com/btcsuite/go-socks v0.0.0-20170105172521-4720035b7bfd // indirect
	github.com/btcsuite/websocket v0.0.0-20150119174127-31079b680792 // indirect
	github.com/decred/dcrd/crypto/blake256 v1.0.0 // indirect
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.0.1 // indirect
	github.com/decred/dcrd/lru v1.1.1 // indirect
	github.com/kkdai/bstream v1.0.0 // indirect
	github.com/klauspost/cpuid/v2 v2.0.9 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/ltcsuite/lnd/clock v0.0.0-20200822020009-1a001cbb895a // indirect
	github.com/ltcsuite/lnd/ticker v1.0.1 // indirect
	github.com/ltcsuite/ltcwallet/wallet/txrules v1.2.0 // indirect
	github.com/ltcsuite/ltcwallet/wallet/txsizes v1.1.0 // indirect
	github.com/ltcsuite/secp256k1 v0.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	go.etcd.io/bbolt v1.3.5-0.20200615073812-232d8fc87f50 // indirect
	golang.org/x/crypto v0.7.0 // indirect
	golang.org/x/sys v0.13.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	lukechampine.com/blake3 v1.2.1 // indirect
)

replace github.com/ltcsuite/neutrino/cache => ./cache

go 1.18
