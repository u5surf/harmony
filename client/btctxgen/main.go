package main

import (
	"flag"
	"fmt"
	"harmony-benchmark/blockchain"
	"harmony-benchmark/client"
	"harmony-benchmark/client/btctxiter"
	"harmony-benchmark/configr"
	"harmony-benchmark/consensus"
	"harmony-benchmark/log"
	"harmony-benchmark/node"
	"harmony-benchmark/p2p"
	proto_node "harmony-benchmark/proto/node"
	"sync"
	"time"

	"github.com/piotrnar/gocoin/lib/btc"
)

type txGenSettings struct {
	crossShard        bool
	maxNumTxsPerBatch int
}

var (
	utxoPoolMutex sync.Mutex
	setting       txGenSettings
	btcTXIter     btctxiter.BTCTXIterator
)

// Generates at most "maxNumTxs" number of simulated transactions based on the current UtxoPools of all shards.
// The transactions are generated by going through the existing utxos and
// randomly select a subset of them as the input for each new transaction. The output
// address of the new transaction are randomly selected from [0 - N), where N is the total number of fake addresses.
//
// When crossShard=true, besides the selected utxo input, select another valid utxo as input from the same address in a second shard.
// Similarly, generate another utxo output in that second shard.
//
// NOTE: the genesis block should contain N coinbase transactions which add
//       token (1000) to each address in [0 - N). See node.AddTestingAddresses()
//
// Params:
//     shardID                    - the shardID for current shard
//     dataNodes                  - nodes containing utxopools of all shards
// Returns:
//     all single-shard txs
//     all cross-shard txs
func generateSimulatedTransactions(shardID int, dataNodes []*node.Node) ([]*blockchain.Transaction, []*blockchain.Transaction) {
	/*
		  UTXO map structure:
		  {
			  address: {
				  txID: {
					  outputIndex: value
				  }
			  }
		  }
	*/

	utxoPoolMutex.Lock()
	txs := []*blockchain.Transaction{}
	crossTxs := []*blockchain.Transaction{}

	nodeShardID := dataNodes[shardID].Consensus.ShardID
	cnt := 0

LOOP:
	for true {
		btcTx := btcTXIter.NextTx()
		tx := blockchain.Transaction{}
		// tx.ID = tx.Hash.String()
		if btcTx.IsCoinBase() {
			// TxIn coinbase, newly generated coins
			prevTxID := [32]byte{}
			// TODO: merge txID with txIndex in TxInput
			tx.TxInput = []blockchain.TXInput{blockchain.TXInput{prevTxID, -1, "", nodeShardID}}
		} else {
			for _, txi := range btcTx.TxIn {
				tx.TxInput = append(tx.TxInput, blockchain.TXInput{txi.Input.Hash, int(txi.Input.Vout), "", nodeShardID})
			}
		}

		for _, txo := range btcTx.TxOut {
			txoAddr := btc.NewAddrFromPkScript(txo.Pk_script, false)
			if txoAddr == nil {
				log.Warn("TxOut: can't decode address")
			}
			txout := blockchain.TXOutput{int(txo.Value), txoAddr.String(), nodeShardID}
			tx.TxOutput = append(tx.TxOutput, txout)
		}
		tx.SetID()
		txs = append(txs, &tx)
		// log.Debug("[Generator] transformed btc tx", "block height", btcTXIter.GetBlockIndex(), "block tx count", btcTXIter.GetBlock().TxCount, "block tx cnt", len(btcTXIter.GetBlock().Txs), "txi", len(tx.TxInput), "txo", len(tx.TxOutput), "txCount", cnt)
		cnt++
		if cnt >= setting.maxNumTxsPerBatch {
			break LOOP
		}
	}

	utxoPoolMutex.Unlock()

	log.Debug("[Generator] generated transations", "single-shard", len(txs), "cross-shard", len(crossTxs))
	return txs, crossTxs
}

func initClient(clientNode *node.Node, clientPort string, leaders *[]p2p.Peer, nodes *[]*node.Node) {
	if clientPort == "" {
		return
	}

	clientNode.Client = client.NewClient(leaders)

	// This func is used to update the client's utxopool when new blocks are received from the leaders
	updateBlocksFunc := func(blocks []*blockchain.Block) {
		log.Debug("Received new block from leader", "len", len(blocks))
		for _, block := range blocks {
			for _, node := range *nodes {
				if node.Consensus.ShardID == block.ShardId {
					log.Debug("Adding block from leader", "shardId", block.ShardId)
					// Add it to blockchain
					utxoPoolMutex.Lock()
					node.AddNewBlock(block)
					utxoPoolMutex.Unlock()
				} else {
					continue
				}
			}
		}
	}
	clientNode.Client.UpdateBlocks = updateBlocksFunc

	// Start the client server to listen to leader's message
	go func() {
		clientNode.StartServer(clientPort)
	}()
}

func main() {
	configFile := flag.String("config_file", "local_config.txt", "file containing all ip addresses and config")
	maxNumTxsPerBatch := flag.Int("max_num_txs_per_batch", 100, "number of transactions to send per message")
	logFolder := flag.String("log_folder", "latest", "the folder collecting the logs of this execution")
	flag.Parse()

	// Read the configs
	config, _ := configr.ReadConfigFile(*configFile)
	leaders, shardIDs := configr.GetLeadersAndShardIds(&config)

	// Do cross shard tx if there are more than one shard
	setting.crossShard = len(shardIDs) > 1
	setting.maxNumTxsPerBatch = *maxNumTxsPerBatch

	// TODO(Richard): refactor this chuck to a single method
	// Setup a logger to stdout and log file.
	logFileName := fmt.Sprintf("./%v/txgen.log", *logFolder)
	h := log.MultiHandler(
		log.StdoutHandler,
		log.Must.FileHandler(logFileName, log.LogfmtFormat()), // Log to file
		// log.Must.NetHandler("tcp", ":3000", log.JSONFormat()) // Log to remote
	)
	log.Root().SetHandler(h)

	btcTXIter.Init()

	// Nodes containing utxopools to mirror the shards' data in the network
	nodes := []*node.Node{}
	for _, shardID := range shardIDs {
		nodes = append(nodes, node.New(&consensus.Consensus{ShardID: shardID}))
	}

	// Client/txgenerator server node setup
	clientPort := configr.GetClientPort(&config)
	consensusObj := consensus.NewConsensus("0", clientPort, "0", nil, p2p.Peer{})
	clientNode := node.New(consensusObj)

	initClient(clientNode, clientPort, &leaders, &nodes)

	// Transaction generation process
	time.Sleep(10 * time.Second) // wait for nodes to be ready
	start := time.Now()
	totalTime := 300.0 //run for 5 minutes

	for true {
		t := time.Now()
		if t.Sub(start).Seconds() >= totalTime {
			log.Debug("Generator timer ended.", "duration", (int(t.Sub(start))), "startTime", start, "totalTime", totalTime)
			break
		}

		allCrossTxs := []*blockchain.Transaction{}
		// Generate simulated transactions
		for i, leader := range leaders {
			txs, crossTxs := generateSimulatedTransactions(i, nodes)
			allCrossTxs = append(allCrossTxs, crossTxs...)

			log.Debug("[Generator] Sending single-shard txs ...", "leader", leader, "numTxs", len(txs), "numCrossTxs", len(crossTxs))
			msg := proto_node.ConstructTransactionListMessage(txs)
			p2p.SendMessage(leader, msg)
			// Note cross shard txs are later sent in batch
		}

		if len(allCrossTxs) > 0 {
			log.Debug("[Generator] Broadcasting cross-shard txs ...", "allCrossTxs", len(allCrossTxs))
			msg := proto_node.ConstructTransactionListMessage(allCrossTxs)
			p2p.BroadcastMessage(leaders, msg)

			// Put cross shard tx into a pending list waiting for proofs from leaders
			if clientPort != "" {
				clientNode.Client.PendingCrossTxsMutex.Lock()
				for _, tx := range allCrossTxs {
					clientNode.Client.PendingCrossTxs[tx.ID] = tx
				}
				clientNode.Client.PendingCrossTxsMutex.Unlock()
			}
		}

		time.Sleep(500 * time.Millisecond) // Send a batch of transactions periodically
	}

	// Send a stop message to stop the nodes at the end
	msg := proto_node.ConstructStopMessage()
	peers := append(configr.GetValidators(*configFile), leaders...)
	p2p.BroadcastMessage(peers, msg)
}