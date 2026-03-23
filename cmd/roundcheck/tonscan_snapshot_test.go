package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tonkeeper/tongo/ton"
)

// Tonscan election API: https://api.tonscan.com/api/bt/getElection/{id}?limit=1000&offset=0
// Snapshots are committed under tests/snapshots/tonscan_elections/.

type tonscanElectionFile struct {
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

type jsSnapshotForTonscanCompare struct {
	ElectionID int64 `json:"election_id"`
	Validators []struct {
		Pool string `json:"pool"`
	} `json:"validators"`
}

func TestTonscanElectionSnapshots(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}

	tonscanDir := filepath.Join(filepath.Dir(currentFile), "..", "..", "tests", "snapshots", "tonscan_elections")
	jsDir := filepath.Join(filepath.Dir(currentFile), "..", "..", "tests", "snapshots", "js_validators_rounds")

	entries, err := os.ReadDir(tonscanDir)
	if err != nil {
		t.Fatalf("read tonscan directory %s: %v", tonscanDir, err)
	}

	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		found++

		t.Run(name, func(t *testing.T) {
			path := filepath.Join(tonscanDir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read tonscan snapshot %s: %v", path, err)
			}

			var file tonscanElectionFile
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.UseNumber()
			if err := dec.Decode(&file); err != nil {
				t.Fatalf("parse tonscan snapshot %s: %v", path, err)
			}

			d := file.JSON.Data
			if d.ID == 0 {
				t.Fatalf("tonscan %s: missing election id", path)
			}
			if len(d.Participants) == 0 {
				t.Fatalf("tonscan %s: participants is empty", path)
			}

			totalStake := jsonNumberToBigInt(t, d.TotalStake, path, "total_stake")

			sumParticipants, tonscanAddrCounts := tonscanParticipantTotals(t, d.Participants, path)
			if sumParticipants.Cmp(totalStake) != 0 {
				t.Errorf("tonscan %s: sum(participant stakes) %s != total_stake %s", path, sumParticipants, totalStake)
			}

			if d.TotalParticipants > 0 && d.TotalParticipants != len(d.Participants) {
				t.Errorf("tonscan %s: total_participants %d != len(participants) %d", path, d.TotalParticipants, len(d.Participants))
			}

			jsPath := filepath.Join(jsDir, fmt.Sprintf("validators_round_%d.json", d.ID))
			jsData, err := os.ReadFile(jsPath)
			if err != nil {
				if os.IsNotExist(err) {
					t.Logf("tonscan %s: no matching JS snapshot at %s (skipping cross-check)", path, jsPath)
					return
				}
				t.Fatalf("read JS snapshot %s: %v", jsPath, err)
			}

			var jsIn jsSnapshotForTonscanCompare
			if err := json.Unmarshal(jsData, &jsIn); err != nil {
				t.Fatalf("parse JS snapshot %s: %v", jsPath, err)
			}
			if jsIn.ElectionID != d.ID {
				t.Fatalf("JS %s: election_id %d != tonscan id %d", jsPath, jsIn.ElectionID, d.ID)
			}

			// Tonscan lists every elector participant with its own stake model / timing; JS uses
			// per-validator effective stake at API snapshot time. Compare normalized pool addresses:
			// each JS validator's pool must appear at least as often among Tonscan participants
			// (Ef/Uf and bounce flags normalize via ParseAccountID).
			jsPoolCounts := jsValidatorPoolCounts(jsIn.Validators)
			if err := jsPoolCountsContainedInTonscan(tonscanAddrCounts, jsPoolCounts); err != nil {
				t.Errorf("tonscan vs JS (%s): %v", jsPath, err)
			}
		})
	}

	if found == 0 {
		t.Fatal("no tonscan snapshot files in tests/snapshots/tonscan_elections")
	}
}

func jsonNumberToBigInt(t *testing.T, n json.Number, path, field string) *big.Int {
	t.Helper()
	s := strings.TrimSpace(n.String())
	if s == "" || s == "null" {
		t.Fatalf("%s: missing %s", path, field)
	}
	v := new(big.Int)
	if _, ok := v.SetString(s, 10); !ok {
		t.Fatalf("%s: invalid %s %q", path, field, s)
	}
	return v
}

func tonscanParticipantTotals(t *testing.T, participants []struct {
	Address string      `json:"address"`
	Stake   json.Number `json:"stake"`
}, path string) (*big.Int, map[string]int) {
	t.Helper()
	sum := new(big.Int)
	addrCounts := make(map[string]int, len(participants))
	for i, p := range participants {
		stake := jsonNumberToBigInt(t, p.Stake, path, fmt.Sprintf("participants[%d].stake", i))
		sum.Add(sum, stake)
		k := normalizeAccountKey(p.Address)
		addrCounts[k]++
	}
	return sum, addrCounts
}

func jsValidatorPoolCounts(validators []struct {
	Pool string `json:"pool"`
}) map[string]int {
	out := make(map[string]int, len(validators))
	for _, v := range validators {
		k := normalizeAccountKey(v.Pool)
		out[k]++
	}
	return out
}

func normalizeAccountKey(human string) string {
	id, err := ton.ParseAccountID(human)
	if err != nil {
		return human
	}
	return id.ToRaw()
}

func jsPoolCountsContainedInTonscan(tonscanAddrCounts, jsPoolCounts map[string]int) error {
	for raw, need := range jsPoolCounts {
		if tonscanAddrCounts[raw] < need {
			return fmt.Errorf("pool raw=%s: need %d tonscan rows, have %d", raw, need, tonscanAddrCounts[raw])
		}
	}
	return nil
}

