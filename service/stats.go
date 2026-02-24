package service

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/tonkeeper/tongo/tlb"
	"github.com/tonkeeper/tongo/ton"

	"github.com/tonkeeper/validators-statistics/model"
)

// FetchStats fetches validator statistics for the given seqno (or latest if nil).
func (s *Service) FetchStats(ctx context.Context, seqno *uint32, includeNominators bool) (*model.Output, error) {
	// Resolve the target block: use provided seqno or fall back to latest.
	var blockIDExt ton.BlockIDExt
	var blockTime time.Time

	// needBlockTime: for seqno=nil we defer LookupBlock to the parallel group.
	var needBlockTime bool

	if seqno != nil {
		var err error
		blockIDExt, blockTime, err = lookupMasterchainBlock(ctx, s.client, *seqno)
		if err != nil {
			return nil, fmt.Errorf("lookupMasterchainBlock: %w", err)
		}
	} else {
		model.CountRPC(ctx)
		info, err := s.client.GetMasterchainInfo(ctx)
		if err != nil {
			return nil, fmt.Errorf("GetMasterchainInfo: %w", err)
		}
		blockIDExt = ton.BlockIDExt{
			BlockID:  ton.BlockID{Workchain: int32(info.Last.Workchain), Shard: info.Last.Shard, Seqno: info.Last.Seqno},
			RootHash: ton.Bits256(info.Last.RootHash),
			FileHash: ton.Bits256(info.Last.FileHash),
		}
		needBlockTime = true
	}

	pinned := s.client.WithBlock(blockIDExt)

	// Fetch independent data in parallel.
	var (
		fetchWg        sync.WaitGroup
		conf           *ton.BlockchainConfig
		confErr        error
		pools          map[tlb.Bits256]poolEntry
		electorBalance uint64
		rewardPerBlock uint64
	)

	fetchWg.Add(4)

	// Block time (only when seqno was nil — deferred from block resolution).
	if needBlockTime {
		fetchWg.Add(1)
		go func() {
			defer fetchWg.Done()
			_, btime, err := lookupMasterchainBlock(ctx, s.client, blockIDExt.Seqno)
			if err == nil {
				blockTime = btime
			}
		}()
	}

	// Config param 34: current validators.
	go func() {
		defer fetchWg.Done()
		model.CountRPC(ctx)
		params, err := pinned.GetConfigParams(ctx, 0, []uint32{34})
		if err != nil {
			confErr = fmt.Errorf("GetConfigParams: %w (liteservers only retain state for recent blocks)", err)
			return
		}
		c, _, err := ton.ConvertBlockchainConfig(params, true)
		if err != nil {
			confErr = fmt.Errorf("ConvertBlockchainConfig: %w", err)
			return
		}
		conf = c
	}()

	// Pool addresses and true stakes from past_elections.
	go func() {
		defer fetchWg.Done()
		p, err := getAllPoolAddresses(ctx, pinned, electorAddr)
		if err != nil {
			log.Printf("warning: pool addresses: %v", err)
		}
		pools = p
	}()

	// Elector balance.
	go func() {
		defer fetchWg.Done()
		model.CountRPC(ctx)
		if st, err := pinned.GetAccountState(ctx, electorAddr); err == nil {
			electorBalance = uint64(st.Account.Account.Storage.Balance.Grams)
		}
	}()

	// Per-block reward = fees_collected from ValueFlow.
	go func() {
		defer fetchWg.Done()
		model.CountRPC(ctx)
		if block, err := s.client.GetBlock(ctx, blockIDExt); err == nil {
			rewardPerBlock = uint64(block.ValueFlow.FeesCollected.Grams)
		}
	}()

	fetchWg.Wait()

	if confErr != nil {
		return nil, confErr
	}

	log.Printf("block seqno=%d time=%s", blockIDExt.Seqno, blockTime.UTC().Format(time.RFC3339))

	out := model.Output{
		Block: model.BlockInfo{
			Seqno: blockIDExt.Seqno,
			Time:  blockTime.UTC().Format(time.RFC3339),
		},
		ElectorBalance: electorBalance,
		RewardPerBlock: rewardPerBlock,
	}

	// Validation round timing from config param 34.
	roundSince, roundUntil := getRoundInfo(conf)
	out.ValidationRound = model.RoundInfo{
		Start: time.Unix(int64(roundSince), 0).UTC().Format(time.RFC3339),
		End:   time.Unix(int64(roundUntil), 0).UTC().Format(time.RFC3339),
	}

	// Current validators.
	validators := extractValidators(conf)
	log.Printf("active validators: %d", len(validators))

	// Total true stake: sum only for current active validators.
	var totalTrueStake uint64
	for _, v := range validators {
		if pe, ok := pools[v.PubKey()]; ok {
			totalTrueStake += pe.TrueStake
		}
	}
	out.TotalStake = totalTrueStake
	log.Printf("total true stake (active validators): %.2f TON", float64(totalTrueStake)/1e9)

	type validatorRow struct {
		v          tlb.ValidatorDescr
		trueStake  uint64
		perBlockNT uint64
		pool       string
		poolAddr   *ton.AccountID
	}
	rows := make([]validatorRow, len(validators))
	for i, v := range validators {
		pk := v.PubKey()
		row := validatorRow{v: v}
		if pe, ok := pools[pk]; ok {
			row.pool = pe.Addr.ToHuman(true, false)
			addr := pe.Addr
			row.poolAddr = &addr
			row.trueStake = pe.TrueStake
			if rewardPerBlock > 0 && totalTrueStake > 0 {
				row.perBlockNT = uint64(float64(rewardPerBlock) * float64(pe.TrueStake) / float64(totalTrueStake))
			}
		}
		rows[i] = row
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].trueStake > rows[j].trueStake })

	// Collect nominators and build final validator list (parallel).
	entries := make([]model.ValidatorEntry, len(rows))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 100) // limit concurrent RPC calls

	for i, row := range rows {
		pk := row.v.PubKey()
		var share float64
		if totalTrueStake > 0 {
			share = float64(row.trueStake) / float64(totalTrueStake)
		}
		entries[i] = model.ValidatorEntry{
			Rank:           i + 1,
			Pubkey:         fmt.Sprintf("%x", pk[:]),
			Stake:          row.trueStake,
			Share:          share,
			PerBlockReward: row.perBlockNT,
			Pool:           row.pool,
		}

		if row.poolAddr == nil || !includeNominators {
			continue
		}

		wg.Add(1)
		go func(idx int, poolAddr ton.AccountID) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			poolType, nominators := detectPoolType(ctx, pinned, poolAddr)
			entries[idx].PoolType = poolType

			if nominators == nil {
				return
			}
			var totalAmount uint64
			for _, n := range nominators.Nominators {
				totalAmount += uint64(n.Amount)
			}
			for _, n := range nominators.Nominators {
				addr := ton.AccountID{Workchain: -1, Address: tlb.Bits256(n.Address)}
				var nomShare float64
				var nomPerBlock, nomStaked uint64
				if totalAmount > 0 {
					nomShare = float64(n.Amount) / float64(totalAmount)
					nomPerBlock = uint64(float64(rows[idx].perBlockNT) * float64(n.Amount) / float64(totalAmount))
					nomStaked = uint64(float64(rows[idx].trueStake) * float64(n.Amount) / float64(totalAmount))
				}
				entries[idx].Nominators = append(entries[idx].Nominators, model.NominatorEntry{
					Address:        addr.ToHuman(true, false),
					Share:          nomShare,
					PerBlockReward: nomPerBlock,
					Staked:         nomStaked,
					PoolBalance:    uint64(n.Amount),
				})
			}
		}(i, *row.poolAddr)
	}
	wg.Wait()

	out.Validators = entries
	return &out, nil
}
