package model

import (
	"fmt"
	"math/big"
	"strings"
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
	Start string `json:"start"`
	End   string `json:"end"`
}

type ValidatorEntry struct {
	ResponseTimeMs       int64            `json:"response_time_ms,omitempty"`
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

type NominatorEntry struct {
	Address        string  `json:"address"`
	Weight         float64 `json:"weight"`
	PerBlockReward *BigInt `json:"per_block_reward"`
	EffectiveStake *BigInt `json:"effective_stake"`
	Stake          *BigInt `json:"stake"`
}
