package service

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"slices"
	"sort"
	"sync"

	"github.com/tonkeeper/tongo/abi"
	"github.com/tonkeeper/tongo/contract/elector"
	"github.com/tonkeeper/tongo/liteapi"
	"github.com/tonkeeper/tongo/tlb"
	"github.com/tonkeeper/tongo/ton"
	"github.com/tonkeeper/tongo/utils"

	"github.com/tonkeeper/validators-statistics/model"
)

// poolTypeCache caches pool types for addresses that don't need fresh data.
// Nominator Pools are NOT cached because we need fresh nominator data each time.
// cachedPoolInfo stores immutable pool metadata for non-Nominator-Pool types.
type cachedPoolInfo struct {
	Type                   string
	ValidatorAddress       tlb.Bits256
	OwnerAddress           tlb.MsgAddress
	ValidatorWalletAddress tlb.MsgAddress
}

var poolTypeCache sync.Map // ton.AccountID → cachedPoolInfo

// Pool type identifiers returned in the API response.
const (
	poolTypeNominatorV10       = "nominator-pool-v1.0"
	poolTypeSingleNominatorV10 = "single-nominator-pool-v1.0"
	poolTypeSingleNominatorV11 = "single-nominator-pool-v1.1"
	poolTypeOther              = "other"
)

// Known contract code hashes for deterministic pool type detection.
func mustDecodeHash(s string) [32]byte {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		panic("invalid hash: " + s)
	}
	return [32]byte(b)
}

var (
	nominatorPoolCodeHash      = mustDecodeHash("9a3ec14bc098f6b44064c305222caea2800f17dda85ee6a8198a7095ede10dcf")
	singleNominatorV10CodeHash = mustDecodeHash("a42ae69eac76ffe0e452d3d4f13d387a14e46c01a5aadba5fc1d893e6c71f5ba")
	singleNominatorV11CodeHash = mustDecodeHash("cc0d39589eb2c0cfe0fde28456657a3bdd3d953955ae3f98f25664ab3c904fbd")
)

// poolTypeByCodeHash returns the pool type for a known code hash, or "" if unknown.
func poolTypeByCodeHash(hash [32]byte) string {
	switch hash {
	case nominatorPoolCodeHash:
		return poolTypeNominatorV10
	case singleNominatorV10CodeHash:
		return poolTypeSingleNominatorV10
	case singleNominatorV11CodeHash:
		return poolTypeSingleNominatorV11
	default:
		return ""
	}
}

var pastElectionsCache struct {
	sync.Mutex
	electionIDs []int64 // sorted electAt values used as cache key
	data        map[tlb.Bits256]poolEntry
}

// poolData holds data returned by GetPoolData + ListNominators for nominator pools.
type poolData struct {
	ValidatorAmount        *big.Int
	NominatorsAmount       *big.Int
	RewardShare            uint32
	NominatorsCount        uint32
	ValidatorAddress       tlb.Bits256
	OwnerAddress           tlb.MsgAddress
	ValidatorWalletAddress tlb.MsgAddress
	Nominators             *abi.ListNominatorsResult
}

// fetchPoolData determines the pool type by contract code hash and, for known
// pool types, fetches enriched pool data.
//
// Detection: a single GetAccountState call retrieves the contract code hash,
// which is matched against known pool contract hashes. This is deterministic
// and avoids multiple heuristic RPC probes.
//
// All types except nominator pools are cached (they have no per-request data).
// Network errors skip detection entirely and return ("", nil) without caching.
func fetchPoolData(ctx context.Context, client *liteapi.Client, poolAddr ton.AccountID) (string, *poolData) {
	// Fast path: confirmed type from a previous call.
	if cached, ok := poolTypeCache.Load(poolAddr); ok {
		info := cached.(cachedPoolInfo)
		if info.ValidatorAddress != (tlb.Bits256{}) || info.OwnerAddress.SumType != "" || info.ValidatorWalletAddress.SumType != "" {
			return info.Type, &poolData{
				ValidatorAddress:       info.ValidatorAddress,
				OwnerAddress:           info.OwnerAddress,
				ValidatorWalletAddress: info.ValidatorWalletAddress,
			}
		}
		return info.Type, nil
	}

	// Fetch account state to determine pool type by code hash (1 RPC call).
	model.CountRPC(ctx)
	st, err := client.GetAccountState(ctx, poolAddr)
	if err != nil {
		return "", nil
	}
	if st.Account.SumType != "Account" {
		return "", nil
	}
	state := st.Account.Account.Storage.State
	if state.SumType != "AccountActive" {
		return "", nil
	}
	code := state.AccountActive.StateInit.Code
	if !code.Exists {
		poolTypeCache.Store(poolAddr, cachedPoolInfo{Type: poolTypeOther})
		return poolTypeOther, nil
	}
	codeHash, err := code.Value.Value.Hash256()
	if err != nil {
		return "", nil
	}

	poolType := poolTypeByCodeHash(codeHash)
	switch poolType {
	case poolTypeNominatorV10:
		// Fetch ListNominators + GetPoolData for full nominator data.
		model.CountRPC(ctx)
		_, lnResult, lnErr := abi.ListNominators(ctx, client, poolAddr)
		if lnErr != nil {
			return poolType, nil
		}
		noms, ok := lnResult.(abi.ListNominatorsResult)
		if !ok {
			return poolType, nil
		}
		pd := &poolData{Nominators: &noms, NominatorsAmount: new(big.Int)}
		for _, n := range noms.Nominators {
			pd.NominatorsAmount.Add(pd.NominatorsAmount, new(big.Int).SetUint64(n.Amount))
		}
		model.CountRPC(ctx)
		_, gpResult, gpErr := abi.GetPoolData(ctx, client, poolAddr)
		if gpErr == nil {
			if tf, ok := gpResult.(abi.GetPoolData_TfResult); ok {
				pd.ValidatorAmount = new(big.Int).SetInt64(tf.ValidatorAmount)
				pd.RewardShare = tf.ValidatorRewardShare
				pd.NominatorsCount = tf.NominatorsCount
				pd.ValidatorAddress = tf.ValidatorAddress
			}
		}
		return poolType, pd

	case poolTypeSingleNominatorV10, poolTypeSingleNominatorV11:
		// Fetch GetPoolData + fetchPoolRoles for validator/owner addresses.
		pd := &poolData{}
		model.CountRPC(ctx)
		_, gpResult, gpErr := abi.GetPoolData(ctx, client, poolAddr)
		if gpErr == nil {
			if tf, ok := gpResult.(abi.GetPoolData_TfResult); ok {
				pd.ValidatorAddress = tf.ValidatorAddress
			}
		}
		if roles, ok := fetchPoolRoles(ctx, client, poolAddr); ok {
			pd.OwnerAddress = roles.OwnerAddress
			pd.ValidatorWalletAddress = roles.ValidatorAddress
		}
		poolTypeCache.Store(poolAddr, cachedPoolInfo{
			Type:                   poolType,
			ValidatorAddress:       pd.ValidatorAddress,
			OwnerAddress:           pd.OwnerAddress,
			ValidatorWalletAddress: pd.ValidatorWalletAddress,
		})
		return poolType, pd

	default:
		// Unknown pool type — no further RPC calls.
		poolTypeCache.Store(poolAddr, cachedPoolInfo{Type: poolTypeOther})
		return poolTypeOther, nil
	}
}

type getRolesResult struct {
	OwnerAddress     tlb.MsgAddress
	ValidatorAddress tlb.MsgAddress
}

// fetchPoolRoles reads owner and validator wallet addresses from get_roles.
func fetchPoolRoles(ctx context.Context, executor abi.Executor, poolAddr ton.AccountID) (getRolesResult, bool) {
	model.CountRPC(ctx)
	errCode, stack, err := executor.RunSmcMethodByID(ctx, poolAddr, utils.MethodIdFromName("get_roles"), tlb.VmStack{})
	if err != nil || (errCode != 0 && errCode != 1) {
		return getRolesResult{}, false
	}

	if len(stack) != 2 {
		return getRolesResult{}, false
	}
	if (stack[0].SumType != "VmStkSlice" && stack[0].SumType != "VmStkNull") ||
		(stack[1].SumType != "VmStkSlice" && stack[1].SumType != "VmStkNull") {
		return getRolesResult{}, false
	}

	var result getRolesResult
	if err := stack.Unmarshal(&result); err != nil {
		return getRolesResult{}, false
	}
	return result, true
}

// frozenMember matches the TL-B layout of a member in past_elections hashmap:
// src_addr:bits256 weight:uint64 true_stake:Grams banned:Bool
type frozenMember struct {
	SrcAddr   tlb.Bits256
	Weight    tlb.Uint64
	TrueStake tlb.Grams
}

// poolEntry holds a validator's pool address and true stake from the frozen election.
type poolEntry struct {
	Addr      ton.AccountID
	TrueStake *big.Int // actual effective stake in nTON, from frozen election leaf
}

// getAllPoolAddresses returns a map from validator pubkey to poolEntry.
func getAllPoolAddresses(ctx context.Context, client *liteapi.Client, electorAddr ton.AccountID) (map[tlb.Bits256]poolEntry, error) {
	// GetParticipantListExtended is informational logging only — fire and forget.
	go func() {
		model.CountRPC(ctx)
		if list, err := elector.GetParticipantListExtended(ctx, electorAddr, client); err == nil && len(list.Validators) > 0 {
			log.Printf("active election: id=%d, participants=%d, totalStake=%.2f TON",
				list.ElectAt, len(list.Validators), float64(list.TotalStake)/1e9)
		}
	}()
	return poolsFromPastElections(ctx, client, electorAddr)
}

// poolsFromPastElections traverses the members hashmap of each past election
// to collect pubkey → poolEntry mappings.
// Frozen member value layout: src_addr (256 bits) | weight (64 bits) | true_stake (Grams) | banned (1 bit).
// Elections are processed in ascending electAt order so the newest data for each pubkey wins.
func poolsFromPastElections(ctx context.Context, client *liteapi.Client, electorAddr ton.AccountID) (map[tlb.Bits256]poolEntry, error) {
	stack, err := retry(func() (tlb.VmStack, error) {
		model.CountRPC(ctx)
		_, stack, err := client.RunSmcMethodByID(ctx, electorAddr, utils.MethodIdFromName("past_elections"), tlb.VmStack{})
		return stack, err
	})
	if err != nil {
		return nil, fmt.Errorf("past_elections: %w", err)
	}
	if len(stack) == 0 {
		return nil, fmt.Errorf("past_elections returned empty stack")
	}

	top := stack[0].VmStkTuple
	elections, err := top.RecursiveToSlice()
	if err != nil {
		return nil, fmt.Errorf("RecursiveToSlice: %w", err)
	}

	// Extract election IDs (cheap) and check cache.
	type rawElection struct {
		electAt int64
		fields  []tlb.VmStackValue
	}
	parsed := make([]rawElection, 0, len(elections))
	ids := make([]int64, 0, len(elections))
	for _, el := range elections {
		elTuple := el.VmStkTuple
		fields, err := elTuple.Data.RecursiveToSlice(int(elTuple.Len))
		if err != nil || len(fields) < 5 {
			continue
		}
		electAt := fields[0].VmStkTinyInt
		ids = append(ids, electAt)
		parsed = append(parsed, rawElection{electAt: electAt, fields: fields})
	}
	slices.Sort(ids)

	pastElectionsCache.Lock()
	if slices.Equal(ids, pastElectionsCache.electionIDs) && pastElectionsCache.data != nil {
		cached := pastElectionsCache.data
		pastElectionsCache.Unlock()
		log.Printf("past elections: cache hit (ids=%v)", ids)
		return cached, nil
	}
	pastElectionsCache.Unlock()

	// Cache miss — full parse of member hashmaps.
	type electionData struct {
		electAt int64
		members tlb.Hashmap[tlb.Bits256, frozenMember]
	}
	var allElections []electionData
	for _, pe := range parsed {
		membersCell := &pe.fields[4].VmStkCell.Value
		var members tlb.Hashmap[tlb.Bits256, frozenMember]
		if err := tlb.Unmarshal(membersCell, &members); err != nil {
			log.Printf("warning: parse members for election %d: %v", pe.electAt, err)
			continue
		}
		log.Printf("past election id=%d: %d members", pe.electAt, len(members.Keys()))
		allElections = append(allElections, electionData{electAt: pe.electAt, members: members})
	}

	sort.Slice(allElections, func(i, j int) bool {
		return allElections[i].electAt < allElections[j].electAt
	})

	merged := make(map[tlb.Bits256]poolEntry)
	for _, ed := range allElections {
		for _, item := range ed.members.Items() {
			merged[item.Key] = poolEntry{
				Addr:      ton.AccountID{Workchain: -1, Address: [32]byte(item.Value.SrcAddr)},
				TrueStake: new(big.Int).SetUint64(uint64(item.Value.TrueStake)),
			}
		}
	}

	pastElectionsCache.Lock()
	pastElectionsCache.electionIDs = ids
	pastElectionsCache.data = merged
	pastElectionsCache.Unlock()

	return merged, nil
}
