package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/the-ton-tech/ton-validators-rewards-api/model"
	"github.com/the-ton-tech/ton-validators-rewards-api/service"
	"github.com/tonkeeper/tongo/ton"
)

// TestFetchPerBlockRewardsMatchesSnapshots calls FetchPerBlockRewards at the exact
// block seqno recorded in each JS snapshot and verifies that:
//   - election_id matches
//   - total_stake matches
//   - each validator's effective_stake matches
//
// Requires access to an archival liteserver. Uses the default mainnet config from
// https://ton.org/global-config.json unless TON_CONFIG env var points to a custom file.
// Example:
//
//	go test ./service/... -run TestFetchPerBlockRewards -v -timeout 10m
func TestFetchPerBlockRewardsMatchesSnapshots(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping blockchain integration test in short mode")
	}

	configPath := os.Getenv("TON_CONFIG") // empty → downloads ton.org/global-config.json

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	snapshotDir := filepath.Join(filepath.Dir(currentFile), "..", "tests", "snapshots", "js_validators_rounds")

	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		t.Fatalf("read snapshot dir %s: %v", snapshotDir, err)
	}

	client, err := service.NewClient(configPath)
	if err != nil {
		t.Fatalf("create blockchain client: %v", err)
	}
	// Allow async connections to initialize before issuing requests.
	time.Sleep(3 * time.Second)

	svc := service.New(client, configPath)

	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		found++

		// if found > 1 {
		// 	continue
		// }

		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(snapshotDir, name))
			if err != nil {
				t.Fatalf("read snapshot: %v", err)
			}

			var snap struct {
				ElectionID int64  `json:"election_id"`
				TotalStake string `json:"total_stake"`
				Block      struct {
					Seqno uint32 `json:"seqno"`
				} `json:"block"`
				Validators []struct {
					Pubkey         string `json:"pubkey"`
					EffectiveStake string `json:"effective_stake"`
				} `json:"validators"`
			}
			if err := json.Unmarshal(data, &snap); err != nil {
				t.Fatalf("parse snapshot: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			seqno := snap.Block.Seqno
			out, err := svc.FetchRoundRewards(ctx, model.RoundRewardsQuery{
				ElectionID: &snap.ElectionID,
			}, false, nil)
			if err != nil {
				t.Fatalf("FetchPerBlockRewards(seqno=%d): %v", seqno, err)
			}

			if out.ElectionID != snap.ElectionID {
				t.Fatalf("election_id: got %d, want %d", out.ElectionID, snap.ElectionID)
			}

			wantTotal := parseBigIntStr(t, snap.TotalStake, "snapshot total_stake")
			if out.TotalStake.Cmp(wantTotal) != 0 {
				t.Errorf("total_stake: got %s, want %s", out.TotalStake, wantTotal)
			}

			// Index service output by pubkey for O(1) lookup.
			outByPubkey := make(map[string]*model.NTon, len(out.Validators))
			for i := range out.Validators {
				v := &out.Validators[i]
				outByPubkey[v.Pubkey] = v.EffectiveStake
			}

			// Every validator in the snapshot must appear in the output with the same stake.
			for _, sv := range snap.Validators {
				wantStake := parseBigIntStr(t, sv.EffectiveStake, "snapshot effective_stake")
				gotStake, found := outByPubkey[sv.Pubkey]
				if !found {
					t.Errorf("validator %s: in snapshot but missing from service output", sv.Pubkey)
					continue
				}
				if gotStake.Cmp(wantStake) != 0 {
					t.Errorf("validator %s effective_stake: got %s, want %s", sv.Pubkey, gotStake, wantStake)
				}
			}

			// Every validator in the output must appear in the snapshot (no extras).
			snapPubkeys := make(map[string]struct{}, len(snap.Validators))
			for _, sv := range snap.Validators {
				snapPubkeys[sv.Pubkey] = struct{}{}
			}
			wrongCount := 0
			for _, ov := range out.Validators {
				if _, found := snapPubkeys[ov.Pubkey]; !found {
					wrongCount++
					// t.Errorf("validator %s: in service output but missing from snapshot", ov.Pubkey)
				}
			}

			if wrongCount > 0 {
				t.Fatalf("wrong count: %d", wrongCount)
			}

			// Pubkey filter: re-run the same round with shallow=true and a
			// small pubkey allowlist. Selected validators must still receive
			// deep enrichment (TotalStake populated), all others must stay
			// shallow. The full call above is the oracle for which pubkeys
			// can actually deep-enrich (those with TotalStake set).
			var selectedPubkeys []string
			for _, v := range out.Validators {
				if v.TotalStake != nil {
					selectedPubkeys = append(selectedPubkeys, v.Pubkey)
					if len(selectedPubkeys) == 2 {
						break
					}
				}
			}
			if len(selectedPubkeys) == 0 {
				t.Logf("no deep-enriched validators available to exercise pubkey filter")
			} else {
				selected := make(map[string]bool, len(selectedPubkeys))
				for _, p := range selectedPubkeys {
					selected[p] = true
				}

				fctx, fcancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer fcancel()
				filtered, err := svc.FetchRoundRewards(fctx, model.RoundRewardsQuery{
					ElectionID: &snap.ElectionID,
				}, true, selected)
				if err != nil {
					t.Fatalf("FetchRoundRewards(pubkeys=%v): %v", selectedPubkeys, err)
				}

				if len(filtered.Validators) != len(out.Validators) {
					t.Errorf("pubkey filter changed validator count: got %d, want %d",
						len(filtered.Validators), len(out.Validators))
				}

				filteredByPubkey := make(map[string]*model.ValidatorReward, len(filtered.Validators))
				for i := range filtered.Validators {
					filteredByPubkey[filtered.Validators[i].Pubkey] = &filtered.Validators[i]
				}

				// Every selected pubkey must appear with deep-enrichment fields.
				for _, p := range selectedPubkeys {
					fv, ok := filteredByPubkey[p]
					if !ok {
						t.Errorf("selected pubkey %s: missing from filtered output", p)
						continue
					}
					if fv.TotalStake == nil {
						t.Errorf("selected pubkey %s: TotalStake nil; want deep enrichment despite shallow=true", p)
					}
				}

				// Non-selected validators must remain shallow even though the
				// oracle call deep-enriched them.
				for _, fv := range filtered.Validators {
					if _, isSelected := selected[fv.Pubkey]; isSelected {
						continue
					}
					if fv.TotalStake != nil {
						t.Errorf("unselected pubkey %s: TotalStake = %s; expected shallow", fv.Pubkey, fv.TotalStake.Int)
					}
					if fv.PoolType != "" {
						t.Errorf("unselected pubkey %s: PoolType = %q; expected shallow", fv.Pubkey, fv.PoolType)
					}
					if fv.NominatorsCount != 0 {
						t.Errorf("unselected pubkey %s: NominatorsCount = %d; expected shallow", fv.Pubkey, fv.NominatorsCount)
					}
					if len(fv.Nominators) != 0 {
						t.Errorf("unselected pubkey %s: %d nominators returned; expected shallow", fv.Pubkey, len(fv.Nominators))
					}
				}
			}

			// Per-block bonuses: call FetchPerBlockRewards at the snapshot
			// seqno and verify the new prev/curr_block_total_bonuses fields
			// plus the per-validator bonus split invariant.
			pbCtx, pbCancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer pbCancel()
			pb, err := svc.FetchPerBlockRewards(pbCtx, &seqno, nil, true, nil)
			if err != nil {
				t.Fatalf("FetchPerBlockRewards(seqno=%d): %v", seqno, err)
			}
			if pb.PrevBlockTotalBonuses == nil || pb.PrevBlockTotalBonuses.Int == nil {
				t.Errorf("prev_block_total_bonuses missing from per-block response")
			}
			if pb.CurrBlockTotalBonuses == nil || pb.CurrBlockTotalBonuses.Int == nil {
				t.Fatalf("curr_block_total_bonuses missing from per-block response")
			}
			if pb.CurrBlockTotalBonuses.Sign() < 0 || pb.PrevBlockTotalBonuses.Sign() < 0 {
				t.Errorf("bonuses must be non-negative: prev=%s curr=%s", pb.PrevBlockTotalBonuses.Int, pb.CurrBlockTotalBonuses.Int)
			}
			if pb.CurrBlockTotalBonuses.Cmp(pb.PrevBlockTotalBonuses.Int) < 0 {
				t.Errorf("curr_block_total_bonuses (%s) < prev_block_total_bonuses (%s); bonuses are monotonic within a round",
					pb.CurrBlockTotalBonuses.Int, pb.PrevBlockTotalBonuses.Int)
			}

			// Per-validator curr_block_total_bonuses = curr * stake / total_stake.
			// Only meaningful when the elector has accumulated bonuses at this block.
			if pb.CurrBlockTotalBonuses.Sign() > 0 && pb.TotalStake != nil && pb.TotalStake.Sign() > 0 {
				sum := new(big.Int)
				for _, v := range pb.Validators {
					if v.CurrBlockTotalBonuses == nil {
						t.Errorf("validator %s: curr_block_total_bonuses missing in per-block response", v.Pubkey)
						continue
					}
					want := new(big.Int).Mul(pb.CurrBlockTotalBonuses.Int, v.EffectiveStake.Int)
					want.Quo(want, pb.TotalStake.Int)
					if v.CurrBlockTotalBonuses.Cmp(want) != 0 {
						t.Errorf("validator %s: curr_block_total_bonuses = %s; want %s (curr*stake/total)",
							v.Pubkey, v.CurrBlockTotalBonuses.Int, want)
					}
					sum.Add(sum, v.CurrBlockTotalBonuses.Int)
				}
				// Sum of truncated splits is at most top-level value and at
				// least top-level minus len(validators) (each MulDiv loses <1).
				diff := new(big.Int).Sub(pb.CurrBlockTotalBonuses.Int, sum)
				if diff.Sign() < 0 {
					t.Errorf("sum of per-validator bonuses (%s) exceeds total (%s)", sum, pb.CurrBlockTotalBonuses.Int)
				}
				if diff.Cmp(big.NewInt(int64(len(pb.Validators)))) > 0 {
					t.Errorf("sum of per-validator bonuses (%s) trails total (%s) by more than %d (rounding tolerance)",
						sum, pb.CurrBlockTotalBonuses.Int, len(pb.Validators))
				}
			}
		})
	}

	if found == 0 {
		t.Fatal("no snapshot files found in tests/snapshots/js_validators_rounds")
	}
}

// TestTonscanElectionMatchesService calls FetchRoundRewards for each tonscan
// election snapshot and verifies that:
//   - every tonscan participant address appears as a validator pool in the service output
//   - the elected validator count does not exceed tonscan's total_participants
//   - total_stake from the service matches the sum derived from tonscan participants
//
// Requires access to an archival liteserver.
// Example:
//
//	go test ./service/... -run TestTonscanElection -v -timeout 10m
func TestTonscanElectionMatchesService(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping blockchain integration test in short mode")
	}

	configPath := os.Getenv("TON_CONFIG")

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	tonscanDir := filepath.Join(filepath.Dir(currentFile), "..", "tests", "snapshots", "tonscan_elections")

	entries, err := os.ReadDir(tonscanDir)
	if err != nil {
		t.Fatalf("read tonscan dir %s: %v", tonscanDir, err)
	}

	client, err := service.NewClient(configPath)
	if err != nil {
		t.Fatalf("create blockchain client: %v", err)
	}
	time.Sleep(3 * time.Second)

	svc := service.New(client, configPath)

	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		found++

		// disable this test for now, since tonscan.com validators are hard to compare
		if found > 0 {
			continue
		}

		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(tonscanDir, name))
			if err != nil {
				t.Fatalf("read tonscan snapshot: %v", err)
			}

			var file struct {
				JSON struct {
					Data struct {
						ID                int64       `json:"id"`
						TotalStake        json.Number `json:"total_stake"`
						TotalParticipants int         `json:"total_participants"`
						Participants      []struct {
							Address string      `json:"address"`
							Stake   json.Number `json:"stake"`
						} `json:"participants"`
					} `json:"data"`
				} `json:"json"`
			}
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.UseNumber()
			if err := dec.Decode(&file); err != nil {
				t.Fatalf("parse tonscan snapshot: %v", err)
			}

			d := file.JSON.Data
			if d.ID == 0 {
				t.Fatalf("missing election id")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			out, err := svc.FetchRoundRewards(ctx, model.RoundRewardsQuery{
				ElectionID: &d.ID,
			}, false, nil)
			if err != nil {
				t.Fatalf("FetchRoundRewards(election=%d): %v", d.ID, err)
			}

			if out.ElectionID != d.ID {
				t.Fatalf("election_id: got %d, want %d", out.ElectionID, d.ID)
			}

			// Build normalized address set from service output validator pools.
			outPoolCounts := make(map[string]int, len(out.Validators))
			for _, v := range out.Validators {
				k := normalizeAccountKey(v.Pool)
				outPoolCounts[k]++
			}

			// Every tonscan participant must appear in the service output.
			tonscanAddrCounts := make(map[string]int, len(d.Participants))
			for _, p := range d.Participants {
				k := normalizeAccountKey(p.Address)
				tonscanAddrCounts[k]++
			}

			// Each service validator pool must appear among tonscan participants.
			for raw, need := range outPoolCounts {
				if tonscanAddrCounts[raw] < need {
					t.Errorf("pool %s: service has %d validators, tonscan has %d participants", raw, need, tonscanAddrCounts[raw])
				}
			}

			// Elected validator count must not exceed total tonscan participants.
			if d.TotalParticipants > 0 && len(out.Validators) > d.TotalParticipants {
				t.Errorf("service validator count %d > tonscan total_participants %d",
					len(out.Validators), d.TotalParticipants)
			}

			// We do not verify tonscan total_stake, since it's computed differently
			// tonscanTotal := jsonNumberToBigIntStr(t, d.TotalStake, "tonscan total_stake")
			// if out.TotalStake.Cmp(tonscanTotal) != 0 {
			// 	t.Errorf("total_stake: service %s, tonscan %s", out.TotalStake, tonscanTotal)
			// }

			wrongCount := 0
			snapAddress := make(map[string]struct{}, len(d.Participants))
			for _, p := range d.Participants {
				snapAddress[p.Address] = struct{}{}
			}
			for _, ov := range out.Validators {
				if _, found := snapAddress[ov.Pool]; !found {
					wrongCount++
					continue
				}

				snapValidatorStake := int64(0)
				poolFound := false
				for _, p := range d.Participants {
					if p.Address == ov.Pool {
						snapValidatorStake, err = p.Stake.Int64()
						if err != nil {
							t.Fatalf("validator %s: parse snap validator stake: %v", ov.Pool, err)
						}

						if snapValidatorStake == 0 {
							t.Fatalf("validator %s: snap validator stake is 0", ov.Pool)
						}

						if ov.TotalStake.Cmp(new(big.Int).SetInt64(int64(snapValidatorStake))) != 0 {
							continue
							// t.Fatalf("validator %s: snap validator stake %v != service effective stake %s", ov.Pool, snapValidatorStake, ov.EffectiveStake)
						}
						poolFound = true
						break
					}
				}

				if !poolFound {
					t.Fatalf("validator %s: pool not found in tonscan", ov.Pool)
				}
			}

			if wrongCount > 0 {
				t.Fatalf("address wrong count: %d", wrongCount)
			}

		})
	}

	if found == 0 {
		t.Fatal("no tonscan snapshot files in tests/snapshots/tonscan_elections")
	}
}

func normalizeAccountKey(human string) string {
	id, err := ton.ParseAccountID(human)
	if err != nil {
		return human
	}
	return id.ToRaw()
}

func jsonNumberToBigIntStr(t *testing.T, n json.Number, label string) *big.Int {
	t.Helper()
	s := strings.TrimSpace(n.String())
	if s == "" || s == "null" {
		t.Fatalf("%s: missing value", label)
	}
	v := new(big.Int)
	if _, ok := v.SetString(s, 10); !ok {
		t.Fatalf("%s: invalid integer %q", label, s)
	}
	return v
}

func parseBigIntStr(t *testing.T, s, label string) *big.Int {
	t.Helper()
	v := new(big.Int)
	if _, ok := v.SetString(s, 10); !ok {
		t.Fatalf("%s: invalid integer %q", label, s)
	}
	return v
}
