package main

import (
	"log"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
)

// blockHashCache stores entries mapping block hashes to block heights.
// NOTE: block heights are reduced to the smallest safe type, uint32
type blockHashCache map[string]uint32

// coinbaseCache stores entries mapping coinbase TXIDs to block heights.
type coinbaseCache map[chainhash.Hash]uint32

type blockHashJob struct {
	height int64
	hash   string
}

type coinbaseJob struct {
	height int64
	txid   string
}

// RPC call to lookup the block hash for a given block height.
func getBlockHash(client *rpcclient.Client, job *blockHashJob) blockHashJob {
	blockHash, err := client.GetBlockHash(job.height)
	errorLog(err)
	job.hash = blockHash.String()
	return *job
}

// Lookup the coinbase TXID for a given block height.
func getCoinbase(client *rpcclient.Client, job *coinbaseJob) coinbaseJob {
	blockHash, err := client.GetBlockHash(job.height)
	errorLog(err)
	block, err := client.GetBlock(blockHash)
	errorLog(err)
	firstTx := block.Transactions[0]
	txid := firstTx.TxHash()
	job.txid = txid.String()
	return *job
}

// Send as jobs a list of block heights from genesis to the specified height.
func pubCoinbaseJobs(height int64, waiter *sync.WaitGroup, jobs chan any) {
	waiter.Add(1)
	defer waiter.Done()

	for currentBlock := int64(0); currentBlock < height+1; currentBlock++ {
		jobs <- coinbaseJob{currentBlock, ""}
	}
	close(jobs)
}

// Send as jobs a list of block heights from genesis to the specified height.
func pubBlockHashJobs(height int64, waiter *sync.WaitGroup, jobs chan any) {
	waiter.Add(1)
	defer waiter.Done()

	for currentBlock := int64(0); currentBlock < height+1; currentBlock++ {
		jobs <- blockHashJob{currentBlock, ""}
	}
	close(jobs)
}

// Receive (blockheight, txid) pairs and add them to a map.
func subCoinbaseResults(cache coinbaseCache, waiter *sync.WaitGroup, results chan any) {
	waiter.Add(1)
	defer waiter.Done()

	for anyResult := range results {
		if result, ok := anyResult.(coinbaseJob); ok {
			height := int64ToUint32(result.height)
			txid, err := chainhash.NewHashFromStr(result.txid)
			errorLog(err)
			cache[*txid] = height
		} else {
			panic("invalid type for consumer")
		}
	}
}

// Receive (blockheight, blockhash) pairs and add them to a map.
func subBlockHashResults(cache blockHashCache, waiter *sync.WaitGroup, results chan any) {
	waiter.Add(1)
	defer waiter.Done()

	for anyResult := range results {
		if result, ok := anyResult.(blockHashJob); ok {
			height := int64ToUint32(result.height)
			cache[result.hash] = height
		} else {
			panic("invalid type for consumer")
		}
	}
}

// Accept an uninitialized coinbaseCache and populate it with entries.
// NOTE: Takes ~90 minutes to run on a Ryzen 7 5800H
func fillCoinbaseCache(ctx *workerContext) bool {
	ctx.coinbaseCache = make(map[chainhash.Hash]uint32)
	var pubSubSync sync.WaitGroup

	client, err := rpcclient.New(ctx.config, nil)
	errorPanic(err)
	defer client.Shutdown()

	maxBlock, err := client.GetBlockCount()
	errorLog(err)
	log.Printf("Block count: %d", maxBlock)

	workerSync, jobs, results := initWorkerPool(1, ctx.config)

	go pubCoinbaseJobs(maxBlock, &pubSubSync, jobs)
	go subCoinbaseResults(ctx.coinbaseCache, &pubSubSync, results)

	workerSync.Wait()
	close(results)
	pubSubSync.Wait()

	log.Println("Cache size: ", len(ctx.coinbaseCache))
	// Coinbases for blocks #91722 and #91812 are missing due to duplicate TXIDs
	// https://github.com/bitcoin/bitcoin/commit/ab91bf39b7c11e9c86bb2043c24f0f377f1cf514
	// The four coinbases from 91722, 91800, 91812, and 91842 are unspendable,
	// so only having those for 91800 and 91842 in the cache is fine.
	// This means our cache should have maxBlock-1 entries, not maxBlock+1.
	retval := int64(len(ctx.coinbaseCache)) == maxBlock-1
	if retval {
		log.Println("Coinbase cache is loaded")
	} else {
		log.Println("Failed to build coinbase cache")
	}
	return retval
}

// Accept an uninitialized blockHashCache and populate it with entries.
func fillBlockHashCache(ctx *workerContext) bool {
	ctx.blockHashCache = make(map[string]uint32)
	var pubSubSync sync.WaitGroup

	client, err := rpcclient.New(ctx.config, nil)
	errorPanic(err)
	defer client.Shutdown()

	maxBlock, err := client.GetBlockCount()
	errorLog(err)
	log.Printf("Block count: %d", maxBlock)

	workerSync, jobs, results := initWorkerPool(2, ctx.config)

	go pubBlockHashJobs(maxBlock, &pubSubSync, jobs)
	go subBlockHashResults(ctx.blockHashCache, &pubSubSync, results)

	workerSync.Wait()
	close(results)
	pubSubSync.Wait()

	retval := int64(len(ctx.blockHashCache)) == maxBlock+1
	if retval {
		log.Println("Block height cache is loaded")
	} else {
		log.Println("Failed to build block height cache")
	}
	return retval
}