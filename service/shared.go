package service

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"sort"

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

// roundData holds data fetched in parallel for a pinned block.
type roundData struct {
	conf      *ton.BlockchainConfig
	pools     map[tlb.Bits256]poolEntry
	elections []RawPastElection
}

// fetchRoundData fetches config param 34, pool addresses, and past elections in parallel.
// All data is read from the same pinned block.
func fetchRoundData(ctx context.Context, pinned LiteClient) (roundData, error) {
	var rd roundData
	g := new(errgroup.Group)

	// Config param 34 → validator list.
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
		rd.conf = c
		return nil
	})

	// Pool addresses and true stakes.
	g.Go(func() error {
		p, err := getAllPoolAddresses(ctx, pinned, electorAddr)
		if err != nil {
			log.Printf("warning: pool addresses: %v", err)
		}
		rd.pools = p
		return nil
	})

	// Past elections → bonuses and total_stake.
	g.Go(func() error {
		parsed, err := fetchRawPastElections(ctx, pinned, electorAddr)
		if err != nil {
			log.Printf("warning: past elections: %v", err)
			return nil
		}
		rd.elections = parsed
		return nil
	})

	if err := g.Wait(); err != nil {
		return rd, err
	}
	return rd, nil
}

// validatorRow holds intermediate per-validator data used by both stats and rewards.
type validatorRow struct {
	pubkey    tlb.Bits256
	trueStake *big.Int
	pool      string
	poolAddr  *ton.AccountID
}

// parseFrozenDict parses a single election's frozen dict into a pools map.
// The frozen dict maps validator pubkey → {src_addr, weight, true_stake}.
func parseFrozenDict(election *RawPastElection) (map[tlb.Bits256]poolEntry, error) {
	if election.FrozenDict.SumType != "VmStkCell" {
		return nil, fmt.Errorf("frozen dict is not a cell (type=%s)", election.FrozenDict.SumType)
	}
	membersCell := &election.FrozenDict.VmStkCell.Value
	var members tlb.Hashmap[tlb.Bits256, frozenMember]
	if err := tlb.Unmarshal(membersCell, &members); err != nil {
		return nil, fmt.Errorf("unmarshal frozen dict: %w", err)
	}
	pools := make(map[tlb.Bits256]poolEntry, len(members.Keys()))
	for _, item := range members.Items() {
		addr := ton.AccountID{Workchain: -1, Address: [32]byte(item.Value.SrcAddr)}
		pools[item.Key] = poolEntry{
			Addr:      addr,
			TrueStake: new(big.Int).SetUint64(uint64(item.Value.TrueStake)),
		}
	}
	return pools, nil
}

// filterPoolsByValidators returns a pools map containing only validators present in config param 34.
func filterPoolsByValidators(conf *ton.BlockchainConfig, pools map[tlb.Bits256]poolEntry) map[tlb.Bits256]poolEntry {
	validators := extractValidators(conf)
	filtered := make(map[tlb.Bits256]poolEntry, len(validators))
	for _, v := range validators {
		pk := v.PubKey()
		if pe, ok := pools[pk]; ok {
			filtered[pk] = pe
		}
	}
	return filtered
}

// buildValidatorRows builds validator rows from a pools map and computes total true stake.
// Returns rows sorted by stake descending.
func buildValidatorRows(pools map[tlb.Bits256]poolEntry) ([]validatorRow, *big.Int) {
	totalTrueStake := new(big.Int)
	rows := make([]validatorRow, 0, len(pools))
	for pk, pe := range pools {
		addr := pe.Addr
		row := validatorRow{
			pubkey:    pk,
			trueStake: new(big.Int).Set(pe.TrueStake),
			pool:      pe.Addr.ToHuman(true, false),
			poolAddr:  &addr,
		}
		totalTrueStake.Add(totalTrueStake, pe.TrueStake)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].trueStake.Cmp(rows[j].trueStake) > 0 })
	return rows, totalTrueStake
}

// computeBaseRewards distributes rewardPool proportionally to each validator's true stake.
// Pure math, no I/O.
func computeBaseRewards(rows []validatorRow, totalTrueStake, rewardPool *big.Int) []model.ValidatorReward {
	rewards := make([]model.ValidatorReward, len(rows))
	for i, row := range rows {
		rewards[i] = model.ValidatorReward{
			Rank:           i + 1,
			Pubkey:         fmt.Sprintf("%x", row.pubkey),
			EffectiveStake: row.trueStake,
			Weight:         stakeWeight(row.trueStake, totalTrueStake),
			Reward:         utils.MulDiv(rewardPool, row.trueStake, totalTrueStake),
			Pool:           row.pool,
		}
	}
	return rewards
}

// splitNominatorRewards splits a validator's total reward between the validator operator
// and their nominators based on rewardShare (basis points) and nominator stakes.
// Pure math, no I/O.
func splitNominatorRewards(totalReward *big.Int, rewardShare uint32, nominators []nominatorData, effectiveStake, totalStake *big.Int) []model.NominatorReward {
	rewardShareBig := big.NewInt(int64(rewardShare))
	tenThousand := big.NewInt(10000)

	validatorSelfReward := utils.MulDiv(totalReward, rewardShareBig, tenThousand)
	if validatorSelfReward.Cmp(totalReward) > 0 {
		validatorSelfReward.Set(totalReward)
	}
	nominatorsReward := new(big.Int).Sub(totalReward, validatorSelfReward)

	nominatorsTotalStake := new(big.Int)
	for _, n := range nominators {
		nominatorsTotalStake.Add(nominatorsTotalStake, new(big.Int).SetUint64(n.Amount))
	}

	result := make([]model.NominatorReward, len(nominators))
	for i, n := range nominators {
		addr := ton.AccountID{Workchain: 0, Address: tlb.Bits256(n.Address)}
		nominatorStake := new(big.Int).SetUint64(n.Amount)
		result[i] = model.NominatorReward{
			Address:        addr.ToHuman(true, false),
			Weight:         utils.InaccurateDivFloat(nominatorStake, nominatorsTotalStake),
			Reward:         utils.MulDiv(nominatorsReward, nominatorStake, nominatorsTotalStake),
			EffectiveStake: utils.MulDiv(nominatorStake, effectiveStake, totalStake),
			Stake:          nominatorStake,
		}
	}
	return result
}

// stakeWeight returns the validator's share of the total true stake.
func stakeWeight(trueStake, totalTrueStake *big.Int) float64 {
	if totalTrueStake.Sign() <= 0 {
		return 0
	}
	return utils.InaccurateDivFloat(trueStake, totalTrueStake)
}

// poolAddressInfo holds resolved pool type and addresses.
type poolAddressInfo struct {
	poolType         string
	validatorAddress string
	ownerAddress     string
	pd               *poolData
}

// resolvePoolAddresses fetches pool data and resolves validator/owner addresses.
func resolvePoolAddresses(ctx context.Context, client LiteClient, poolAddr ton.AccountID) poolAddressInfo {
	poolType, pd := fetchPoolData(ctx, client, poolAddr)
	info := poolAddressInfo{poolType: poolType, pd: pd}

	if pd != nil {
		if vAddr, ok := msgAddressToHuman(pd.ValidatorWalletAddress, true); ok {
			info.validatorAddress = vAddr
		} else if pd.ValidatorAddress != (tlb.Bits256{}) {
			vAddr := ton.AccountID{Workchain: -1, Address: [32]byte(pd.ValidatorAddress)}
			info.validatorAddress = vAddr.ToHuman(true, false)
		}
		if ownerAddr, ok := msgAddressToHuman(pd.OwnerAddress, true); ok {
			info.ownerAddress = ownerAddr
		}
	}

	return info
}

// nominatorPoolMeta holds computed metadata for a nominator pool.
type nominatorPoolMeta struct {
	validatorStake       *big.Int
	nominatorsStake      *big.Int
	totalPoolStake       *big.Int
	validatorRewardShare float64
	nominatorsCount      uint32
}

// computeNominatorPoolMeta extracts nominator pool metadata from pool data.
func computeNominatorPoolMeta(pd *poolData) nominatorPoolMeta {
	meta := nominatorPoolMeta{
		validatorRewardShare: float64(pd.RewardShare) / 10000.0,
		nominatorsCount:      pd.NominatorsCount,
	}
	totalPoolStake := new(big.Int)
	if pd.ValidatorAmount != nil {
		meta.validatorStake = pd.ValidatorAmount
		totalPoolStake.Add(totalPoolStake, pd.ValidatorAmount)
	}
	if pd.NominatorsAmount != nil {
		meta.nominatorsStake = pd.NominatorsAmount
		totalPoolStake.Add(totalPoolStake, pd.NominatorsAmount)
	}
	meta.totalPoolStake = totalPoolStake
	return meta
}

// findElection returns the past election matching the given election ID, or nil.
func findElection(elections []RawPastElection, electAt int64) *RawPastElection {
	for i := range elections {
		if elections[i].ElectAt == electAt {
			return &elections[i]
		}
	}
	return nil
}

// enrichValidatorRewards fetches pool data in parallel and enriches base rewards
// with pool type, addresses, credits, and nominator reward splits.
// This is the I/O layer — the math is in computeBaseRewards and splitNominatorRewards.
func enrichValidatorRewards(ctx context.Context, pinned LiteClient, rewards []model.ValidatorReward, rows []validatorRow) {
	g := new(errgroup.Group)
	for i, row := range rows {
		if row.poolAddr == nil {
			continue
		}
		g.Go(func() error {
			poolAddr := *row.poolAddr
			info := resolvePoolAddresses(ctx, pinned, poolAddr)
			rewards[i].PoolType = info.poolType
			rewards[i].ValidatorAddress = info.validatorAddress
			rewards[i].OwnerAddress = info.ownerAddress

			credit, err := computeReturnedStake(ctx, pinned, poolAddr)
			if err != nil {
				log.Printf("warning: computeReturnedStake(%s): %v", poolAddr.ToRaw(), err)
				credit = new(big.Int)
			}

			totalStake := new(big.Int).Add(row.trueStake, credit)
			rewards[i].TotalStake = totalStake

			if info.pd == nil || info.poolType != poolTypeNominatorV10 {
				return nil
			}

			meta := computeNominatorPoolMeta(info.pd)
			rewards[i].ValidatorStake = meta.validatorStake
			rewards[i].NominatorsStake = meta.nominatorsStake
			rewards[i].ValidatorRewardShare = meta.validatorRewardShare
			rewards[i].NominatorsCount = meta.nominatorsCount

			if info.pd.Nominators == nil {
				return nil
			}

			rewards[i].Nominators = splitNominatorRewards(
				rewards[i].Reward, info.pd.RewardShare,
				info.pd.Nominators, row.trueStake, totalStake,
			)
			return nil
		})
	}
	_ = g.Wait()
}
