// Command roundcheck validates aggregate values in round JSON files.
//
// Usage:
//
//	go run ./cmd/roundcheck -file round.json
//	go run ./cmd/roundcheck round.json
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
)

type roundRewardsFile struct {
	ElectionID     int64       `json:"election_id"`
	TotalBonuses   string      `json:"total_bonuses"`
	RewardPerBlock string      `json:"reward_per_block"`
	Validators     []validator `json:"validators"`
}

type validator struct {
	Weight         json.Number `json:"weight"`
	Reward         string      `json:"reward"`
	PerBlockReward string      `json:"per_block_reward"`
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("roundcheck", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	filePath := fs.String("file", "", "path to round rewards JSON file")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	path := *filePath
	if path == "" && fs.NArg() > 0 {
		path = fs.Arg(0)
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "usage: roundcheck -file <round.json> OR roundcheck <round.json>")
		return 2
	}

	out, err := calculate(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roundcheck: %v\n", err)
		return 1
	}

	fmt.Printf("file: %s\n", path)
	fmt.Printf("election_id: %d\n", out.electionID)
	fmt.Printf("validators_count: %d\n", out.validatorsCount)
	fmt.Printf("weight_sum: %s\n", out.weightSum.FloatString(30))
	fmt.Printf("weight_diff_from_1: %s\n", out.weightDiff.FloatString(30))
	fmt.Printf("weight_match_exact: %t\n", out.weightDiff.Sign() == 0)
	fmt.Printf("check_mode: %s\n", out.checkMode)
	fmt.Printf("%s_sum: %s\n", out.valueField, out.valueSum.String())
	fmt.Printf("%s: %s\n", out.targetField, out.targetValue.String())
	fmt.Printf("%s_diff: %s\n", out.valueField, out.valueDiff.String())
	fmt.Printf("%s_match_exact: %t\n", out.valueField, out.valueDiff.Sign() == 0)

	return 0
}

type result struct {
	electionID      int64
	validatorsCount int
	weightSum       *big.Rat
	weightDiff      *big.Rat
	checkMode       string
	valueField      string
	targetField     string
	valueSum        *big.Int
	targetValue     *big.Int
	valueDiff       *big.Int
}

func calculate(path string) (*result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.UseNumber()

	var in roundRewardsFile
	if err := dec.Decode(&in); err != nil {
		return nil, err
	}

	if len(in.Validators) == 0 {
		return nil, errors.New("validators is empty")
	}

	weightSum := new(big.Rat)

	for i, v := range in.Validators {
		w := new(big.Rat)
		if _, ok := w.SetString(v.Weight.String()); !ok {
			return nil, fmt.Errorf("validator[%d]: invalid weight %q", i, v.Weight.String())
		}
		weightSum.Add(weightSum, w)
	}

	weightDiff := new(big.Rat).Sub(weightSum, big.NewRat(1, 1))

	var (
		checkMode   string
		valueField  string
		targetField string
		valueSum    = new(big.Int)
		targetValue = new(big.Int)
	)

	switch {
	case in.TotalBonuses != "":
		checkMode = "round_rewards"
		valueField = "reward"
		targetField = "total_bonuses"
		if _, ok := targetValue.SetString(in.TotalBonuses, 10); !ok {
			return nil, fmt.Errorf("invalid total_bonuses: %q", in.TotalBonuses)
		}
		for i, v := range in.Validators {
			r := new(big.Int)
			if _, ok := r.SetString(v.Reward, 10); !ok {
				return nil, fmt.Errorf("validator[%d]: invalid reward %q", i, v.Reward)
			}
			valueSum.Add(valueSum, r)
		}
	case in.RewardPerBlock != "":
		checkMode = "per_block_rewards"
		valueField = "per_block_reward"
		targetField = "reward_per_block"
		if _, ok := targetValue.SetString(in.RewardPerBlock, 10); !ok {
			return nil, fmt.Errorf("invalid reward_per_block: %q", in.RewardPerBlock)
		}
		for i, v := range in.Validators {
			r := new(big.Int)
			if _, ok := r.SetString(v.PerBlockReward, 10); !ok {
				return nil, fmt.Errorf("validator[%d]: invalid per_block_reward %q", i, v.PerBlockReward)
			}
			valueSum.Add(valueSum, r)
		}
	default:
		return nil, errors.New("no supported reward fields found: expected total_bonuses or reward_per_block")
	}

	valueDiff := new(big.Int).Sub(valueSum, targetValue)

	return &result{
		electionID:      in.ElectionID,
		validatorsCount: len(in.Validators),
		weightSum:       weightSum,
		weightDiff:      weightDiff,
		checkMode:       checkMode,
		valueField:      valueField,
		targetField:     targetField,
		valueSum:        valueSum,
		targetValue:     targetValue,
		valueDiff:       valueDiff,
	}, nil
}
