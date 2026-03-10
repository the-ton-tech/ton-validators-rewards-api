package service

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"sort"
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

	// Compute per-validator rewards and sort.
	type rewardRow struct {
		validatorRow
		reward *big.Int
	}
	rewardRows := make([]rewardRow, len(rows))
	for i, row := range rows {
		// reward = bonuses * trueStake / totalTrueStake
		reward := utils.MulDiv(rewardPerBlock, row.trueStake, totalTrueStake)
		rewardRows[i] = rewardRow{validatorRow: row, reward: reward}
	}
	sort.Slice(rewardRows, func(i, j int) bool { return rewardRows[i].trueStake.Cmp(rewardRows[j].trueStake) > 0 })

	// rewardGroup: fetch pool data and compute nominator reward split in parallel per validator.
	validatorRewards := make([]model.ValidatorReward, len(rewardRows))
	rewardGroup := new(errgroup.Group)

	for i, row := range rewardRows {
		rewardGroup.Go(func() error {
			validatorRewards[i] = model.ValidatorReward{
				Rank:           i + 1,
				Pubkey:         fmt.Sprintf("%x", row.descr.PubKey()),
				EffectiveStake: row.trueStake,
				Weight:         validatorWeight(row.trueStake, totalTrueStake),
				Reward:         row.reward,
				Pool:           row.pool,
			}

			if row.poolAddr == nil {
				return nil
			}

			poolAddr := *row.poolAddr
			info := resolvePoolAddresses(ctx, pinned, poolAddr)
			validatorRewards[i].PoolType = info.poolType
			validatorRewards[i].ValidatorAddress = info.validatorAddress
			validatorRewards[i].OwnerAddress = info.ownerAddress

			// TotalStake from elector: true_stake + credit (leftover balance kept in contract after election)
			credit, err := computeReturnedStake(ctx, pinned, poolAddr)
			if err != nil {
				log.Printf("warning: computeReturnedStake(%s): %v", poolAddr.ToRaw(), err)
				credit = new(big.Int)
			}
			validatorRewards[i].TotalStake = new(big.Int).Add(row.trueStake, credit)

			// return if not a nominator pool, next steps is not applicable
			if info.pd == nil || info.poolType != poolTypeNominatorV10 {
				return nil
			}

			// Nominator Pool: extract metadata and compute per-nominator rewards.
			meta := computeNominatorPoolMeta(info.pd)
			validatorRewards[i].ValidatorStake = meta.validatorStake
			validatorRewards[i].NominatorsStake = meta.nominatorsStake
			validatorRewards[i].ValidatorRewardShare = meta.validatorRewardShare
			validatorRewards[i].NominatorsCount = meta.nominatorsCount

			if info.pd.Nominators == nil {
				return nil
			}

			// Reward split following elector-code.fc:
			// validatorSelfReward = (totalValidatorReward * rewardShare) / 10000
			// nominatorsReward = totalValidatorReward - validator_reward

			rewardShare := big.NewInt(int64(info.pd.RewardShare))
			tenThousand := big.NewInt(10000)
			totalValidatorReward := validatorRewards[i].Reward

			validatorSelfReward := utils.MulDiv(totalValidatorReward, rewardShare, tenThousand)
			// Theoretical invalid case if rewardShare > 10000
			if validatorSelfReward.Cmp(totalValidatorReward) > 0 {
				validatorSelfReward.Set(totalValidatorReward)
			}

			nominatorsReward := new(big.Int).Sub(totalValidatorReward, validatorSelfReward)
			nominatorsTotalStake := info.pd.NominatorsAmount

			for _, n := range info.pd.Nominators {
				addr := ton.AccountID{Workchain: 0, Address: tlb.Bits256(n.Address)}
				nominatorStake := new(big.Int).SetUint64(n.Amount)
				nominatorReward := utils.MulDiv(nominatorsReward, nominatorStake, nominatorsTotalStake)

				// total stake 5
				// effective stake 3
				// weight 3/5 = 0.6

				// nominator stake 4
				// nominator effective stake = 4 * 0.6 = 2.4
				//                             4 * 3 / 5 = 2.4

				// nominator effective stake = nominator stake * effective stake / total stake
				nominatorEffectiveStake := utils.MulDiv(nominatorStake, row.trueStake, nominatorsTotalStake)

				validatorRewards[i].Nominators = append(validatorRewards[i].Nominators, model.NominatorReward{
					Address:        addr.ToHuman(true, false),
					Weight:         utils.InaccurateDivFloat(nominatorStake, nominatorsTotalStake),
					Reward:         nominatorReward,
					EffectiveStake: nominatorEffectiveStake,
					Stake:          nominatorStake,
				})
			}

			return nil
		})
	}
	_ = rewardGroup.Wait()

	out.Validators = buildValidatorEntries(validatorRewards)
	return &out, nil
}

func buildValidatorEntries(validatorRewards []model.ValidatorReward) []model.ValidatorEntry {
	validatorEntries := make([]model.ValidatorEntry, len(validatorRewards))
	for i, validatorReward := range validatorRewards {
		validatorEntries[i] = model.ValidatorEntry{
			Rank:                 validatorReward.Rank,
			Pubkey:               validatorReward.Pubkey,
			EffectiveStake:       validatorReward.EffectiveStake,
			Weight:               validatorReward.Weight,
			PerBlockReward:       validatorReward.Reward,
			Pool:                 validatorReward.Pool,
			PoolType:             validatorReward.PoolType,
			OwnerAddress:         validatorReward.OwnerAddress,
			ValidatorAddress:     validatorReward.ValidatorAddress,
			TotalStake:           validatorReward.TotalStake,
			ValidatorRewardShare: validatorReward.ValidatorRewardShare,
			NominatorsCount:      validatorReward.NominatorsCount,
		}
		if validatorReward.PoolType == poolTypeNominatorV10 {
			validatorEntries[i].ValidatorStake = validatorReward.ValidatorStake
			validatorEntries[i].NominatorsStake = validatorReward.NominatorsStake
			validatorEntries[i].ValidatorRewardShare = validatorReward.ValidatorRewardShare
			validatorEntries[i].NominatorsCount = validatorReward.NominatorsCount
			validatorEntries[i].Nominators = buildNominatorEntries(validatorReward.Nominators)

		}
	}
	return validatorEntries
}

func buildNominatorEntries(nominatorRewards []model.NominatorReward) []model.NominatorEntry {
	nominatorEntries := make([]model.NominatorEntry, len(nominatorRewards))
	for i, nominatorReward := range nominatorRewards {
		nominatorEntries[i] = model.NominatorEntry{
			Address:        nominatorReward.Address,
			Weight:         nominatorReward.Weight,
			PerBlockReward: nominatorReward.Reward,
			EffectiveStake: nominatorReward.EffectiveStake,
			Stake:          nominatorReward.Stake,
		}
	}
	return nominatorEntries
}
