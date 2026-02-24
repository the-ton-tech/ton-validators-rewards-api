package service

import (
	"context"
	"time"

	"github.com/tonkeeper/tongo/liteapi"
	"github.com/tonkeeper/tongo/tlb"
	"github.com/tonkeeper/tongo/ton"
)

// lookupMasterchainBlock resolves a seqno to a BlockIDExt and returns the block time.
func lookupMasterchainBlock(ctx context.Context, client *liteapi.Client, seqno uint32) (ton.BlockIDExt, time.Time, error) {
	blockID := ton.BlockID{
		Workchain: -1,
		Shard:     0x8000000000000000,
		Seqno:     seqno,
	}
	countRPC(ctx)
	ext, info, err := client.LookupBlock(ctx, blockID, 1, nil, nil)
	if err != nil {
		return ton.BlockIDExt{}, time.Time{}, err
	}
	return ext, time.Unix(int64(info.GenUtime), 0), nil
}

// getRoundInfo returns the unix timestamps of the current validation round start and end.
// Config param 34 has two TL-B variants: "validators#11" (legacy) and "validators_ext#12"
// (current, adds TotalWeight). Both carry UtimeSince/UtimeUntil so we handle both.
func getRoundInfo(conf *ton.BlockchainConfig) (since, until uint32) {
	if conf.ConfigParam34 == nil {
		return
	}
	vs := conf.ConfigParam34.CurValidators
	switch vs.SumType {
	case "Validators":
		since = vs.Validators.UtimeSince
		until = vs.Validators.UtimeUntil
	case "ValidatorsExt":
		since = vs.ValidatorsExt.UtimeSince
		until = vs.ValidatorsExt.UtimeUntil
	}
	return since, until
}

// extractValidators returns all ValidatorDescr entries from config param 34.
// Handles both "validators#11" and "validators_ext#12" TL-B variants.
func extractValidators(conf *ton.BlockchainConfig) []tlb.ValidatorDescr {
	if conf.ConfigParam34 == nil {
		return nil
	}
	vs := conf.ConfigParam34.CurValidators
	var items []tlb.ValidatorDescr
	switch vs.SumType {
	case "Validators":
		for _, item := range vs.Validators.List.Items() {
			items = append(items, item.Value)
		}
	case "ValidatorsExt":
		for _, item := range vs.ValidatorsExt.List.Items() {
			items = append(items, item.Value)
		}
	}
	return items
}
