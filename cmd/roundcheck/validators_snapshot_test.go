package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/the-ton-tech/ton-validators-rewards-api/utils"
	"github.com/tonkeeper/tongo/ton"
)

type jsValidatorsRoundSnapshot struct {
	ElectionID     int64                 `json:"election_id"`
	TotalStake     string                `json:"total_stake"`
	RewardPerBlock string                `json:"reward_per_block"`
	ElectorBalance string                `json:"elector_balance"`
	Validators     []jsValidatorSnapshot `json:"validators"`
}

type jsValidatorSnapshot struct {
	Rank           int     `json:"rank"`
	Pubkey         string  `json:"pubkey"`
	EffectiveStake string  `json:"effective_stake"`
	Weight         float64 `json:"weight"`
	PerBlockReward string  `json:"per_block_reward"`
	Pool           string  `json:"pool"`
	PoolType       string  `json:"pool_type"`
	TotalStake     string  `json:"total_stake"`
}

func TestSnapshotsValidateComputedValidatorStats(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}

	snapshotDir := filepath.Join(filepath.Dir(currentFile), "..", "..", "tests", "snapshots", "js_validators_rounds")

	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		t.Fatalf("read snapshot directory %s: %v", snapshotDir, err)
	}

	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		found++

		t.Run(name, func(t *testing.T) {
			path := filepath.Join(snapshotDir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read snapshot %s: %v", path, err)
			}

			var in jsValidatorsRoundSnapshot
			if err := json.Unmarshal(data, &in); err != nil {
				t.Fatalf("parse snapshot %s: %v", path, err)
			}

			if len(in.Validators) == 0 {
				t.Fatalf("snapshot %s: validators list is empty", path)
			}

			totalStake := parseBigInt(t, in.TotalStake, "total_stake", path)
			if totalStake.Sign() <= 0 {
				t.Fatalf("snapshot %s: total_stake must be positive", path)
			}

			rewardPool := parseBigInt(t, in.RewardPerBlock, "reward_per_block", path)

			type validatorForCalc struct {
				Pubkey         string
				EffectiveStake *big.Int
				Rank           int
			}

			rows := make([]validatorForCalc, 0, len(in.Validators))
			for _, v := range in.Validators {
				rows = append(rows, validatorForCalc{
					Pubkey:         v.Pubkey,
					EffectiveStake: parseBigInt(t, v.EffectiveStake, fmt.Sprintf("validators[%s].effective_stake", v.Pubkey), path),
					Rank:           v.Rank,
				})
			}

			sort.Slice(rows, func(i, j int) bool {
				if cmp := rows[i].EffectiveStake.Cmp(rows[j].EffectiveStake); cmp != 0 {
					return cmp > 0
				}
				return rows[i].Pubkey < rows[j].Pubkey
			})

			snapshotByPubkey := make(map[string]jsValidatorSnapshot, len(in.Validators))
			for _, v := range in.Validators {
				snapshotByPubkey[v.Pubkey] = v
			}

			byStakeSum := new(big.Int)
			computedRewardSum := new(big.Int)
			for i, row := range rows {
				v, ok := snapshotByPubkey[row.Pubkey]
				if !ok {
					t.Fatalf("snapshot %s: missing validator %s", path, row.Pubkey)
				}

				byStakeSum.Add(byStakeSum, row.EffectiveStake)
				computedReward := utils.MulDiv(rewardPool, row.EffectiveStake, totalStake)
				computedRewardSum.Add(computedRewardSum, computedReward)

				t.Logf("snapshot %s: effective stake compare %s expected=%s computed=%s", path, row.Pubkey, v.EffectiveStake, row.EffectiveStake.String())

				if v.Rank != i+1 {
					t.Errorf("snapshot %s: validator %s rank mismatch: expected %d, got %d", path, row.Pubkey, i+1, v.Rank)
				}

				if v.EffectiveStake != row.EffectiveStake.String() {
					t.Errorf("snapshot %s: validator %s effective_stake mismatch: expected %s, got %s", path, row.Pubkey, v.EffectiveStake, row.EffectiveStake.String())
				}

				expectedWeight := utils.InaccurateDivFloat(row.EffectiveStake, totalStake)
				if math.Abs(expectedWeight-v.Weight) > 1e-12 {
					t.Errorf("snapshot %s: validator %s weight mismatch: expected %.18f, got %.18f", path, row.Pubkey, expectedWeight, v.Weight)
				}

				if v.PerBlockReward != computedReward.String() {
					t.Errorf("snapshot %s: validator %s per_block_reward mismatch: expected %s, got %s", path, row.Pubkey, computedReward.String(), v.PerBlockReward)
				}
			}

			if byStakeSum.Cmp(totalStake) != 0 {
				t.Fatalf("snapshot %s: total stake mismatch: validators sum %s, snapshot field %s", path, byStakeSum, totalStake)
			}

			if computedRewardSum.Cmp(rewardPool) > 0 {
				t.Fatalf("snapshot %s: computed per-block reward sum %s exceeds reward_per_block %s", path, computedRewardSum, rewardPool)
			}

			// elector_balance must be present and positive.
			electorBalance := parseBigInt(t, in.ElectorBalance, "elector_balance", path)
			if electorBalance.Sign() <= 0 {
				t.Errorf("snapshot %s: elector_balance must be positive, got %s", path, electorBalance)
			}

			for _, v := range in.Validators {
				// pubkey must be a 64-character lowercase hex string (256-bit key).
				if len(v.Pubkey) != 64 {
					t.Errorf("snapshot %s: validator rank %d: pubkey %q has length %d, want 64", path, v.Rank, v.Pubkey, len(v.Pubkey))
				} else if _, err := hex.DecodeString(v.Pubkey); err != nil {
					t.Errorf("snapshot %s: validator rank %d: pubkey %q is not valid hex: %v", path, v.Rank, v.Pubkey, err)
				}

				// pool must be present and a valid TON AccountID.
				if v.Pool == "" {
					t.Errorf("snapshot %s: validator rank %d (pubkey %s): pool is empty", path, v.Rank, v.Pubkey)
				} else if _, err := ton.ParseAccountID(v.Pool); err != nil {
					t.Errorf("snapshot %s: validator rank %d: pool %q is not a valid TON address: %v", path, v.Rank, v.Pool, err)
				}

				// pool_type must be non-empty.
				if v.PoolType == "" {
					t.Errorf("snapshot %s: validator rank %d (pubkey %s): pool_type is empty", path, v.Rank, v.Pubkey)
				}

				// For all pool types except nominator-pool-v1.0, total_stake is trueStake + credit
				// (credit >= 0), so total_stake >= effective_stake must hold.
				// nominator-pool-v1.0 stores only the pool contract's internal balance in total_stake,
				// which is less than the frozen election stake, so it is excluded from this check.
				if v.PoolType != "nominator-pool-v1.0" && v.TotalStake != "" {
					effectiveStake := parseBigInt(t, v.EffectiveStake, fmt.Sprintf("validators[%s].effective_stake", v.Pubkey), path)
					totalStakeV := parseBigInt(t, v.TotalStake, fmt.Sprintf("validators[%s].total_stake", v.Pubkey), path)
					if totalStakeV.Cmp(effectiveStake) < 0 {
						t.Errorf("snapshot %s: validator rank %d (%s, pool_type=%s): total_stake %s < effective_stake %s",
							path, v.Rank, v.Pubkey, v.PoolType, totalStakeV, effectiveStake)
					}
				}
			}
		})
	}

	if found == 0 {
		t.Fatal("no snapshot files found in tests/snapshots/js_validators_rounds")
	}
}

func parseBigInt(t *testing.T, value, name, path string) *big.Int {
	t.Helper()
	v := new(big.Int)
	if _, ok := v.SetString(value, 10); !ok {
		t.Fatalf("snapshot %s: invalid %s %q", path, name, value)
	}
	return v
}
