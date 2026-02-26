package model

// JSON output types.

type Output struct {
	ResponseTimeMs  int64            `json:"response_time_ms"`
	Block           BlockInfo        `json:"block"`
	ValidationRound RoundInfo        `json:"validation_round"`
	ElectionID      int64            `json:"election_id"`
	ElectorBalance  uint64           `json:"elector_balance"`
	TotalStake      uint64           `json:"total_stake"`
	RewardPerBlock  uint64           `json:"reward_per_block"`
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
	EffectiveStake       uint64           `json:"effective_stake"`
	Weight               float64          `json:"weight"`
	PerBlockReward       uint64           `json:"per_block_reward"`
	Pool                 string           `json:"pool,omitempty"`
	OwnerAddress         string           `json:"owner_address,omitempty"`
	ValidatorAddress     string           `json:"validator_address,omitempty"`
	PoolType             string           `json:"pool_type,omitempty"`
	ValidatorStake       uint64           `json:"validator_stake,omitempty"`
	NominatorsStake      uint64           `json:"nominators_stake,omitempty"`
	TotalStake           uint64           `json:"total_stake,omitempty"`            // total pool stake (replaces previous balance semantics)
	ValidatorRewardShare float64          `json:"validator_reward_share,omitempty"` // 0.0–1.0
	NominatorsCount      uint32           `json:"nominators_count,omitempty"`
	Nominators           []NominatorEntry `json:"nominators,omitempty"`
}

type NominatorEntry struct {
	Address        string  `json:"address"`
	Weight         float64 `json:"weight"`
	PerBlockReward uint64  `json:"per_block_reward"`
	EffectiveStake uint64  `json:"effective_stake"` // proportional share of validator's true_stake
	Stake          uint64  `json:"stake"`           // pool's internal balance (includes reinvested rewards)
}
