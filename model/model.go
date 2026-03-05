package model

import (
	"fmt"
	"math/big"
	"strings"
	"time"
)

// BigInt wraps big.Int with JSON string marshalling to preserve precision.
type BigInt struct{ big.Int }

// NewBigInt creates a *BigInt from a uint64 value.
func NewBigInt(v uint64) *BigInt {
	b := new(BigInt)
	b.SetUint64(v)
	return b
}

func (b BigInt) MarshalJSON() ([]byte, error) {
	return []byte(`"` + b.String() + `"`), nil
}

func (b *BigInt) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	_, ok := b.SetString(s, 10)
	if !ok {
		return fmt.Errorf("invalid BigInt: %s", s)
	}
	return nil
}

// JSON output types.

type Output struct {
	ResponseTimeMs  int64            `json:"response_time_ms"`
	Block           BlockInfo        `json:"block"`
	ValidationRound RoundInfo        `json:"validation_round"`
	ElectionID      int64            `json:"election_id"`
	ElectorBalance  *BigInt          `json:"elector_balance"`
	TotalStake      *BigInt          `json:"total_stake"`
	RewardPerBlock  *BigInt          `json:"reward_per_block"`
	Validators      []ValidatorEntry `json:"validators"`
}

type BlockInfo struct {
	Seqno uint32 `json:"seqno"`
	Time  string `json:"time"`
}

type RoundInfo struct {
	Start      string `json:"start"`
	End        string `json:"end"`
	StartBlock uint32 `json:"start_block"`
	EndBlock   uint32 `json:"end_block,omitempty"`
}

type ValidatorEntry struct {
	Rank                 int              `json:"rank"`
	Pubkey               string           `json:"pubkey"`
	EffectiveStake       *BigInt          `json:"effective_stake"`
	Weight               float64          `json:"weight"`
	PerBlockReward       *BigInt          `json:"per_block_reward"`
	Pool                 string           `json:"pool,omitempty"`
	OwnerAddress         string           `json:"owner_address,omitempty"`
	ValidatorAddress     string           `json:"validator_address,omitempty"`
	PoolType             string           `json:"pool_type,omitempty"`
	ValidatorStake       *BigInt          `json:"validator_stake,omitempty"`
	NominatorsStake      *BigInt          `json:"nominators_stake,omitempty"`
	TotalStake           *BigInt          `json:"total_stake,omitempty"`
	ValidatorRewardShare float64          `json:"validator_reward_share,omitempty"`
	NominatorsCount      uint32           `json:"nominators_count,omitempty"`
	Nominators           []NominatorEntry `json:"nominators,omitempty"`
}

type ValidationRound struct {
	ElectionID     int64     `json:"election_id"`
	Start          time.Time `json:"start"`
	End            time.Time `json:"end"`
	StartBlock     uint32    `json:"start_block"`
	EndBlock       uint32    `json:"end_block,omitempty"`
	Finished       bool      `json:"finished"`
	PrevElectionID *int64    `json:"prev_election_id,omitempty"`
	NextElectionID *int64    `json:"next_election_id,omitempty"`
	Error          string    `json:"error,omitempty"`
}

type ValidationRoundsOutput struct {
	ResponseTimeMs int64             `json:"response_time_ms"`
	Rounds         []ValidationRound `json:"rounds"`
	Error          string            `json:"error,omitempty"`
}

// RoundsQuery holds query parameters for the validation-rounds endpoint.
type RoundsQuery struct {
	ElectionID *int64
	Block      *uint32
}

// RoundRewardsQuery holds query parameters for the round-rewards endpoint.
type RoundRewardsQuery struct {
	Block      *uint32
	ElectionID *int64
}

// RoundRewardsOutput is the response for the round-rewards endpoint.
type RoundRewardsOutput struct {
	ResponseTimeMs int64             `json:"response_time_ms"`
	ElectionID     int64             `json:"election_id"`
	RoundStart     string            `json:"round_start"`
	RoundEnd       string            `json:"round_end"`
	StartBlock     uint32            `json:"start_block"`
	EndBlock       uint32            `json:"end_block"`
	TotalBonuses   *BigInt           `json:"total_bonuses"`
	TotalStake     *BigInt           `json:"total_stake"`
	Validators     []ValidatorReward `json:"validators"`
	Error          string            `json:"error,omitempty"`
}

// ValidatorReward holds per-validator reward data for a finished round.
type ValidatorReward struct {
	Rank                 int               `json:"rank"`
	Pubkey               string            `json:"pubkey"`
	EffectiveStake       *BigInt           `json:"effective_stake"`
	Weight               float64           `json:"weight"`
	Reward               *BigInt           `json:"reward"`
	Pool                 string            `json:"pool,omitempty"`
	OwnerAddress         string            `json:"owner_address,omitempty"`
	ValidatorAddress     string            `json:"validator_address,omitempty"`
	PoolType             string            `json:"pool_type,omitempty"`
	ValidatorStake       *BigInt           `json:"validator_stake,omitempty"`
	NominatorsStake      *BigInt           `json:"nominators_stake,omitempty"`
	TotalStake           *BigInt           `json:"total_stake,omitempty"`
	ValidatorRewardShare float64           `json:"validator_reward_share,omitempty"`
	NominatorsCount      uint32            `json:"nominators_count,omitempty"`
	Nominators           []NominatorReward `json:"nominators,omitempty"`
}

// NominatorReward holds per-nominator reward data for a finished round.
type NominatorReward struct {
	Address        string  `json:"address"`
	Weight         float64 `json:"weight"`
	Reward         *BigInt `json:"reward"`
	EffectiveStake *BigInt `json:"effective_stake"`
	Stake          *BigInt `json:"stake"`
}

type NominatorEntry struct {
	Address        string  `json:"address"`
	Weight         float64 `json:"weight"`
	PerBlockReward *BigInt `json:"per_block_reward"`
	EffectiveStake *BigInt `json:"effective_stake"`
	Stake          *BigInt `json:"stake"`
}
