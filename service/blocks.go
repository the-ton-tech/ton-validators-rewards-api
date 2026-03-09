package service

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"time"

	"github.com/tonkeeper/tongo/tlb"
	"github.com/tonkeeper/tongo/ton"
	"golang.org/x/sync/errgroup"

	"github.com/the-ton-tech/ton-validators-rewards-api/model"
	"github.com/the-ton-tech/ton-validators-rewards-api/utils"
)

// FetchPerBlockRewards fetches validator statistics for the given seqno (or latest if nil).
func (s *Service) FetchPerBlockRewards(ctx context.Context, seqno *uint32, includeNominators bool) (*model.Output, error) {
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
		rd                roundData
		electorBalance    = new(big.Int)
		rewardPerBlock    = new(big.Int)
		previousElections []RawPastElection
	)

	// fetchGroup: parallel fetches for round data, elector balance, and previous block elections.
	fetchGroup := new(errgroup.Group)

	// Block time (only when seqno was nil — deferred from block resolution).
	if needBlockTime {
		fetchGroup.Go(func() error {
			_, btime, err := lookupMasterchainBlock(ctx, client, blockIDExt.Seqno)
			if err == nil {
				blockTime = btime
			}
			return nil
		})
	}

	// Config, pools, and current elections.
	fetchGroup.Go(func() error {
		r, err := fetchRoundData(ctx, pinned)
		if err != nil {
			return err
		}
		rd = r
		return nil
	})

	// Elector balance.
	fetchGroup.Go(func() error {
		bal, err := retry(func() (uint64, error) {
			model.CountRPC(ctx)
			st, err := pinned.GetAccountState(ctx, electorAddr)
			if err != nil {
				return 0, err
			}
			return uint64(st.Account.Account.Storage.Balance.Grams), nil
		})
		if err == nil {
			electorBalance.SetUint64(bal)
		}
		return nil
	})

	// Past elections at previous block (for bonus diff calculation).
	fetchGroup.Go(func() error {
		if blockIDExt.Seqno <= 1 {
			return nil
		}
		prevExt, _, err := lookupMasterchainBlock(ctx, client, blockIDExt.Seqno-1)
		if err != nil {
			return nil
		}
		prevPinned := client.WithBlock(prevExt)
		parsed, err := fetchRawPastElections(ctx, prevPinned, electorAddr)
		if err == nil {
			previousElections = parsed
		}
		return nil
	})

	if err := fetchGroup.Wait(); err != nil {
		return nil, err
	}

	log.Printf("block seqno=%d time=%s", blockIDExt.Seqno, blockTime.UTC().Format(time.RFC3339))

	// Validation round timing from config param 34.
	roundSince, roundUntil := getRoundInfo(rd.conf)

	// Per-block reward = bonus diff between current and previous block in elector.
	electionID := int64(roundSince)
	if cur := findElection(rd.elections, electionID); cur != nil && cur.Bonuses != nil {
		if prev := findElection(previousElections, electionID); prev != nil && prev.Bonuses != nil {
			rewardPerBlock.Sub(cur.Bonuses, prev.Bonuses)
		}
	}

	// blockGroup: resolve round start/end block seqnos in parallel.
	var roundStartBlock, roundEndBlock uint32
	blockGroup := new(errgroup.Group)
	if roundSince > 0 {
		blockGroup.Go(func() error {
			ext, err := lookupMasterchainBlockByUtime(ctx, client, roundSince)
			if err == nil {
				roundStartBlock = ext.Seqno
			}
			return nil
		})
	}
	if roundUntil > 0 && time.Unix(int64(roundUntil), 0).Before(time.Now()) {
		blockGroup.Go(func() error {
			ext, err := lookupMasterchainBlockByUtime(ctx, client, roundUntil)
			if err == nil {
				roundEndBlock = ext.Seqno
			}
			return nil
		})
	}
	_ = blockGroup.Wait()

	out := model.Output{
		Block: model.BlockInfo{
			Seqno: blockIDExt.Seqno,
			Time:  blockTime.UTC().Format(time.RFC3339),
		},
		ElectionID:     int64(roundSince),
		ElectorBalance: electorBalance,
		RewardPerBlock: rewardPerBlock,
	}
	if roundStartBlock > 0 {
		out.PrevElectionID = fetchPrevElectionIDForBlock(ctx, client, roundStartBlock)
	}
	if roundUntil > 0 && time.Unix(int64(roundUntil), 0).Before(time.Now()) {
		nextID := int64(roundUntil)
		out.NextElectionID = &nextID
	}
	out.ValidationRound = model.RoundInfo{
		Start:      time.Unix(int64(roundSince), 0).UTC().Format(time.RFC3339),
		End:        time.Unix(int64(roundUntil), 0).UTC().Format(time.RFC3339),
		StartBlock: roundStartBlock,
		EndBlock:   roundEndBlock,
	}

	// Build validator rows.
	rows, totalTrueStake := buildValidatorRows(rd.conf, rd.pools)
	log.Printf("active validators: %d", len(rows))
	out.TotalStake = totalTrueStake
	log.Printf("total true stake (active validators): %.2f TON", new(big.Float).Quo(new(big.Float).SetInt(totalTrueStake), big.NewFloat(1e9)))

	// nominatorGroup: fetch pool data and nominator details in parallel per validator.
	entries := make([]model.ValidatorEntry, len(rows))
	nominatorGroup := new(errgroup.Group)

	for i, row := range rows {
		// perBlockNT = rewardPerBlock * trueStake / totalTrueStake
		perBlockNT := new(big.Int)
		if rewardPerBlock.Sign() > 0 && totalTrueStake.Sign() > 0 {
			perBlockNT = utils.MulDiv(rewardPerBlock, row.trueStake, totalTrueStake)
		}

		entries[i] = model.ValidatorEntry{
			Rank:           i + 1,
			Pubkey:         fmt.Sprintf("%x", row.descr.PubKey()),
			EffectiveStake: row.trueStake,
			Weight:         validatorWeight(row.trueStake, totalTrueStake),
			PerBlockReward: perBlockNT,
			Pool:           row.pool,
		}

		if row.poolAddr == nil || !includeNominators {
			continue
		}

		nominatorGroup.Go(func() error {
			poolAddr := *row.poolAddr
			info := resolvePoolAddresses(ctx, pinned, poolAddr)
			entries[i].PoolType = info.poolType
			entries[i].ValidatorAddress = info.validatorAddress
			entries[i].OwnerAddress = info.ownerAddress

			if info.pd == nil || info.poolType != poolTypeNominatorV10 {
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
					total := new(big.Int).Add(new(big.Int).SetUint64(contractBal), rows[i].trueStake)
					entries[i].TotalStake = total
				}
				return nil
			}

			// Nominator Pool: extract metadata and compute per-nominator rewards.
			meta := computeNominatorPoolMeta(info.pd)
			entries[i].ValidatorStake = meta.validatorStake
			entries[i].NominatorsStake = meta.nominatorsStake
			entries[i].TotalStake = meta.totalPoolStake
			entries[i].ValidatorRewardShare = meta.validatorRewardShare
			entries[i].NominatorsCount = meta.nominatorsCount

			if info.pd.Nominators == nil {
				return nil
			}

			totalAmount := info.pd.NominatorsAmount
			// Nominator share of true_stake: nominators don't own the validator's own stake.
			nominatorsStake := new(big.Int).Set(rows[i].trueStake)
			if totalAmount != nil && totalAmount.Cmp(nominatorsStake) < 0 {
				nominatorsStake.Set(totalAmount)
			}

			rewardShare := big.NewInt(int64(info.pd.RewardShare))

			for _, n := range info.pd.Nominators {
				addr := ton.AccountID{Workchain: 0, Address: tlb.Bits256(n.Address)}
				nAmount := new(big.Int).SetUint64(n.Amount)
				var nomWeight float64
				nomPerBlock := new(big.Int)
				nomStaked := new(big.Int)
				if totalAmount != nil && totalAmount.Sign() > 0 {
					nomWeight = utils.InaccurateDivFloat(nAmount, totalAmount)
					// nomStaked = nominatorsStake * nAmount / totalAmount
					nomStaked = utils.MulDiv(nominatorsStake, nAmount, totalAmount)
					// nomShareOfReward = 10000 - rewardShare
					nomShareOfReward := new(big.Int).Sub(big.NewInt(10000), rewardShare)
					// nomPerBlock = rewardPerBlock * nomStaked * nomShareOfReward / (totalTrueStake * 10000)
					nomPerBlock = utils.MulDiv(utils.MulDiv(rewardPerBlock, nomStaked, totalTrueStake), nomShareOfReward, big.NewInt(10000))
				}
				entries[i].Nominators = append(entries[i].Nominators, model.NominatorEntry{
					Address:        addr.ToHuman(true, false),
					Weight:         nomWeight,
					PerBlockReward: nomPerBlock,
					EffectiveStake: nomStaked,
					Stake:          nAmount,
				})
			}
			return nil
		})
	}
	_ = nominatorGroup.Wait()

	out.Validators = entries
	return &out, nil
}
