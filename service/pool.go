package service

import (
	"context"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/tonkeeper/tongo/abi"
	"github.com/tonkeeper/tongo/contract/elector"
	"github.com/tonkeeper/tongo/liteapi"
	"github.com/tonkeeper/tongo/tlb"
	"github.com/tonkeeper/tongo/ton"
	"github.com/tonkeeper/tongo/utils"

	"github.com/tonkeeper/validators-statistics/model"
)

// poolTypeCache caches pool addresses known to be "Single Nominator"
// so we skip the ListNominators RPC call on subsequent requests.
// Nominator Pools are NOT cached because we need fresh nominator data each time.
var poolTypeCache sync.Map // ton.AccountID → "Single Nominator"

var pastElectionsCache struct {
	sync.Mutex
	electionIDs []int64 // sorted electAt values used as cache key
	data        map[tlb.Bits256]poolEntry
}

// detectPoolType determines the pool contract type and returns nominator data.
// Uses a single ListNominators call:
//   - succeeds with nominators → "Nominator Pool" + nominators
//   - succeeds but no valid result → "Single Nominator" (cached)
//   - RPC error → "Single Nominator" (NOT cached, will retry next request)
//
// Only "Single Nominator" confirmed results are cached; Nominator Pools are
// re-fetched each call to get fresh nominator data.
func detectPoolType(ctx context.Context, executor abi.Executor, poolAddr ton.AccountID) (string, *abi.ListNominatorsResult) {
	// Fast path: confirmed Single Nominator — skip the RPC call.
	if cached, ok := poolTypeCache.Load(poolAddr); ok {
		return cached.(string), nil
	}

	model.CountRPC(ctx)
	_, result, err := abi.ListNominators(ctx, executor, poolAddr)
	if err != nil {
		if strings.Contains(err.Error(), "method execution failed") {
			// TVM error (code 11, 32, etc.) — deterministic, contract doesn't
			// support list_nominators. Cache to avoid repeating on next request.
			poolTypeCache.Store(poolAddr, "Single Nominator")
		}
		// Network errors are NOT cached — will retry on next request.
		return "Single Nominator", nil
	}
	var noms *abi.ListNominatorsResult
	if n, ok := result.(abi.ListNominatorsResult); ok {
		noms = &n
	}

	if noms != nil {
		return "Nominator Pool", noms
	}

	// Call succeeded but returned non-nominator result — confirmed Single Nominator.
	poolTypeCache.Store(poolAddr, "Single Nominator")
	return "Single Nominator", nil
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
	TrueStake uint64 // actual effective stake in nTON, from frozen election leaf
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
				TrueStake: uint64(item.Value.TrueStake),
			}
		}
	}

	pastElectionsCache.Lock()
	pastElectionsCache.electionIDs = ids
	pastElectionsCache.data = merged
	pastElectionsCache.Unlock()

	return merged, nil
}
