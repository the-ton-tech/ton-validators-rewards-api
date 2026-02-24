package service

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"

	"github.com/tonkeeper/tongo/abi"
	"github.com/tonkeeper/tongo/boc"
	"github.com/tonkeeper/tongo/contract/elector"
	"github.com/tonkeeper/tongo/liteapi"
	"github.com/tonkeeper/tongo/tlb"
	"github.com/tonkeeper/tongo/ton"
	"github.com/tonkeeper/tongo/utils"
)

// poolTypeCache caches pool type by address. Contract code never changes,
// so a successful detection is cached indefinitely.
var poolTypeCache sync.Map // ton.AccountID → string

// detectPoolType determines the pool contract type by probing its get methods
// and returns any nominator data found in the same call:
//   - list_nominators exists → "Nominator Pool" + nominators
//   - get_roles exists       → "Single Nominator"
//   - neither                → "Other"
func detectPoolType(ctx context.Context, executor abi.Executor, poolAddr ton.AccountID) (string, *abi.ListNominatorsResult) {
	if cached, ok := poolTypeCache.Load(poolAddr); ok {
		poolType := cached.(string)
		if poolType == "Nominator Pool" {
			// Re-fetch nominators (data changes per block, only type is cached).
			countRPC(ctx)
			if _, result, err := abi.ListNominators(ctx, executor, poolAddr); err == nil {
				if noms, ok := result.(abi.ListNominatorsResult); ok {
					return poolType, &noms
				}
			}
		}
		return poolType, nil
	}

	countRPC(ctx)
	if _, result, err := abi.ListNominators(ctx, executor, poolAddr); err == nil {
		if noms, ok := result.(abi.ListNominatorsResult); ok {
			poolTypeCache.Store(poolAddr, "Nominator Pool")
			return "Nominator Pool", &noms
		}
	}

	// get_roles is specific to Single Nominator contracts.
	countRPC(ctx)
	errCode, _, err := executor.RunSmcMethodByID(ctx, poolAddr, utils.MethodIdFromName("get_roles"), tlb.VmStack{})
	if err == nil && errCode == 0 {
		poolTypeCache.Store(poolAddr, "Single Nominator")
		return "Single Nominator", nil
	}

	poolTypeCache.Store(poolAddr, "Other")
	return "Other", nil
}

// poolEntry holds a validator's pool address and true stake from the frozen election.
type poolEntry struct {
	Addr      ton.AccountID
	TrueStake uint64 // actual effective stake in nTON, from frozen election leaf
}

// getAllPoolAddresses returns a map from validator pubkey to poolEntry.
func getAllPoolAddresses(ctx context.Context, client *liteapi.Client, electorAddr ton.AccountID) (map[tlb.Bits256]poolEntry, error) {
	countRPC(ctx)
	if list, err := elector.GetParticipantListExtended(ctx, electorAddr, client); err == nil && len(list.Validators) > 0 {
		log.Printf("active election: id=%d, participants=%d, totalStake=%.2f TON",
			list.ElectAt, len(list.Validators), float64(list.TotalStake)/1e9)
	}
	return poolsFromPastElections(ctx, client, electorAddr)
}

// poolsFromPastElections traverses the members hashmap of each past election
// to collect pubkey → poolEntry mappings.
// Frozen member value layout: src_addr (256 bits) | weight (64 bits) | true_stake (Grams) | banned (1 bit).
// Elections are processed in ascending electAt order so the newest data for each pubkey wins.
func poolsFromPastElections(ctx context.Context, client *liteapi.Client, electorAddr ton.AccountID) (map[tlb.Bits256]poolEntry, error) {
	countRPC(ctx)
	_, stack, err := client.RunSmcMethodByID(ctx, electorAddr, utils.MethodIdFromName("past_elections"), tlb.VmStack{})
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

	type electionData struct {
		electAt int64
		leaves  map[tlb.Bits256]*boc.Cell
	}
	var allElections []electionData
	for _, el := range elections {
		elTuple := el.VmStkTuple
		fields, err := elTuple.Data.RecursiveToSlice(int(elTuple.Len))
		if err != nil || len(fields) < 5 {
			continue
		}
		electAt := fields[0].VmStkTinyInt
		membersCell := &fields[4].VmStkCell.Value
		leaves := make(map[tlb.Bits256]*boc.Cell)
		traverseHashmap(256, membersCell, nil, leaves)
		log.Printf("past election id=%d: %d members", electAt, len(leaves))
		allElections = append(allElections, electionData{electAt: electAt, leaves: leaves})
	}

	// Sort ascending so the newest election is processed last and wins (last-write-wins).
	sort.Slice(allElections, func(i, j int) bool {
		return allElections[i].electAt < allElections[j].electAt
	})

	merged := make(map[tlb.Bits256]poolEntry)
	for _, ed := range allElections {
		for pubkey, leaf := range ed.leaves {
			if leaf.BitsAvailableForRead() < 256 {
				continue
			}
			// src_addr: 256 bits
			var addrBytes [32]byte
			for i := range addrBytes {
				b, _ := leaf.ReadUint(8)
				addrBytes[i] = byte(b)
			}
			entry := poolEntry{Addr: ton.AccountID{Workchain: -1, Address: addrBytes}}
			// skip weight (64 bits)
			if leaf.BitsAvailableForRead() >= 64 {
				leaf.ReadUint(64) //nolint:errcheck
			}
			// true_stake: Grams = 4-bit length L, then L*8 bits of value
			if leaf.BitsAvailableForRead() >= 4 {
				gramsLen, err := leaf.ReadUint(4)
				if err == nil && gramsLen > 0 && leaf.BitsAvailableForRead() >= int(gramsLen)*8 {
					v, err := leaf.ReadUint(int(gramsLen) * 8)
					if err == nil {
						entry.TrueStake = v
					}
				}
			}
			merged[pubkey] = entry
		}
	}
	return merged, nil
}

// traverseHashmap recursively visits every leaf of a TL-B Hashmap.
func traverseHashmap(keyBitsLeft int, c *boc.Cell, prefix []bool, out map[tlb.Bits256]*boc.Cell) {
	first, err := c.ReadBit()
	if err != nil {
		return
	}

	var label []bool
	if !first {
		// hml_short: unary-encoded label length.
		n, err := c.ReadUnary()
		if err != nil {
			return
		}
		for i := 0; i < int(n); i++ {
			b, err := c.ReadBit()
			if err != nil {
				return
			}
			label = append(label, b)
		}
	} else {
		second, err := c.ReadBit()
		if err != nil {
			return
		}
		if !second {
			// hml_long: explicit length then individual bits.
			n, err := c.ReadLimUint(keyBitsLeft)
			if err != nil {
				return
			}
			for i := 0; i < int(n); i++ {
				b, err := c.ReadBit()
				if err != nil {
					return
				}
				label = append(label, b)
			}
		} else {
			// hml_same: one repeated bit value.
			bitVal, err := c.ReadBit()
			if err != nil {
				return
			}
			n, err := c.ReadLimUint(keyBitsLeft)
			if err != nil {
				return
			}
			for i := 0; i < int(n); i++ {
				label = append(label, bitVal)
			}
		}
	}

	currentKey := make([]bool, len(prefix)+len(label))
	copy(currentKey, prefix)
	copy(currentKey[len(prefix):], label)
	keyBitsLeft -= len(label)

	if keyBitsLeft == 0 {
		if len(currentKey) != 256 {
			return
		}
		var key tlb.Bits256
		for i, b := range currentKey {
			if b {
				key[i/8] |= 1 << (7 - uint(i%8))
			}
		}
		out[key] = c
		return
	}

	left, err := c.NextRef()
	if err != nil {
		return
	}
	right, err := c.NextRef()
	if err != nil {
		return
	}

	leftKey := make([]bool, len(currentKey)+1)
	copy(leftKey, currentKey)
	leftKey[len(currentKey)] = false
	traverseHashmap(keyBitsLeft-1, left, leftKey, out)

	rightKey := make([]bool, len(currentKey)+1)
	copy(rightKey, currentKey)
	rightKey[len(currentKey)] = true
	traverseHashmap(keyBitsLeft-1, right, rightKey, out)
}
