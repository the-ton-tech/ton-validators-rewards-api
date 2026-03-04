// Command bonuses prints the elector bonus value for the validation round
// containing the given masterchain block.
//
// Usage:
//
//	go run ./cmd/bonuses -block 57900000
//	go run ./cmd/bonuses -block 57900000 -config archival-config.json
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"time"

	"github.com/tonkeeper/tongo/config"
	"github.com/tonkeeper/tongo/liteapi"
	"github.com/tonkeeper/tongo/tlb"
	"github.com/tonkeeper/tongo/ton"
	"github.com/tonkeeper/tongo/utils"

	"github.com/tonkeeper/validators-statistics/service"
)

var electorAddr = ton.AccountID{
	Workchain: -1,
	Address: [32]byte{
		0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33,
		0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33,
		0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33,
		0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33,
	},
}

func main() {
	block := flag.Uint("block", 0, "masterchain block seqno (required)")
	configPath := flag.String("config", "", "path to TON global config JSON")
	flag.Parse()

	if *block == 0 {
		fmt.Fprintln(os.Stderr, "usage: bonuses -block <seqno> [-config <path>]")
		os.Exit(1)
	}

	client, err := createClient(*configPath)
	if err != nil {
		log.Fatalf("create client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Lookup block → config param 34 → election_id.
	blockID := ton.BlockID{Workchain: -1, Shard: 0x8000000000000000, Seqno: uint32(*block)}
	ext, _, err := client.LookupBlock(ctx, blockID, 1, nil, nil)
	if err != nil {
		log.Fatalf("LookupBlock: %v", err)
	}

	pinned := client.WithBlock(ext)
	params, err := pinned.GetConfigParams(ctx, 0, []uint32{34})
	if err != nil {
		log.Fatalf("GetConfigParams: %v", err)
	}
	bcConf, _, err := ton.ConvertBlockchainConfig(params, true)
	if err != nil {
		log.Fatalf("ConvertBlockchainConfig: %v", err)
	}

	var since uint32
	if bcConf.ConfigParam34 != nil {
		vs := bcConf.ConfigParam34.CurValidators
		switch vs.SumType {
		case "Validators":
			since = vs.Validators.UtimeSince
		case "ValidatorsExt":
			since = vs.ValidatorsExt.UtimeSince
		}
	}
	if since == 0 {
		log.Fatal("config param 34 is empty")
	}

	// Fetch past_elections, find matching election_id, extract bonuses.
	_, stack, err := pinned.RunSmcMethodByID(ctx, electorAddr, utils.MethodIdFromName("past_elections"), tlb.VmStack{})
	if err != nil {
		log.Fatalf("past_elections: %v", err)
	}
	if len(stack) == 0 {
		log.Fatal("past_elections: empty stack")
	}

	elections, err := stack[0].VmStkTuple.RecursiveToSlice()
	if err != nil {
		log.Fatalf("RecursiveToSlice: %v", err)
	}

	for _, el := range elections {
		fields, err := el.VmStkTuple.Data.RecursiveToSlice(int(el.VmStkTuple.Len))
		if err != nil || len(fields) < service.PastElFieldBonuses+1 {
			continue
		}
		if fields[service.PastElFieldElectionID].VmStkTinyInt != int64(since) {
			continue
		}
		bonuses := extractBigInt(fields[service.PastElFieldBonuses])
		if bonuses == nil {
			log.Fatal("bonuses field is not an integer")
		}
		f := new(big.Float).Quo(new(big.Float).SetInt(bonuses), big.NewFloat(1e9))
		fmt.Printf("election_id=%d  bonuses=%s (%s TON)\n", since, bonuses.String(), f.Text('f', 2))
		return
	}

	log.Fatalf("election %d not found in past_elections", since)
}

func createClient(configPath string) (*liteapi.Client, error) {
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		conf, err := config.ParseConfig(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		return liteapi.NewClient(liteapi.WithConfigurationFile(*conf))
	}
	return liteapi.NewClientWithDefaultMainnet()
}

func extractBigInt(v tlb.VmStackValue) *big.Int {
	switch v.SumType {
	case "VmStkTinyInt":
		return big.NewInt(v.VmStkTinyInt)
	case "VmStkInt":
		i := big.Int(v.VmStkInt)
		return &i
	default:
		return nil
	}
}
