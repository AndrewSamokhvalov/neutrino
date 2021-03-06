// TODO: Break up tests into bite-sized pieces.

package neutrino_test

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aakselrod/btctestlog"
	"github.com/btcsuite/btclog"
	"github.com/lightninglabs/neutrino"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/btcjson"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/rpctest"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcrpcclient"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcutil/gcs"
	"github.com/roasbeef/btcutil/gcs/builder"
	"github.com/roasbeef/btcwallet/waddrmgr"
	"github.com/roasbeef/btcwallet/wallet/txauthor"
	"github.com/roasbeef/btcwallet/walletdb"
	_ "github.com/roasbeef/btcwallet/walletdb/bdb"
)

var (
	// Try btclog.InfoLvl for output like you'd see in normal operation, or
	// btclog.TraceLvl to help debug code. Anything but btclog.Off turns on
	// log messages from the tests themselves as well. Keep in mind some
	// log messages may not appear in order due to use of multiple query
	// goroutines in the tests.
	logLevel    = btclog.Off
	syncTimeout = 30 * time.Second
	syncUpdate  = time.Second
	// Don't set this too high for your platform, or the tests will miss
	// messages.
	// TODO: Make this a benchmark instead.
	// TODO: Implement load limiting for both outgoing and incoming
	// messages.
	numQueryThreads = 20
	queryOptions    = []neutrino.QueryOption{
	//neutrino.NumRetries(5),
	}
	// The logged sequence of events we want to see. The value of i
	// represents the block for which a loop is generating a log entry,
	// given for readability only.
	// "bc":	OnBlockConnected
	// "fc" xx:	OnFilteredBlockConnected with xx (uint8) relevant TXs
	// "rv":	OnRecvTx
	// "rd":	OnRedeemingTx
	// "bd":	OnBlockDisconnected
	// "fd":	OnFilteredBlockDisconnected
	wantLog = func() (log []byte) {
		for i := 796; i <= 800; i++ {
			// BlockConnected and FilteredBlockConnected
			log = append(log, []byte("bcfc")...)
			// 0 relevant TXs
			log = append(log, 0x00)
		}
		// Block with one relevant (receive) transaction
		log = append(log, []byte("bcrvfc")...)
		log = append(log, 0x01)
		// 124 blocks with nothing
		for i := 802; i <= 925; i++ {
			log = append(log, []byte("bcfc")...)
			log = append(log, 0x00)
		}
		// Block with 1 redeeming transaction
		log = append(log, []byte("bcrdfc")...)
		log = append(log, 0x01)
		// Block with nothing
		log = append(log, []byte("bcfc")...)
		log = append(log, 0x00)
		// Update with rewind - rewind back to 795, add another address,
		// and see more interesting transactions.
		for i := 927; i >= 796; i-- {
			// BlockDisconnected and FilteredBlockDisconnected
			log = append(log, []byte("bdfd")...)
		}
		// Forward to 800
		for i := 796; i <= 800; i++ {
			// BlockConnected and FilteredBlockConnected
			log = append(log, []byte("bcfc")...)
			// 0 relevant TXs
			log = append(log, 0x00)
		}
		// Block with two relevant (receive) transactions
		log = append(log, []byte("bcrvrvfc")...)
		log = append(log, 0x02)
		// 124 blocks with nothing
		for i := 802; i <= 925; i++ {
			log = append(log, []byte("bcfc")...)
			log = append(log, 0x00)
		}
		// 2 blocks with 1 redeeming transaction each
		for i := 926; i <= 927; i++ {
			log = append(log, []byte("bcrdfc")...)
			log = append(log, 0x01)
		}
		// Block with nothing
		log = append(log, []byte("bcfc")...)
		log = append(log, 0x00)
		// 3 block rollback
		for i := 928; i >= 926; i-- {
			log = append(log, []byte("fdbd")...)
		}
		// 5 block empty reorg
		for i := 926; i <= 930; i++ {
			log = append(log, []byte("bcfc")...)
			log = append(log, 0x00)
		}
		// 5 block rollback
		for i := 930; i >= 926; i-- {
			log = append(log, []byte("fdbd")...)
		}
		// 2 blocks with 1 redeeming transaction each
		for i := 926; i <= 927; i++ {
			log = append(log, []byte("bcrdfc")...)
			log = append(log, 0x01)
		}
		// 8 block rest of reorg
		for i := 928; i <= 935; i++ {
			log = append(log, []byte("bcfc")...)
			log = append(log, 0x00)
		}
		return log
	}()

	// rescanMtx locks all the variables to which the rescan goroutine's
	// notifications write.
	rescanMtx sync.RWMutex

	// gotLog is where we accumulate the event log from the rescan. Then we
	// compare it to wantLog to see if the series of events the rescan saw
	// happened as expected.
	gotLog []byte

	// curBlockHeight lets the rescan goroutine track where it thinks the
	// chain is based on OnBlockConnected and OnBlockDisconnected.
	curBlockHeight int32

	// curFilteredBlockHeight lets the rescan goroutine track where it
	// thinks the chain is based on OnFilteredBlockConnected and
	// OnFilteredBlockDisconnected.
	curFilteredBlockHeight int32

	// ourKnownTxsByBlock lets the rescan goroutine keep track of
	// transactions we're interested in that are in the blockchain we're
	// following as signalled by OnBlockConnected, OnBlockDisconnected,
	// OnRecvTx, and OnRedeemingTx.
	ourKnownTxsByBlock = make(map[chainhash.Hash][]*btcutil.Tx)

	// ourKnownTxsByFilteredBlock lets the rescan goroutine keep track of
	// transactions we're interested in that are in the blockchain we're
	// following as signalled by OnFilteredBlockConnected and
	// OnFilteredBlockDisconnected.
	ourKnownTxsByFilteredBlock = make(map[chainhash.Hash][]*btcutil.Tx)
)

// secSource is an implementation of btcwallet/txauthor/SecretsSource that
// stores WitnessPubKeyHash addresses.
type secSource struct {
	keys    map[string]*btcec.PrivateKey
	scripts map[string]*[]byte
	params  *chaincfg.Params
}

func (s *secSource) add(privKey *btcec.PrivateKey) (btcutil.Address, error) {
	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, s.params)
	if err != nil {
		return nil, err
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, err
	}
	s.keys[addr.String()] = privKey
	s.scripts[addr.String()] = &script
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(script, s.params)
	if err != nil {
		return nil, err
	}
	if addrs[0].String() != addr.String() {
		return nil, fmt.Errorf("Encoded and decoded addresses don't "+
			"match. Encoded: %s, decoded: %s", addr, addrs[0])
	}
	return addr, nil
}

// GetKey is required by the txscript.KeyDB interface
func (s *secSource) GetKey(addr btcutil.Address) (*btcec.PrivateKey, bool,
	error) {
	privKey, ok := s.keys[addr.String()]
	if !ok {
		return nil, true, fmt.Errorf("No key for address %s", addr)
	}
	return privKey, true, nil
}

// GetScript is required by the txscript.ScriptDB interface
func (s *secSource) GetScript(addr btcutil.Address) ([]byte, error) {
	script, ok := s.scripts[addr.String()]
	if !ok {
		return nil, fmt.Errorf("No script for address %s", addr)
	}
	return *script, nil
}

// ChainParams is required by the SecretsSource interface
func (s *secSource) ChainParams() *chaincfg.Params {
	return s.params
}

func newSecSource(params *chaincfg.Params) *secSource {
	return &secSource{
		keys:    make(map[string]*btcec.PrivateKey),
		scripts: make(map[string]*[]byte),
		params:  params,
	}
}

func TestSetup(t *testing.T) {
	// Set up logging.
	logger, err := btctestlog.NewTestLogger(t)
	if err != nil {
		t.Fatalf("Could not set up logger: %s", err)
	}
	chainLogger := btclog.NewSubsystemLogger(logger, "CHAIN: ")
	chainLogger.SetLevel(logLevel)
	neutrino.UseLogger(chainLogger)
	rpcLogger := btclog.NewSubsystemLogger(logger, "RPCC: ")
	rpcLogger.SetLevel(logLevel)
	btcrpcclient.UseLogger(rpcLogger)

	// Create a btcd SimNet node and generate 500 blocks
	h1, err := rpctest.New(&chaincfg.SimNetParams, nil, nil)
	if err != nil {
		t.Fatalf("Couldn't create harness: %s", err)
	}
	defer h1.TearDown()
	err = h1.SetUp(false, 0)
	if err != nil {
		t.Fatalf("Couldn't set up harness: %s", err)
	}
	_, err = h1.Node.Generate(500)
	if err != nil {
		t.Fatalf("Couldn't generate blocks: %s", err)
	}

	// Create a second btcd SimNet node
	h2, err := rpctest.New(&chaincfg.SimNetParams, nil, nil)
	if err != nil {
		t.Fatalf("Couldn't create harness: %s", err)
	}
	defer h2.TearDown()
	err = h2.SetUp(false, 0)
	if err != nil {
		t.Fatalf("Couldn't set up harness: %s", err)
	}

	// Create a third btcd SimNet node and generate 900 blocks
	h3, err := rpctest.New(&chaincfg.SimNetParams, nil, nil)
	if err != nil {
		t.Fatalf("Couldn't create harness: %s", err)
	}
	defer h3.TearDown()
	err = h3.SetUp(false, 0)
	if err != nil {
		t.Fatalf("Couldn't set up harness: %s", err)
	}
	_, err = h3.Node.Generate(900)
	if err != nil {
		t.Fatalf("Couldn't generate blocks: %s", err)
	}

	// Connect, sync, and disconnect h1 and h2
	err = csd([]*rpctest.Harness{h1, h2})
	if err != nil {
		t.Fatalf("Couldn't connect/sync/disconnect h1 and h2: %s", err)
	}

	// Generate 300 blocks on the first node and 350 on the second
	_, err = h1.Node.Generate(300)
	if err != nil {
		t.Fatalf("Couldn't generate blocks: %s", err)
	}
	_, err = h2.Node.Generate(350)
	if err != nil {
		t.Fatalf("Couldn't generate blocks: %s", err)
	}

	// Now we have a node with 800 blocks (h1), 850 blocks (h2), and
	// 900 blocks (h3). The chains of nodes h1 and h2 match up to block
	// 500. By default, a synchronizing wallet connected to all three
	// should synchronize to h3. However, we're going to take checkpoints
	// from h1 at 111, 333, 555, and 777, and add those to the
	// synchronizing wallet's chain parameters so that it should
	// disconnect from h3 at block 111, and from h2 at block 555, and
	// then synchronize to block 800 from h1. Order of connection is
	// unfortunately not guaranteed, so the reorg may not happen with every
	// test.

	// Copy parameters and insert checkpoints
	modParams := chaincfg.SimNetParams
	for _, height := range []int64{111, 333, 555, 777} {
		hash, err := h1.Node.GetBlockHash(height)
		if err != nil {
			t.Fatalf("Couldn't get block hash for height %d: %s",
				height, err)
		}
		modParams.Checkpoints = append(modParams.Checkpoints,
			chaincfg.Checkpoint{
				Hash:   hash,
				Height: int32(height),
			})
	}

	// Create a temporary directory, initialize an empty walletdb with an
	// SPV chain namespace, and create a configuration for the ChainService.
	tempDir, err := ioutil.TempDir("", "neutrino")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %s", err)
	}
	defer os.RemoveAll(tempDir)
	db, err := walletdb.Create("bdb", tempDir+"/weks.db")
	defer db.Close()
	if err != nil {
		t.Fatalf("Error opening DB: %s\n", err)
	}
	if err != nil {
		t.Fatalf("Error geting namespace: %s\n", err)
	}
	config := neutrino.Config{
		DataDir:     tempDir,
		Database:    db,
		ChainParams: modParams,
		AddPeers: []string{
			h3.P2PAddress(),
			h2.P2PAddress(),
			h1.P2PAddress(),
		},
	}

	neutrino.MaxPeers = 3
	neutrino.BanDuration = 5 * time.Second
	neutrino.WaitForMoreCFHeaders = time.Second
	svc, err := neutrino.NewChainService(config)
	if err != nil {
		t.Fatalf("Error creating ChainService: %s", err)
	}
	svc.Start()
	defer svc.Stop()

	// Make sure the client synchronizes with the correct node
	err = waitForSync(t, svc, h1)
	if err != nil {
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}

	// Generate an address and send it some coins on the h1 chain. We use
	// this to test rescans and notifications.
	secSrc := newSecSource(&modParams)
	privKey1, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("Couldn't generate private key: %s", err)
	}
	addr1, err := secSrc.add(privKey1)
	if err != nil {
		t.Fatalf("Couldn't create address from key: %s", err)
	}
	script1, err := secSrc.GetScript(addr1)
	if err != nil {
		t.Fatalf("Couldn't create script from address: %s", err)
	}
	out1 := wire.TxOut{
		PkScript: script1,
		Value:    1000000000,
	}
	// Fee rate is satoshis per byte
	tx1, err := h1.CreateTransaction([]*wire.TxOut{&out1}, 1000)
	if err != nil {
		t.Fatalf("Couldn't create transaction from script: %s", err)
	}
	_, err = h1.Node.SendRawTransaction(tx1, true)
	if err != nil {
		t.Fatalf("Unable to send raw transaction to node: %s", err)
	}
	privKey2, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("Couldn't generate private key: %s", err)
	}
	addr2, err := secSrc.add(privKey2)
	if err != nil {
		t.Fatalf("Couldn't create address from key: %s", err)
	}
	script2, err := secSrc.GetScript(addr2)
	if err != nil {
		t.Fatalf("Couldn't create script from address: %s", err)
	}
	out2 := wire.TxOut{
		PkScript: script2,
		Value:    1000000000,
	}
	// Fee rate is satoshis per byte
	tx2, err := h1.CreateTransaction([]*wire.TxOut{&out2}, 1000)
	if err != nil {
		t.Fatalf("Couldn't create transaction from script: %s", err)
	}
	_, err = h1.Node.SendRawTransaction(tx2, true)
	if err != nil {
		t.Fatalf("Unable to send raw transaction to node: %s", err)
	}
	_, err = h1.Node.Generate(1)
	if err != nil {
		t.Fatalf("Couldn't generate/submit block: %s", err)
	}
	err = waitForSync(t, svc, h1)
	if err != nil {
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}

	// Do a rescan that searches only for a specific TXID
	startBlock := waddrmgr.BlockStamp{Height: 795}
	endBlock := waddrmgr.BlockStamp{Height: 801}
	var foundTx *btcutil.Tx
	err = svc.Rescan(
		neutrino.StartBlock(&startBlock),
		neutrino.EndBlock(&endBlock),
		neutrino.WatchTxIDs(tx1.TxHash()),
		neutrino.NotificationHandlers(btcrpcclient.NotificationHandlers{
			OnFilteredBlockConnected: func(height int32,
				header *wire.BlockHeader,
				relevantTxs []*btcutil.Tx) {
				if height == 801 {
					if len(relevantTxs) != 1 {
						t.Fatalf("Didn't get expected "+
							"number of relevant "+
							"transactions from "+
							"rescan: want 1, got "+
							"%d", len(relevantTxs))
					}
					if *(relevantTxs[0].Hash()) !=
						tx1.TxHash() {
						t.Fatalf("Didn't get expected "+
							"relevant transaction:"+
							" want %s, got %s",
							tx1.TxHash(),
							relevantTxs[0].Hash())
					}
					foundTx = relevantTxs[0]
				}
			},
		}),
	)
	if err != nil || foundTx == nil || *(foundTx.Hash()) != tx1.TxHash() {
		t.Fatalf("Couldn't rescan chain for transaction %s: %s",
			tx1.TxHash(), err)
	}
	// Check that we got the right transaction index.
	blockHeader, err := svc.BlockHeaders.FetchHeaderByHeight(801)
	if err != nil {
		t.Fatalf("Couldn't get block hash for block 801: %s", err)
	}
	blockHash := blockHeader.BlockHash()
	block, err := h1.Node.GetBlock(&blockHash)
	if err != nil {
		t.Fatalf("Couldn't get block %s via RPC: %s", blockHash, err)
	}
	ourIndex := 0
	for i, tx := range block.Transactions {
		if tx.TxHash() == tx1.TxHash() {
			ourIndex = i
		}
	}
	if foundTx.Index() != ourIndex {
		t.Fatalf("Index of found transaction incorrect: want 1, got %d",
			foundTx.Index())
	}

	// Call GetUtxo for our output in tx1 to see if it's spent.
	ourIndex = 1 << 30 // Should work on 32-bit systems
	for i, txo := range tx1.TxOut {
		if bytes.Equal(txo.PkScript, script1) {
			ourIndex = i
		}
	}
	var ourOutPoint wire.OutPoint
	if ourIndex != 1<<30 {
		ourOutPoint = wire.OutPoint{
			Hash:  tx1.TxHash(),
			Index: uint32(ourIndex),
		}
	} else {
		t.Fatalf("Couldn't find the index of our output in transaction"+
			" %s", tx1.TxHash())
	}
	spendReport, err := svc.GetUtxo(
		neutrino.WatchOutPoints(ourOutPoint),
		neutrino.StartBlock(&waddrmgr.BlockStamp{Height: 801}),
	)
	if err != nil {
		t.Fatalf("Couldn't get UTXO %s: %s", ourOutPoint, err)
	}
	if !bytes.Equal(spendReport.Output.PkScript, script1) {
		t.Fatalf("UTXO's script doesn't match expected script for %s",
			ourOutPoint)
	}

	// Start a rescan with notifications in another goroutine. We'll kill
	// it with a quit channel at the end and make sure we got the expected
	// results.
	quitRescan := make(chan struct{})
	startBlock = waddrmgr.BlockStamp{Height: 795}
	rescan, errChan := startRescan(t, svc, addr1, &startBlock, quitRescan)
	if err != nil {
		t.Fatalf("Couldn't start a rescan for %s: %s", addr1, err)
	}
	err = waitForSync(t, svc, h1)
	if err != nil {
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	numTXs, _, err := checkRescanStatus()
	if numTXs != 1 {
		t.Fatalf("Wrong number of relevant transactions. Want: 1, got:"+
			" %d", numTXs)
	}

	// Generate 124 blocks on h1 to make sure it reorgs the other nodes.
	// Ensure the ChainService instance stays caught up.
	h1.Node.Generate(124)
	err = waitForSync(t, svc, h1)
	if err != nil {
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}

	// Connect/sync/disconnect h2 to make it reorg to the h1 chain.
	err = csd([]*rpctest.Harness{h1, h2})
	if err != nil {
		t.Fatalf("Couldn't sync h2 to h1: %s", err)
	}

	// Spend the outputs we sent ourselves over two blocks.
	inSrc := func(tx wire.MsgTx) func(target btcutil.Amount) (
		total btcutil.Amount, inputs []*wire.TxIn,
		inputValues []btcutil.Amount, scripts [][]byte, err error) {
		ourIndex := 1 << 30 // Should work on 32-bit systems
		for i, txo := range tx.TxOut {
			if bytes.Equal(txo.PkScript, script1) ||
				bytes.Equal(txo.PkScript, script2) {
				ourIndex = i
			}
		}
		return func(target btcutil.Amount) (total btcutil.Amount,
			inputs []*wire.TxIn, inputValues []btcutil.Amount,
			scripts [][]byte, err error) {
			if ourIndex == 1<<30 {
				err = fmt.Errorf("Couldn't find our address " +
					"in the passed transaction's outputs.")
				return
			}
			total = target
			inputs = []*wire.TxIn{
				{
					PreviousOutPoint: wire.OutPoint{
						Hash:  tx.TxHash(),
						Index: uint32(ourIndex),
					},
				},
			}
			inputValues = []btcutil.Amount{
				btcutil.Amount(tx.TxOut[ourIndex].Value)}
			scripts = [][]byte{tx.TxOut[ourIndex].PkScript}
			err = nil
			return
		}
	}
	// Create another address to send to so we don't trip the rescan with
	// the old address and we can test monitoring both OutPoint usage and
	// receipt by addresses.
	privKey3, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("Couldn't generate private key: %s", err)
	}
	addr3, err := secSrc.add(privKey3)
	if err != nil {
		t.Fatalf("Couldn't create address from key: %s", err)
	}
	script3, err := secSrc.GetScript(addr3)
	if err != nil {
		t.Fatalf("Couldn't create script from address: %s", err)
	}
	out3 := wire.TxOut{
		PkScript: script3,
		Value:    500000000,
	}
	// Spend the first transaction and mine a block.
	authTx1, err := txauthor.NewUnsignedTransaction(
		[]*wire.TxOut{
			&out3,
		},
		// Fee rate is satoshis per kilobyte
		1024000,
		inSrc(*tx1),
		func() ([]byte, error) {
			return script3, nil
		},
	)
	if err != nil {
		t.Fatalf("Couldn't create unsigned transaction: %s", err)
	}
	err = authTx1.AddAllInputScripts(secSrc)
	if err != nil {
		t.Fatalf("Couldn't sign transaction: %s", err)
	}
	banPeer(svc, h2)
	err = svc.SendTransaction(authTx1.Tx, queryOptions...)
	if err != nil {
		t.Fatalf("Unable to send transaction to network: %s", err)
	}
	_, err = h1.Node.Generate(1)
	if err != nil {
		t.Fatalf("Couldn't generate/submit block: %s", err)
	}
	err = waitForSync(t, svc, h1)
	if err != nil {
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	numTXs, _, err = checkRescanStatus()
	if numTXs != 2 {
		t.Fatalf("Wrong number of relevant transactions. Want: 2, got:"+
			" %d", numTXs)
	}
	// Spend the second transaction and mine a block.
	authTx2, err := txauthor.NewUnsignedTransaction(
		[]*wire.TxOut{
			&out3,
		},
		// Fee rate is satoshis per kilobyte
		1024000,
		inSrc(*tx2),
		func() ([]byte, error) {
			return script3, nil
		},
	)
	if err != nil {
		t.Fatalf("Couldn't create unsigned transaction: %s", err)
	}
	err = authTx2.AddAllInputScripts(secSrc)
	if err != nil {
		t.Fatalf("Couldn't sign transaction: %s", err)
	}
	banPeer(svc, h2)
	err = svc.SendTransaction(authTx2.Tx, queryOptions...)
	if err != nil {
		t.Fatalf("Unable to send transaction to network: %s", err)
	}
	_, err = h1.Node.Generate(1)
	if err != nil {
		t.Fatalf("Couldn't generate/submit block: %s", err)
	}
	err = waitForSync(t, svc, h1)
	if err != nil {
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	numTXs, _, err = checkRescanStatus()
	if numTXs != 2 {
		t.Fatalf("Wrong number of relevant transactions. Want: 2, got:"+
			" %d", numTXs)
	}

	// Update the filter with the second address, and we should have 2 more
	// relevant transactions.
	err = rescan.Update(neutrino.AddAddrs(addr2), neutrino.Rewind(795))
	if err != nil {
		t.Fatalf("Couldn't update the rescan filter: %s", err)
	}
	err = waitForSync(t, svc, h1)
	if err != nil {
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	numTXs, _, err = checkRescanStatus()
	if numTXs != 4 {
		t.Fatalf("Wrong number of relevant transactions. Want: 4, got:"+
			" %d", numTXs)
	}

	// Generate a block with a nonstandard coinbase to generate a basic
	// filter with 0 entries.
	_, err = h1.GenerateAndSubmitBlockWithCustomCoinbaseOutputs(
		[]*btcutil.Tx{}, rpctest.BlockVersion, time.Time{},
		[]wire.TxOut{{
			Value:    0,
			PkScript: []byte{},
		}})
	if err != nil {
		t.Fatalf("Couldn't generate/submit block: %s", err)
	}
	err = waitForSync(t, svc, h1)
	if err != nil {
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}

	// Check and make sure the previous UTXO is now spent.
	spendReport, err = svc.GetUtxo(
		neutrino.WatchOutPoints(ourOutPoint),
		neutrino.StartBlock(&waddrmgr.BlockStamp{Height: 801}),
	)
	if err != nil {
		t.Fatalf("Couldn't get UTXO %s: %s", ourOutPoint, err)
	}
	if spendReport.SpendingTx.TxHash() != authTx1.Tx.TxHash() {
		t.Fatalf("Redeeming transaction doesn't match expected "+
			"transaction: want %s, got %s", authTx1.Tx.TxHash(),
			spendReport.SpendingTx.TxHash())
	}

	// Test that we can get blocks and cfilters via P2P and decide which are
	// valid and which aren't.
	// TODO: Split this out into a benchmark.
	err = testRandomBlocks(t, svc, h1)
	if err != nil {
		t.Fatalf("Testing blocks and cfilters failed: %s", err)
	}

	// Generate 5 blocks on h2 and wait for ChainService to sync to the
	// newly-best chain on h2. This includes the transactions sent via
	// svc.SendTransaction earlier, so we'll have to
	_, err = h2.Node.Generate(5)
	if err != nil {
		t.Fatalf("Couldn't generate/submit blocks: %s", err)
	}
	err = waitForSync(t, svc, h2)
	if err != nil {
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	numTXs, _, err = checkRescanStatus()
	if numTXs != 2 {
		t.Fatalf("Wrong number of relevant transactions. Want: 2, got:"+
			" %d", numTXs)
	}

	// Generate 7 blocks on h1 and wait for ChainService to sync to the
	// newly-best chain on h1.
	_, err = h1.Node.Generate(7)
	if err != nil {
		t.Fatalf("Couldn't generate/submit block: %s", err)
	}
	err = waitForSync(t, svc, h1)
	if err != nil {
		t.Fatalf("Couldn't sync ChainService: %s", err)
	}
	numTXs, _, err = checkRescanStatus()
	if numTXs != 4 {
		t.Fatalf("Wrong number of relevant transactions. Want: 4, got:"+
			" %d", numTXs)
	}

	close(quitRescan)
	err = <-errChan
	if err != nil {
		t.Fatalf("Rescan ended with error: %s", err)
	}
	if !bytes.Equal(wantLog, gotLog) {
		leastBytes := len(wantLog)
		if len(gotLog) < leastBytes {
			leastBytes = len(gotLog)
		}
		diffIndex := 0
		for i := 0; i < leastBytes; i++ {
			if wantLog[i] != gotLog[i] {
				diffIndex = i
				break
			}
		}
		t.Fatalf("Rescan event logs differ starting at %d.\nWant: %s\n"+
			"Got:  %s\nDifference - want: %s\nDifference -- got: "+
			"%s", diffIndex, wantLog, gotLog, wantLog[diffIndex:],
			gotLog[diffIndex:])
	}
}

// csd does a connect-sync-disconnect between nodes in order to support
// reorg testing. It brings up and tears down a temporary node, otherwise the
// nodes try to reconnect to each other which results in unintended reorgs.
func csd(harnesses []*rpctest.Harness) error {
	hTemp, err := rpctest.New(&chaincfg.SimNetParams, nil, nil)
	if err != nil {
		return err
	}
	// Tear down node at the end of the function.
	defer hTemp.TearDown()
	err = hTemp.SetUp(false, 0)
	if err != nil {
		return err
	}
	for _, harness := range harnesses {
		err = rpctest.ConnectNode(hTemp, harness)
		if err != nil {
			return err
		}
	}
	return rpctest.JoinNodes(harnesses, rpctest.Blocks)
}

// waitForSync waits for the ChainService to sync to the current chain state.
func waitForSync(t *testing.T, svc *neutrino.ChainService,
	correctSyncNode *rpctest.Harness) error {
	knownBestHash, knownBestHeight, err :=
		correctSyncNode.Node.GetBestBlock()
	if err != nil {
		return err
	}
	if logLevel != btclog.Off {
		t.Logf("Syncing to %d (%s)", knownBestHeight, knownBestHash)
	}
	var haveBest *waddrmgr.BlockStamp
	haveBest, err = svc.BestSnapshot()
	if err != nil {
		return fmt.Errorf("Couldn't get best snapshot from "+
			"ChainService: %s", err)
	}
	var total time.Duration
	for haveBest.Hash != *knownBestHash {
		if total > syncTimeout {
			return fmt.Errorf("Timed out after %v waiting for "+
				"header synchronization.", syncTimeout)
		}
		if haveBest.Height > knownBestHeight {
			return fmt.Errorf("synchronized to the wrong chain")
		}
		time.Sleep(syncUpdate)
		total += syncUpdate
		haveBest, err = svc.BestSnapshot()
		if err != nil {
			return fmt.Errorf("Couldn't get best snapshot from "+
				"ChainService: %s", err)
		}
	}
	// Check if we're current.
	if !svc.IsCurrent() {
		return fmt.Errorf("the ChainService doesn't see itself as " +
			"current")
	}
	// Check if we have all of the cfheaders.
	knownBasicHeader, err := correctSyncNode.Node.GetCFilterHeader(
		knownBestHash, false)
	if err != nil {
		return fmt.Errorf("Couldn't get latest basic header from "+
			"%s: %s", correctSyncNode.P2PAddress(), err)
	}
	knownExtHeader, err := correctSyncNode.Node.GetCFilterHeader(
		knownBestHash, true)
	if err != nil {
		return fmt.Errorf("Couldn't get latest extended header from "+
			"%s: %s", correctSyncNode.P2PAddress(), err)
	}
	haveBasicHeader := &chainhash.Hash{}
	haveExtHeader := &chainhash.Hash{}
	for (*knownBasicHeader.HeaderHashes[0] != *haveBasicHeader) &&
		(*knownExtHeader.HeaderHashes[0] != *haveExtHeader) {
		if total > syncTimeout {
			return fmt.Errorf("Timed out after %v waiting for "+
				"cfheaders synchronization.", syncTimeout)
		}
		haveBasicHeader, _ = svc.RegFilterHeaders.FetchHeader(knownBestHash)
		haveExtHeader, _ = svc.ExtFilterHeaders.FetchHeader(knownBestHash)
		time.Sleep(syncUpdate)
		total += syncUpdate
	}
	if logLevel != btclog.Off {
		t.Logf("Synced cfheaders to %d (%s)", haveBest.Height,
			haveBest.Hash)
	}
	// At this point, we know the latest cfheader is stored in the
	// ChainService database. We now compare each cfheader the
	// harness knows about to what's stored in the ChainService
	// database to see if we've missed anything or messed anything
	// up.
	for i := int32(0); i <= haveBest.Height; i++ {
		head, err := svc.BlockHeaders.FetchHeaderByHeight(uint32(i))
		if err != nil {
			return fmt.Errorf("Couldn't read block by "+
				"height: %s", err)
		}
		hash := head.BlockHash()
		haveBasicHeader, err = svc.RegFilterHeaders.FetchHeader(&hash)
		if err != nil {
			return fmt.Errorf("Couldn't get basic header "+
				"for %d (%s) from DB", i, hash)
		}
		haveExtHeader, err = svc.ExtFilterHeaders.FetchHeader(&hash)
		if err != nil {
			return fmt.Errorf("Couldn't get extended "+
				"header for %d (%s) from DB", i, hash)
		}
		knownBasicHeader, err =
			correctSyncNode.Node.GetCFilterHeader(&hash,
				false)
		if err != nil {
			return fmt.Errorf("Couldn't get basic header "+
				"for %d (%s) from node %s", i, hash,
				correctSyncNode.P2PAddress())
		}
		knownExtHeader, err =
			correctSyncNode.Node.GetCFilterHeader(&hash,
				true)
		if err != nil {
			return fmt.Errorf("Couldn't get extended "+
				"header for %d (%s) from node %s", i,
				hash, correctSyncNode.P2PAddress())
		}
		if *haveBasicHeader !=
			*knownBasicHeader.HeaderHashes[0] {
			return fmt.Errorf("Basic header for %d (%s) "+
				"doesn't match node %s. DB: %s, node: "+
				"%s", i, hash,
				correctSyncNode.P2PAddress(),
				haveBasicHeader,
				knownBasicHeader.HeaderHashes[0])
		}
		if *haveExtHeader !=
			*knownExtHeader.HeaderHashes[0] {
			return fmt.Errorf("Extended header for %d (%s)"+
				" doesn't match node %s. DB: %s, node:"+
				" %s", i, hash,
				correctSyncNode.P2PAddress(),
				haveExtHeader,
				knownExtHeader.HeaderHashes[0])
		}
	}
	// At this point, we know we have good cfheaders. Now we wait for the
	// rescan, if one is going, to catch up.
	for {
		time.Sleep(syncUpdate)
		total += syncUpdate
		rescanMtx.RLock()
		// We don't want to do this if we haven't started a rescan
		// yet.
		if len(gotLog) == 0 {
			rescanMtx.RUnlock()
			break
		}
		_, rescanHeight, err := checkRescanStatus()
		if err != nil {
			rescanMtx.RUnlock()
			return err
		}
		if logLevel != btclog.Off {
			t.Logf("Rescan caught up to block %d", rescanHeight)
		}
		if rescanHeight == haveBest.Height {
			rescanMtx.RUnlock()
			break
		}
		if total > syncTimeout {
			rescanMtx.RUnlock()
			return fmt.Errorf("Timed out after %v waiting for "+
				"rescan to catch up.", syncTimeout)
		}
		rescanMtx.RUnlock()
	}
	return nil
}

// testRandomBlocks goes through all blocks in random order and ensures we can
// correctly get cfilters from them. It uses numQueryThreads goroutines running
// at the same time to go through this. 50 is comfortable on my somewhat dated
// laptop with default query optimization settings.
// TODO: Make this a benchmark instead.
func testRandomBlocks(t *testing.T, svc *neutrino.ChainService,
	correctSyncNode *rpctest.Harness) error {
	var haveBest *waddrmgr.BlockStamp
	haveBest, err := svc.BestSnapshot()
	if err != nil {
		return fmt.Errorf("Couldn't get best snapshot from "+
			"ChainService: %s", err)
	}
	// Keep track of an error channel with enough buffer space to track one
	// error per block.
	errChan := make(chan error, haveBest.Height)
	// Test getting all of the blocks and filters.
	var wg sync.WaitGroup
	workerQueue := make(chan struct{}, numQueryThreads)
	for i := int32(1); i <= haveBest.Height; i++ {
		wg.Add(1)
		height := uint32(i)
		// Wait until there's room in the worker queue.
		workerQueue <- struct{}{}
		go func() {
			// On exit, open a spot in workerQueue and tell the
			// wait group we're done.
			defer func() {
				<-workerQueue
			}()
			defer wg.Done()
			// Get block header from database.
			blockHeader, err := svc.BlockHeaders.FetchHeaderByHeight(height)
			if err != nil {
				errChan <- fmt.Errorf("Couldn't get block "+
					"header by height %d: %s", height, err)
				return
			}
			blockHash := blockHeader.BlockHash()
			// Get block via RPC.
			wantBlock, err := correctSyncNode.Node.GetBlock(
				&blockHash)
			if err != nil {
				errChan <- fmt.Errorf("Couldn't get block %d "+
					"(%s) by RPC", height, blockHash)
				return
			}
			// Get block from network.
			haveBlock, err := svc.GetBlockFromNetwork(blockHash,
				queryOptions...)
			if err != nil {
				errChan <- err
				return
			}
			if haveBlock == nil {
				errChan <- fmt.Errorf("Couldn't get block %d "+
					"(%s) from network", height, blockHash)
				return
			}
			// Check that network and RPC blocks match.
			if !reflect.DeepEqual(*haveBlock.MsgBlock(),
				*wantBlock) {
				errChan <- fmt.Errorf("Block from network "+
					"doesn't match block from RPC. Want: "+
					"%s, RPC: %s, network: %s", blockHash,
					wantBlock.BlockHash(),
					haveBlock.MsgBlock().BlockHash())
				return
			}
			// Check that block height matches what we have.
			if int32(height) != haveBlock.Height() {
				errChan <- fmt.Errorf("Block height from "+
					"network doesn't match expected "+
					"height. Want: %s, network: %s",
					height, haveBlock.Height())
				return
			}
			// Get basic cfilter from network.
			haveFilter, err := svc.GetCFilter(blockHash, false,
				queryOptions...)
			if err != nil {
				errChan <- err
				return
			}
			// Get basic cfilter from RPC.
			wantFilter, err := correctSyncNode.Node.GetCFilter(
				&blockHash, false)
			if err != nil {
				errChan <- fmt.Errorf("Couldn't get basic "+
					"filter for block %d via RPC: %s",
					height, err)
				return
			}
			// Check that network and RPC cfilters match.
			var haveBytes []byte
			if haveFilter != nil {
				haveBytes = haveFilter.NBytes()
			}
			if !bytes.Equal(haveBytes, wantFilter.Data) {
				errChan <- fmt.Errorf("Basic filter from P2P "+
					"network/DB doesn't match RPC value "+
					"for block %d", height)
				return
			}
			// Calculate basic filter from block.
			calcFilter, err := builder.BuildBasicFilter(
				haveBlock.MsgBlock())
			if err != nil && err != gcs.ErrNoData {
				errChan <- fmt.Errorf("Couldn't build basic "+
					"filter for block %d (%s): %s", height,
					blockHash, err)
				return
			}
			// Check that the network value matches the calculated
			// value from the block.
			if !reflect.DeepEqual(haveFilter, calcFilter) {
				errChan <- fmt.Errorf("Basic filter from P2P "+
					"network/DB doesn't match calculated "+
					"value for block %d", height)
				return
			}
			// Get previous basic filter header from the database.
			prevHeader, err := svc.RegFilterHeaders.FetchHeader(
				&blockHeader.PrevBlock)
			if err != nil {
				errChan <- fmt.Errorf("Couldn't get basic "+
					"filter header for block %d (%s) from "+
					"DB: %s", height-1,
					blockHeader.PrevBlock, err)
				return
			}
			// Get current basic filter header from the database.
			curHeader, err := svc.RegFilterHeaders.FetchHeader(&blockHash)
			if err != nil {
				errChan <- fmt.Errorf("Couldn't get basic "+
					"filter header for block %d (%s) from "+
					"DB: %s", height-1, blockHash, err)
				return
			}
			// Check that the filter and header line up.
			calcHeader := builder.MakeHeaderForFilter(calcFilter,
				*prevHeader)
			if !bytes.Equal(curHeader[:], calcHeader[:]) {
				errChan <- fmt.Errorf("Filter header doesn't "+
					"match. Want: %s, got: %s", curHeader,
					calcHeader)
				return
			}
			// Get extended cfilter from network
			haveFilter, err = svc.GetCFilter(blockHash, true,
				queryOptions...)
			if err != nil {
				errChan <- err
				return
			}
			// Get extended cfilter from RPC
			wantFilter, err = correctSyncNode.Node.GetCFilter(
				&blockHash, true)
			if err != nil {
				errChan <- fmt.Errorf("Couldn't get extended "+
					"filter for block %d via RPC: %s",
					height, err)
				return
			}
			// Check that network and RPC cfilters match
			if haveFilter != nil {
				haveBytes = haveFilter.NBytes()
			}
			if !bytes.Equal(haveBytes, wantFilter.Data) {
				errChan <- fmt.Errorf("Extended filter from "+
					"P2P network/DB doesn't match RPC "+
					"value for block %d", height)
				return
			}
			// Calculate extended filter from block
			calcFilter, err = builder.BuildExtFilter(
				haveBlock.MsgBlock())
			if err != nil && err != gcs.ErrNoData {
				errChan <- fmt.Errorf("Couldn't build extended"+
					" filter for block %d (%s): %s", height,
					blockHash, err)
				return
			}
			// Check that the network value matches the calculated
			// value from the block.
			if !reflect.DeepEqual(haveFilter, calcFilter) {
				errChan <- fmt.Errorf("Extended filter from "+
					"P2P network/DB doesn't match "+
					"calculated value for block %d", height)
				return
			}
			// Get previous extended filter header from the
			// database.
			prevHeader, err = svc.ExtFilterHeaders.FetchHeader(
				&blockHeader.PrevBlock)
			if err != nil {
				errChan <- fmt.Errorf("Couldn't get extended "+
					"filter header for block %d (%s) from "+
					"DB: %s", height-1,
					blockHeader.PrevBlock, err)
				return
			}
			// Get current basic filter header from the database.
			curHeader, err = svc.ExtFilterHeaders.FetchHeader(&blockHash)
			if err != nil {
				errChan <- fmt.Errorf("Couldn't get extended "+
					"filter header for block %d (%s) from "+
					"DB: %s", height-1,
					blockHash, err)
				return
			}
			// Check that the filter and header line up.
			calcHeader = builder.MakeHeaderForFilter(calcFilter,
				*prevHeader)
			if !bytes.Equal(curHeader[:], calcHeader[:]) {
				errChan <- fmt.Errorf("Filter header doesn't "+
					"match. Want: %s, got: %s", curHeader,
					calcHeader)
				return
			}
		}()
	}
	// Wait for all queries to finish.
	wg.Wait()
	// Close the error channel to make the error monitoring goroutine
	// finish.
	close(errChan)
	var lastErr error
	for err := range errChan {
		if err != nil {
			t.Errorf("%s", err)
			lastErr = fmt.Errorf("Couldn't validate all " +
				"blocks, filters, and filter headers.")
		}
	}
	if logLevel != btclog.Off {
		t.Logf("Finished checking %d blocks and their cfilters",
			haveBest.Height)
	}
	return lastErr
}

// startRescan starts a rescan in another goroutine, and logs all notifications
// from the rescan. At the end, the log should match one we precomputed based
// on the flow of the test. The rescan starts at the genesis block and the
// notifications continue until the `quit` channel is closed.
func startRescan(t *testing.T, svc *neutrino.ChainService, addr btcutil.Address,
	startBlock *waddrmgr.BlockStamp, quit <-chan struct{}) (neutrino.Rescan,
	<-chan error) {
	rescan := svc.NewRescan(
		neutrino.QuitChan(quit),
		neutrino.WatchAddrs(addr),
		neutrino.StartBlock(startBlock),
		neutrino.NotificationHandlers(
			btcrpcclient.NotificationHandlers{
				OnBlockConnected: func(
					hash *chainhash.Hash,
					height int32, time time.Time) {
					rescanMtx.Lock()
					gotLog = append(gotLog,
						[]byte("bc")...)
					curBlockHeight = height
					rescanMtx.Unlock()
				},
				OnBlockDisconnected: func(
					hash *chainhash.Hash,
					height int32, time time.Time) {
					rescanMtx.Lock()
					delete(ourKnownTxsByBlock, *hash)
					gotLog = append(gotLog,
						[]byte("bd")...)
					curBlockHeight = height - 1
					rescanMtx.Unlock()
				},
				OnRecvTx: func(tx *btcutil.Tx,
					details *btcjson.BlockDetails) {
					rescanMtx.Lock()
					hash, err := chainhash.
						NewHashFromStr(
							details.Hash)
					if err != nil {
						t.Errorf("Couldn't "+
							"decode hash "+
							"%s: %s",
							details.Hash,
							err)
					}
					ourKnownTxsByBlock[*hash] = append(
						ourKnownTxsByBlock[*hash],
						tx)
					gotLog = append(gotLog,
						[]byte("rv")...)
					rescanMtx.Unlock()
				},
				OnRedeemingTx: func(tx *btcutil.Tx,
					details *btcjson.BlockDetails) {
					rescanMtx.Lock()
					hash, err := chainhash.
						NewHashFromStr(
							details.Hash)
					if err != nil {
						t.Errorf("Couldn't "+
							"decode hash "+
							"%s: %s",
							details.Hash,
							err)
					}
					ourKnownTxsByBlock[*hash] = append(
						ourKnownTxsByBlock[*hash],
						tx)
					gotLog = append(gotLog,
						[]byte("rd")...)
					rescanMtx.Unlock()
				},
				OnFilteredBlockConnected: func(
					height int32,
					header *wire.BlockHeader,
					relevantTxs []*btcutil.Tx) {
					rescanMtx.Lock()
					ourKnownTxsByFilteredBlock[header.BlockHash()] =
						relevantTxs
					gotLog = append(gotLog,
						[]byte("fc")...)
					gotLog = append(gotLog,
						uint8(len(relevantTxs)))
					curFilteredBlockHeight = height
					rescanMtx.Unlock()
				},
				OnFilteredBlockDisconnected: func(
					height int32,
					header *wire.BlockHeader) {
					rescanMtx.Lock()
					delete(ourKnownTxsByFilteredBlock,
						header.BlockHash())
					gotLog = append(gotLog,
						[]byte("fd")...)
					curFilteredBlockHeight =
						height - 1
					rescanMtx.Unlock()
				},
			}),
	)

	errChan := rescan.Start()

	return rescan, errChan
}

// checkRescanStatus returns the number of relevant transactions we currently
// know about and the currently known height.
func checkRescanStatus() (int, int32, error) {
	var txCount [2]int
	rescanMtx.RLock()
	defer rescanMtx.RUnlock()
	for _, list := range ourKnownTxsByBlock {
		for range list {
			txCount[0]++
		}
	}
	for _, list := range ourKnownTxsByFilteredBlock {
		for range list {
			txCount[1]++
		}
	}
	if txCount[0] != txCount[1] {
		return 0, 0, fmt.Errorf("Conflicting transaction count " +
			"between notifications.")
	}
	if curBlockHeight != curFilteredBlockHeight {
		return 0, 0, fmt.Errorf("Conflicting block height between " +
			"notifications.")
	}
	return txCount[0], curBlockHeight, nil
}

// banPeer bans and disconnects the requested harness from the ChainService
// instance for BanDuration seconds.
func banPeer(svc *neutrino.ChainService, harness *rpctest.Harness) {
	peers := svc.Peers()
	for _, peer := range peers {
		if peer.Addr() == harness.P2PAddress() {
			svc.BanPeer(peer)
			peer.Disconnect()
		}
	}
}
