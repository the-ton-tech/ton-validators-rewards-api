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

	"github.com/tonkeeper/validators-statistics/model"
	"github.com/tonkeeper/validators-statistics/utils"
)

func getAnchorExt(ctx context.Context, client LiteClient, block_seqno *uint32, election_id *int64) (*ton.BlockIDExt, error) {
	var anchorExt ton.BlockIDExt
	switch {
	case block_seqno != nil:
		ext, _, err := lookupMasterchainBlock(ctx, client, *block_seqno)

		if err != nil {
			return nil, fmt.Errorf("lookupMasterchainBlock(%d): %w", *block_seqno, err)
		}
		anchorExt = ext

	case election_id != nil:
		ext, err := lookupMasterchainBlockByUtime(ctx, client, uint32(*election_id))
		if err != nil {
			return nil, fmt.Errorf("lookupMasterchainBlockByUtime(election_id=%d): %w", *election_id, err)
		}
		anchorExt = ext
	}
	return &anchorExt, nil
}

// FetchRoundRewards computes per-validator and per-nominator reward distribution
// for a finished validation round using the elector's bonuses value.
func (s *Service) FetchRoundRewards(ctx context.Context, query model.RoundRewardsQuery) (*model.RoundRewardsOutput, error) {
	client := s.currentClient()

	anchor, err := getAnchorExt(ctx, client, query.Block, query.ElectionID)
	if err != nil || anchor == nil {
		return nil, fmt.Errorf("getAnchorExt error or nil: %w", err)
	}
	anchorExt := *anchor

	// 2. Resolve round boundaries from config param 34.
	since, until, err := getConfigParam34(ctx, client, anchorExt)
	if err != nil {
		return nil, fmt.Errorf("getConfigParam34: %w", err)
	}
	if since == 0 {
		return nil, fmt.Errorf("config param 34 is empty at block %d", anchorExt.Seqno)
	}

	// 3. Verify the round is finished.
	if time.Unix(int64(until), 0).After(time.Now()) {
		return nil, fmt.Errorf("round %d is not finished yet (ends %s)", since, time.Unix(int64(until), 0).UTC().Format(time.RFC3339))
	}

	// 4. Resolve start_block and end_block.
	startExt, err := lookupMasterchainBlockByUtime(ctx, client, since)
	if err != nil {
		return nil, fmt.Errorf("lookupMasterchainBlockByUtime(since=%d): %w", since, err)
	}
	endExt, err := lookupMasterchainBlockByUtime(ctx, client, until)
	if err != nil {
		return nil, fmt.Errorf("lookupMasterchainBlockByUtime(until=%d): %w", until, err)
	}
	endBlock := endExt.Seqno - 1 // end_block is the last block of this round

	// 5. Pin to end_block + 1 and fetch data in parallel.
	pinnedExt, _, err := lookupMasterchainBlock(ctx, client, endExt.Seqno)
	if err != nil {
		return nil, fmt.Errorf("lookupMasterchainBlock(end+1=%d): %w", endExt.Seqno, err)
	}
	pinned := client.WithBlock(pinnedExt)

	var (
		conf      *ton.BlockchainConfig
		pools     map[tlb.Bits256]poolEntry
		elections []RawPastElection
	)

	g := new(errgroup.Group)

	// Config param 34 at end_block+1 → validator list.
	g.Go(func() error {
		c, err := retry(func() (*ton.BlockchainConfig, error) {
			model.CountRPC(ctx)
			params, err := pinned.GetConfigParams(ctx, 0, []uint32{34})
			if err != nil {
				return nil, fmt.Errorf("GetConfigParams: %w", err)
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

	// Pool addresses and true stakes.
	g.Go(func() error {
		p, err := getAllPoolAddresses(ctx, pinned, electorAddr)
		if err != nil {
			log.Printf("warning: pool addresses: %v", err)
		}
		pools = p
		return nil
	})

	// Past elections → bonuses and total_stake.
	g.Go(func() error {
		parsed, err := fetchRawPastElections(ctx, pinned, electorAddr)
		if err != nil {
			return fmt.Errorf("fetchRawPastElections: %w", err)
		}
		elections = parsed
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// 6. Extract validators.
	validators := extractValidators(conf)
	if len(validators) == 0 {
		return nil, fmt.Errorf("no validators found in config param 34 at block %d", pinnedExt.Seqno)
	}

	// 7. Find matching election and extract bonuses + total_stake.
	electionID := int64(since)
	var bonuses, electionTotalStake *big.Int
	for _, el := range elections {
		if el.ElectAt != electionID {
			continue
		}
		electionTotalStake = el.TotalStake
		bonuses = el.Bonuses
		break
	}
	if bonuses == nil {
		return nil, fmt.Errorf("election %d not found in past_elections or bonuses not available", electionID)
	}

	// 8. Compute total true stake for active validators.
	totalTrueStake := new(big.Int)
	for _, v := range validators {
		if pe, ok := pools[v.PubKey()]; ok {
			totalTrueStake.Add(totalTrueStake, pe.TrueStake)
		}
	}

	if totalTrueStake.Sign() == 0 {
		return nil, fmt.Errorf("total true stake is zero — no pool data available")
	}

	// 9. Build validator rows with rewards.
	type validatorRow struct {
		v         tlb.ValidatorDescr
		trueStake *big.Int
		reward    *big.Int
		pool      string
		poolAddr  *ton.AccountID
	}
	validatorRows := make([]validatorRow, len(validators))
	for i, v := range validators {
		pk := v.PubKey()
		row := validatorRow{v: v, trueStake: new(big.Int), reward: new(big.Int)}
		if pe, ok := pools[pk]; ok {
			row.pool = pe.Addr.ToHuman(true, false)
			addr := pe.Addr
			row.poolAddr = &addr
			row.trueStake.Set(pe.TrueStake)
			// reward = bonuses * trueStake / totalTrueStake
			row.reward = utils.MulDiv(bonuses, pe.TrueStake, totalTrueStake)
		}
		validatorRows[i] = row
	}
	sort.Slice(validatorRows, func(i, j int) bool { return validatorRows[i].trueStake.Cmp(validatorRows[j].trueStake) > 0 })

	// 10. Collect pool data + nominator split (parallel).
	validatorRewards := make([]model.ValidatorReward, len(validatorRows))
	g2 := new(errgroup.Group)

	for i, row := range validatorRows {
		var share float64
		if totalTrueStake.Sign() > 0 {
			share, _ = new(big.Float).Quo(
				new(big.Float).SetInt(row.trueStake),
				new(big.Float).SetInt(totalTrueStake),
			).Float64()
		}
		validatorRewards[i] = model.ValidatorReward{
			Rank:           i + 1,
			Pubkey:         fmt.Sprintf("%x", row.v.PubKey()),
			EffectiveStake: row.trueStake,
			Weight:         share,
			Reward:         row.reward,
			Pool:           row.pool,
		}

		if row.poolAddr == nil {
			continue
		}

		g2.Go(func() error {
			poolAddr := *row.poolAddr
			poolType, pd := fetchPoolData(ctx, pinned, poolAddr)
			validatorRewards[i].PoolType = poolType

			if pd != nil {
				if vAddr, ok := msgAddressToHuman(pd.ValidatorWalletAddress, true); ok {
					validatorRewards[i].ValidatorAddress = vAddr
				} else if pd.ValidatorAddress != (tlb.Bits256{}) {
					vAddr := ton.AccountID{Workchain: -1, Address: [32]byte(pd.ValidatorAddress)}
					validatorRewards[i].ValidatorAddress = vAddr.ToHuman(true, false)
				}
				if ownerAddr, ok := msgAddressToHuman(pd.OwnerAddress, true); ok {
					validatorRewards[i].OwnerAddress = ownerAddr
				}
			}

			if pd == nil || poolType != poolTypeNominatorV10 {
				return nil
			}

			// Nominator Pool: use data from GetPoolData + ListNominators.
			if pd.ValidatorAmount != nil {
				validatorRewards[i].ValidatorStake = pd.ValidatorAmount
			}
			if pd.NominatorsAmount != nil {
				validatorRewards[i].NominatorsStake = pd.NominatorsAmount
			}
			totalPoolStake := new(big.Int) // todo - why we don't get it from state of the elector?
			if pd.ValidatorAmount != nil {
				totalPoolStake.Add(totalPoolStake, pd.ValidatorAmount)
			}
			if pd.NominatorsAmount != nil {
				totalPoolStake.Add(totalPoolStake, pd.NominatorsAmount)
			}
			validatorRewards[i].TotalStake = totalPoolStake
			validatorRewards[i].ValidatorRewardShare = float64(pd.RewardShare) / 10000.0
			validatorRewards[i].NominatorsCount = pd.NominatorsCount

			if pd.Nominators == nil {
				return nil
			}

			// totalAmount := pd.NominatorsAmount
			// // Nominator share of true_stake: nominators don't own the validator's own stake.
			// nominatorsStake := new(big.Int).Set(validatorRows[i].trueStake)

			// // todo - why that can happen?
			// if totalAmount != nil && totalAmount.Cmp(nominatorsStake) < 0 {
			// 	nominatorsStake.Set(totalAmount)
			// }

			if poolType == poolTypeNominatorV10 {
				/**
				int validator_reward = (reward * validator_reward_share) / 10000;
				if (validator_reward > reward) { ;; Theoretical invalid case if validator_reward_share > 10000
					validator_reward = reward;
				}
				validator_amount += validator_reward;
				nominators_reward = reward - validator_reward;
				*/

				rewardShare := big.NewInt(int64(pd.RewardShare))
				tenThousand := big.NewInt(10000)
				totalValidatorReward := validatorRows[i].reward
				validatorSelfReward := big.NewInt(0)

				validatorSelfReward = utils.MulDiv(totalValidatorReward, rewardShare, tenThousand)

				if validatorSelfReward.Cmp(totalValidatorReward) > 0 {
					validatorSelfReward.Set(totalValidatorReward)
				}

				nominatorsReward := new(big.Int).Sub(totalValidatorReward, validatorSelfReward)

				nominatorsTotalStake := pd.NominatorsAmount

				for _, n := range pd.Nominators {
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

					nominatorStakeMulEffectiveStake := utils.MulDiv(
						nominatorStake,
						validatorRows[i].trueStake,
						totalPoolStake,
					)

					validatorRewards[i].Nominators = append(validatorRewards[i].Nominators, model.NominatorReward{
						Address:        addr.ToHuman(true, false),
						Weight:         utils.InaccurateDivFloat(nominatorStake, nominatorsTotalStake),
						Reward:         nominatorReward,
						EffectiveStake: nominatorStakeMulEffectiveStake,
						Stake:          nominatorStake,
					})
				}
			}

			return nil
		})
	}
	_ = g2.Wait()

	out := &model.RoundRewardsOutput{
		ElectionID:   electionID,
		RoundStart:   time.Unix(int64(since), 0).UTC().Format(time.RFC3339),
		RoundEnd:     time.Unix(int64(until), 0).UTC().Format(time.RFC3339),
		StartBlock:   startExt.Seqno,
		EndBlock:     endBlock,
		TotalBonuses: bonuses,
		TotalStake:   electionTotalStake,
		Validators:   validatorRewards,
	}
	out.PrevElectionID = fetchPrevElectionIDForBlock(ctx, client, startExt.Seqno)
	nextID := int64(until)
	out.NextElectionID = &nextID
	return out, nil
}
