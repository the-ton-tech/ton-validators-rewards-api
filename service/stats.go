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
		conf              *ton.BlockchainConfig
		pools             map[tlb.Bits256]poolEntry
		electorBalance    = new(big.Int)
		rewardPerBlock    = new(big.Int)
		currentElections  []RawPastElection
		previousElections []RawPastElection
	)

	// fetchGroup: parallel fetches for config, pools, elector balance, and per-block reward.
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

	// Config param 34: current validators.
	fetchGroup.Go(func() error {
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
	fetchGroup.Go(func() error {
		p, err := getAllPoolAddresses(ctx, pinned, electorAddr)
		if err != nil {
			log.Printf("warning: pool addresses: %v", err)
		}
		pools = p
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

	// Past elections at current block (for bonus diff calculation).
	fetchGroup.Go(func() error {
		parsed, err := fetchRawPastElections(ctx, pinned, electorAddr)
		if err == nil {
			currentElections = parsed
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
	roundSince, roundUntil := getRoundInfo(conf)

	// Per-block reward = bonus diff between current and previous block in elector.
	electionID := int64(roundSince)
	var currentBonuses, prevBonuses *big.Int
	for _, el := range currentElections {
		if el.ElectAt == electionID {
			currentBonuses = el.Bonuses
			break
		}
	}
	for _, el := range previousElections {
		if el.ElectAt == electionID {
			prevBonuses = el.Bonuses
			break
		}
	}
	if currentBonuses != nil && prevBonuses != nil {
		rewardPerBlock.Sub(currentBonuses, prevBonuses)
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

	// Current validators.
	validators := extractValidators(conf)
	log.Printf("active validators: %d", len(validators))

	// Total true stake: sum only for current active validators.
	totalTrueStake := new(big.Int)
	for _, v := range validators {
		if pe, ok := pools[v.PubKey()]; ok {
			totalTrueStake.Add(totalTrueStake, pe.TrueStake)
		}
	}
	out.TotalStake = totalTrueStake
	log.Printf("total true stake (active validators): %.2f TON", new(big.Float).Quo(new(big.Float).SetInt(totalTrueStake), big.NewFloat(1e9)))

	type validatorRow struct {
		v          tlb.ValidatorDescr
		trueStake  *big.Int
		perBlockNT *big.Int
		pool       string
		poolAddr   *ton.AccountID
	}
	rows := make([]validatorRow, len(validators))
	for i, v := range validators {
		pk := v.PubKey()
		row := validatorRow{v: v, trueStake: new(big.Int), perBlockNT: new(big.Int)}
		if pe, ok := pools[pk]; ok {
			row.pool = pe.Addr.ToHuman(true, false)
			addr := pe.Addr
			row.poolAddr = &addr
			row.trueStake.Set(pe.TrueStake)
			if rewardPerBlock.Sign() > 0 && totalTrueStake.Sign() > 0 {
				// perBlockNT = rewardPerBlock * trueStake / totalTrueStake
				row.perBlockNT.Div(new(big.Int).Mul(rewardPerBlock, pe.TrueStake), totalTrueStake)
			}
		}
		rows[i] = row
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].trueStake.Cmp(rows[j].trueStake) > 0 })

	// nominatorGroup: fetch pool data and nominator details in parallel per validator.
	entries := make([]model.ValidatorEntry, len(rows))
	nominatorGroup := new(errgroup.Group)

	for i, row := range rows {
		pk := row.v.PubKey()
		_ = pk
		var share float64
		if totalTrueStake.Sign() > 0 {
			share, _ = new(big.Float).Quo(
				new(big.Float).SetInt(row.trueStake),
				new(big.Float).SetInt(totalTrueStake),
			).Float64()
		}
		entries[i] = model.ValidatorEntry{
			Rank:           i + 1,
			Pubkey:         fmt.Sprintf("%x", row.v.PubKey()),
			EffectiveStake: row.trueStake,
			Weight:         share,
			PerBlockReward: row.perBlockNT,
			Pool:           row.pool,
		}

		if row.poolAddr == nil || !includeNominators {
			continue
		}

		nominatorGroup.Go(func() error {
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

			if pd == nil || poolType != poolTypeNominatorV10 {
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

			// Nominator Pool: use data from GetPoolData + ListNominators.
			if pd.ValidatorAmount != nil {
				entries[i].ValidatorStake = pd.ValidatorAmount
			}
			if pd.NominatorsAmount != nil {
				entries[i].NominatorsStake = pd.NominatorsAmount
			}
			totalPoolStake := new(big.Int)
			if pd.ValidatorAmount != nil {
				totalPoolStake.Add(totalPoolStake, pd.ValidatorAmount)
			}
			if pd.NominatorsAmount != nil {
				totalPoolStake.Add(totalPoolStake, pd.NominatorsAmount)
			}
			entries[i].TotalStake = totalPoolStake
			entries[i].ValidatorRewardShare = float64(pd.RewardShare) / 10000.0
			entries[i].NominatorsCount = pd.NominatorsCount

			if pd.Nominators == nil {
				return nil
			}

			totalAmount := pd.NominatorsAmount
			// Nominator share of true_stake: nominators don't own the validator's own stake.
			nominatorsStake := new(big.Int).Set(rows[i].trueStake)
			if totalAmount != nil && totalAmount.Cmp(nominatorsStake) < 0 {
				nominatorsStake.Set(totalAmount)
			}

			rewardShare := big.NewInt(int64(pd.RewardShare))

			for _, n := range pd.Nominators {
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
					// equal to (rewardPerBlock*nomStaked/totalTrueStake)*nomShareOfReward/10000
					// Complete formula: rewardPerBlock * nomStaked * (10000 - rewardShare) / (totalTrueStake * 10000)
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
