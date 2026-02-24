package model

// JSON output types.

type Output struct {
	ResponseTimeMs  int64            `json:"response_time_ms"`
	Block           BlockInfo        `json:"block"`
	ValidationRound RoundInfo        `json:"validation_round"`
	ElectionParams  ElectionParams   `json:"election_params"`
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

type ElectionParams struct {
	ValidatorsElectedForSec uint32 `json:"validators_elected_for_sec"`
	ElectionsStartBeforeSec uint32 `json:"elections_start_before_sec"`
	ElectionsEndBeforeSec   uint32 `json:"elections_end_before_sec"`
	StakeHeldForSec         uint32 `json:"stake_held_for_sec"`
}

type ValidatorEntry struct {
	ResponseTimeMs int64            `json:"response_time_ms,omitempty"`
	Rank           int              `json:"rank"`
	Pubkey         string           `json:"pubkey"`
	Stake          uint64           `json:"stake"`
	Share          float64          `json:"share"`
	PerBlockReward uint64           `json:"per_block_reward"`
	Pool           string           `json:"pool,omitempty"`
	PoolType       string           `json:"pool_type,omitempty"`
	Nominators     []NominatorEntry `json:"nominators,omitempty"`
}

type NominatorEntry struct {
	Address        string  `json:"address"`
	Share          float64 `json:"share"`
	PerBlockReward uint64  `json:"per_block_reward"`
	Staked         uint64  `json:"staked"`       // proportional share of validator's true_stake
	PoolBalance    uint64  `json:"pool_balance"` // pool's internal balance (includes reinvested rewards)
}
