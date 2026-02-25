package service

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/tonkeeper/tongo/tlb"
	"github.com/tonkeeper/tongo/ton"
	"golang.org/x/sync/errgroup"

	"github.com/tonkeeper/validators-statistics/model"
)

func msgAddressToHuman(addr tlb.MsgAddress, bounce bool) (string, bool) {
	id, err := ton.AccountIDFromTlb(addr)
	if err != nil || id == nil {
		return "", false
	}
	return id.ToHuman(bounce, false), true
}

// FetchStats fetches validator statistics for the given seqno (or latest if nil).
func (s *Service) FetchStats(ctx context.Context, seqno *uint32, includeNominators bool) (*model.Output, error) {
	client := s.currentClient()

	// Resolve the target block: use provided seqno or fall back to latest.
	var blockIDExt ton.BlockIDExt
	var blockTime time.Time

	// needBlockTime: for seqno=nil we defer LookupBlock to the parallel group.
	var needBlockTime bool

	if seqno != nil {
		var err error
		blockIDExt, blockTime, err = lookupMasterchainBlock(ctx, client, *seqno)
		if err != nil {
			return nil, fmt.Errorf("lookupMasterchainBlock: %w", err)
		}
	} else {
		info, err := retry(func() (ton.BlockIDExt, error) {
			model.CountRPC(ctx)
			res, err := client.GetMasterchainInfo(ctx)
			if err != nil {
				return ton.BlockIDExt{}, err
			}
			return ton.BlockIDExt{
				BlockID:  ton.BlockID{Workchain: int32(res.Last.Workchain), Shard: res.Last.Shard, Seqno: res.Last.Seqno},
				RootHash: ton.Bits256(res.Last.RootHash),
				FileHash: ton.Bits256(res.Last.FileHash),
			}, nil
		})
		if err != nil {
			return nil, fmt.Errorf("GetMasterchainInfo: %w", err)
		}
		blockIDExt = info
		needBlockTime = true
	}

	pinned := client.WithBlock(blockIDExt)

	// Fetch independent data in parallel.
	var (
		conf           *ton.BlockchainConfig
		pools          map[tlb.Bits256]poolEntry
		electorBalance uint64
		rewardPerBlock uint64
	)

	g := new(errgroup.Group)

	// Block time (only when seqno was nil — deferred from block resolution).
	if needBlockTime {
		g.Go(func() error {
			_, btime, err := lookupMasterchainBlock(ctx, client, blockIDExt.Seqno)
			if err == nil {
				blockTime = btime
			}
			return nil
		})
	}

	// Config param 34: current validators.
	g.Go(func() error {
		c, err := retry(func() (*ton.BlockchainConfig, error) {
			model.CountRPC(ctx)
			params, err := pinned.GetConfigParams(ctx, 0, []uint32{34})
			if err != nil {
				return nil, fmt.Errorf("GetConfigParams: %w (liteservers only retain state for recent blocks)", err)
			}
			c, _, err := ton.ConvertBlockchainConfig(params, true)
			if err != nil {
				return nil, fmt.Errorf("ConvertBlockchainConfig: %w", err)
			}
			return c, nil
		})
		if err != nil {
			return err
		}
		conf = c
		return nil
	})

	// Pool addresses and true stakes from past_elections.
	g.Go(func() error {
		p, err := getAllPoolAddresses(ctx, pinned, electorAddr)
		if err != nil {
			log.Printf("warning: pool addresses: %v", err)
		}
		pools = p
		return nil
	})

	// Elector balance.
	g.Go(func() error {
		bal, err := retry(func() (uint64, error) {
			model.CountRPC(ctx)
			st, err := pinned.GetAccountState(ctx, electorAddr)
			if err != nil {
				return 0, err
			}
			return uint64(st.Account.Account.Storage.Balance.Grams), nil
		})
		if err == nil {
			electorBalance = bal
		}
		return nil
	})

	// Per-block reward = fees_collected from ValueFlow.
	g.Go(func() error {
		reward, err := retry(func() (uint64, error) {
			model.CountRPC(ctx)
			block, err := client.GetBlock(ctx, blockIDExt)
			if err != nil {
				return 0, err
			}
			return uint64(block.ValueFlow.FeesCollected.Grams), nil
		})
		if err == nil {
			rewardPerBlock = reward
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
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
	g2 := new(errgroup.Group)

	for i, row := range rows {
		pk := row.v.PubKey()
		var share float64
		if totalTrueStake > 0 {
			share = float64(row.trueStake) / float64(totalTrueStake)
		}
		entries[i] = model.ValidatorEntry{
			Rank:           i + 1,
			Pubkey:         fmt.Sprintf("%x", pk[:]),
			EffectiveStake: row.trueStake,
			Weight:         share,
			PerBlockReward: row.perBlockNT,
			Pool:           row.pool,
		}

		if row.poolAddr == nil || !includeNominators {
			continue
		}

		g2.Go(func() error {
			poolAddr := *row.poolAddr
			poolType, pd := fetchPoolData(ctx, pinned, poolAddr)
			entries[i].PoolType = poolType

			if pd != nil {
				if vAddr, ok := msgAddressToHuman(pd.ValidatorWalletAddress, true); ok {
					entries[i].ValidatorAddress = vAddr
				} else if pd.ValidatorAddress != (tlb.Bits256{}) {
					vAddr := ton.AccountID{Workchain: -1, Address: [32]byte(pd.ValidatorAddress)}
					entries[i].ValidatorAddress = vAddr.ToHuman(true, false)
				}
				if ownerAddr, ok := msgAddressToHuman(pd.OwnerAddress, true); ok {
					entries[i].OwnerAddress = ownerAddr
				}
			}

			if pd == nil || poolType != "Nominator Pool" {
				// Non-Nominator Pool: fetch contract balance, approximate total.
				contractBal, err := retry(func() (uint64, error) {
					model.CountRPC(ctx)
					st, err := pinned.GetAccountState(ctx, poolAddr)
					if err != nil {
						return 0, err
					}
					return uint64(st.Account.Account.Storage.Balance.Grams), nil
				})
				if err == nil {
					entries[i].TotalStake = contractBal + rows[i].trueStake
				}
				return nil
			}

			// Nominator Pool: use data from GetPoolData + ListNominators.
			entries[i].ValidatorStake = pd.ValidatorAmount
			entries[i].NominatorsStake = pd.NominatorsAmount
			entries[i].TotalStake = entries[i].ValidatorStake + entries[i].NominatorsStake
			// ValidatorRewardShare is stored on-chain as a uint32 in range [0, 10000],
			// where 10000 = 100%. The nominator pool contract (pool.fc) computes:
			//   validator_reward = reward * validator_reward_share / 10000
			// For example, 3000 means the validator keeps 30% of rewards.
			// See: https://github.com/ton-blockchain/nominator-pool/blob/main/func/pool.fc
			entries[i].ValidatorRewardShare = float64(pd.RewardShare) / 10000.0
			entries[i].NominatorsCount = pd.NominatorsCount

			if pd.Nominators == nil {
				return nil
			}

			totalAmount := pd.NominatorsAmount
			// Nominator share of true_stake: nominators don't own the validator's own stake.
			nominatorsStake := rows[i].trueStake
			if totalAmount < nominatorsStake {
				nominatorsStake = totalAmount
			}

			nominatorRewardShare := 1.0 - float64(pd.RewardShare)/10000.0
			for _, n := range pd.Nominators.Nominators {
				addr := ton.AccountID{Workchain: 0, Address: tlb.Bits256(n.Address)}
				var nomWeight float64
				var nomPerBlock, nomStaked uint64
				if totalAmount > 0 {
					nomWeight = float64(n.Amount) / float64(totalAmount)
					nomStaked = uint64(float64(nominatorsStake) * float64(n.Amount) / float64(totalAmount))
					nomPerBlock = uint64(float64(rows[i].perBlockNT) * nominatorRewardShare * float64(n.Amount) / float64(totalAmount))
				}
				entries[i].Nominators = append(entries[i].Nominators, model.NominatorEntry{
					Address:        addr.ToHuman(false, false),
					Weight:         nomWeight,
					PerBlockReward: nomPerBlock,
					EffectiveStake: nomStaked,
					Stake:          uint64(n.Amount),
				})
			}
			return nil
		})
	}
	g2.Wait()

	out.Validators = entries
	return &out, nil
}
