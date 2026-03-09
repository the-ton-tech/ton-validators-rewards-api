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
	descr     tlb.ValidatorDescr
	trueStake *big.Int
	pool      string
	poolAddr  *ton.AccountID
}

// buildValidatorRows extracts validators from config, computes total true stake,
// and returns rows sorted by stake descending.
func buildValidatorRows(conf *ton.BlockchainConfig, pools map[tlb.Bits256]poolEntry) ([]validatorRow, *big.Int) {
	validators := extractValidators(conf)

	totalTrueStake := new(big.Int)
	for _, v := range validators {
		if pe, ok := pools[v.PubKey()]; ok {
			totalTrueStake.Add(totalTrueStake, pe.TrueStake)
		}
	}

	rows := make([]validatorRow, len(validators))
	for i, v := range validators {
		pk := v.PubKey()
		row := validatorRow{descr: v, trueStake: new(big.Int)}
		if pe, ok := pools[pk]; ok {
			row.pool = pe.Addr.ToHuman(true, false)
			addr := pe.Addr
			row.poolAddr = &addr
			row.trueStake.Set(pe.TrueStake)
		}
		rows[i] = row
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].trueStake.Cmp(rows[j].trueStake) > 0 })

	return rows, totalTrueStake
}

// validatorWeight returns the validator's share of the total true stake.
func validatorWeight(trueStake, totalTrueStake *big.Int) float64 {
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
