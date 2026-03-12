package main

import (
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
)

type jsValidatorsRoundSnapshot struct {
	ElectionID     int64                `json:"election_id"`
	TotalStake     string               `json:"total_stake"`
	RewardPerBlock string               `json:"reward_per_block"`
	Validators     []jsValidatorSnapshot `json:"validators"`
}

type jsValidatorSnapshot struct {
	Rank            int    `json:"rank"`
	Pubkey          string `json:"pubkey"`
	EffectiveStake  string `json:"effective_stake"`
	Weight          float64 `json:"weight"`
	PerBlockReward  string `json:"per_block_reward"`
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
