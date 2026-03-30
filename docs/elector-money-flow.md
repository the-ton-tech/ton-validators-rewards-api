# Elector Contract: Money Flow

This document describes the complete lifecycle of funds (Grams/nanoTON) inside the TON Elector contract (`-1:3333...3333`).

---

## Overview

```
  Validators                    Elector Contract                     Network
  ──────────                    ────────────────                     ───────
      │                               │                                │
      │──── new_stake() ─────────────>│                                │
      │     (deposit TON)             │                                │
      │                               │<──── transaction fees ────────│
      │                               │      (every block)             │
      │<──── recover_stake() ────────│                                │
      │      (withdraw TON + reward)  │                                │
```

All amounts are in **nanoTON** (1 TON = 10^9 nanoTON), stored as the TL-B type `Grams` = `VarUInteger 16`.

---

## Stages

### Stage 1 — Elections Open

The elector announces a new election. Validators submit their stakes.

```
Validator Pool ──── new_stake(query_id, validator_pubkey, stake_at, max_factor, ...) ────> Elector
                    └─ attached value: desired stake in nanoTON
```

**What happens to the money:**

| Event | Grams movement |
|-------|---------------|
| Validator sends `new_stake()` | TON lands on elector's balance |
| Elector records the bid | Stored in `elections.members` dict |

At this point the elector simply accumulates deposits. Nothing is locked yet.

---

### Stage 2 — Elections Conducted

The elector picks the winning validator set based on stakes and `max_factor`. This is triggered when the config contract requests a new validator set.

**What happens to the money:**

```
For each elected validator:
┌──────────────────────────────────────────────────────────┐
│                                                          │
│  deposited_stake ──┬──> true_stake   (locked in freeze)  │
│                    │                                     │
│                    └──> credit       (excess returned*)  │
│                                                          │
│  * "returned" = recorded in credits dict,                │
│    not yet sent back to the validator                    │
└──────────────────────────────────────────────────────────┘

For each NOT elected validator:
┌──────────────────────────────────────────────────────────┐
│                                                          │
│  deposited_stake ──────> credit       (full refund*)     │
│                                                          │
│  * also stays in elector until recover_stake() is called │
└──────────────────────────────────────────────────────────┘
```

**Key calculations:**

- `true_stake` — the effective stake the elector actually locks, capped by `max_factor` and the election algorithm
- `credit` = `deposited_stake` - `true_stake` (the excess that didn't fit)
- `total_stake` = sum of all `true_stake` values across elected validators

**Data written to elector storage:**

| Field | Description |
|-------|-------------|
| `frozen_dict` | Hashmap: `validator_pubkey` -> `{ src_addr, weight, true_stake }` |
| `total_stake` | Sum of all `true_stake` values |
| `credits` | Dict of per-address leftover balances |

---

### Stage 3 — Validation Round Starts

The new validator set is installed. The elector creates a `past_elections` entry.

**Bonus allocation — the `grams >> 3` moment:**

```
elector_free_balance = elector_total_balance
                     - all_frozen_stakes (across all active past_elections)
                     - all_credits

initial_bonuses = elector_free_balance / 8      ← (grams >> 3)
```

The free balance consists of transaction fees accumulated from previous rounds that haven't been distributed yet. Only **1/8** is allocated per round — the rest carries over, creating a smoothing effect:

```
Round N:    distributes 1/8 of pool     →  keeps 7/8
Round N+1:  distributes 1/8 of (7/8)   →  keeps 7/8 of (7/8)
Round N+2:  distributes 1/8 of (7/8)^2 →  ...

This is exponential decay — fees spread across many rounds.
```

**`past_elections` entry created:**

```
past_elections[election_id] = (
    election_id,      ← utime_since of this validator set
    unfreeze_at,      ← when stakes can be withdrawn
    stake_held,       ← duration stakes remain frozen after round ends
    vset_hash,        ← hash of the validator set
    frozen_dict,      ← { pubkey -> { src_addr, weight, true_stake } }
    total_stake,      ← sum of all true_stake
    bonuses,          ← starts at (grams >> 3), grows each block
    complaints        ← validator misbehavior reports
)
```

---

### Stage 4 — During the Round (Block by Block)

Every block, transaction fees from the network are added to the elector's balance. The `bonuses` field in `past_elections` grows:

```
Block N:    bonuses = 1_000_000_000
Block N+1:  bonuses = 1_000_042_000   (+42_000 from tx fees)
Block N+2:  bonuses = 1_000_099_500   (+57_500 from tx fees)
...
```

**This is how per-block rewards are computed in the API:**

```go
rewardPerBlock = bonuses_at_block_N - bonuses_at_block_(N-1)
```

No Grams move during this stage — the bonuses field is just a counter. The actual TON sits on the elector's balance.

---

### Stage 5 — Round Ends

The next validator set is installed. The current round's `past_elections` entry now has its **final bonuses** value.

```
                     Round N                          Round N+1
─────────────┬───────────────────────┬─────────────────────────────
             │                       │
        round starts            round ends
        bonuses = X             bonuses = X + accumulated_fees
        (grams >> 3)            (final value)
                                     │
                                     ├─ new past_elections entry for Round N+1
                                     │  with its own (grams >> 3)
                                     │
                                     └─ Round N entry: frozen, waiting for unfreeze
```

**At this moment, Round N's entry contains:**

| Field | Status |
|-------|--------|
| `frozen_dict` | Immutable — set at Stage 2 |
| `total_stake` | Immutable — set at Stage 2 |
| `bonuses` | Final — no longer grows |
| `unfreeze_at` | Set to `round_end + stake_held` |

---

### Stage 6 — Unfreeze Period

Between round end and `unfreeze_at`, stakes remain locked. This is a safety window for submitting complaints about validator misbehavior.

```
Round ends                                    unfreeze_at
    │◄──────────── stake_held period ──────────────►│
    │                                               │
    │  Stakes locked. Complaints can be filed.      │  Stakes unlocked.
    │  No withdrawals possible.                     │  recover_stake() enabled.
```

If a complaint is accepted, the misbehaving validator's stake can be slashed (partially or fully).

---

### Stage 7 — Withdrawal (`recover_stake`)

After `unfreeze_at`, each validator calls `recover_stake()` to get their funds back.

**Payout calculation:**

```
validator_reward = floor(total_bonuses * true_stake / total_stake)

payout = true_stake + validator_reward
```

```
┌─────────────────────────────────────────────────────┐
│                                                     │
│  Elector Balance                                    │
│                                                     │
│  ┌─────────────┐  ┌─────────────┐                  │
│  │ true_stake   │  │ reward      │ ──────────────>  Validator Pool
│  │ (returned)   │  │ (from bonus)│    payout msg    │
│  └─────────────┘  └─────────────┘                  │
│                                                     │
│  ┌─────────────┐                                    │
│  │ credit      │ ──────────────────────────────────> Validator Pool
│  │ (excess)    │    also via recover_stake()         │
│  └─────────────┘                                    │
│                                                     │
│  ┌─────────────┐                                    │
│  │ remainder   │  (rounding dust, stays in elector) │
│  │ ~N nanoTON  │                                    │
│  └─────────────┘                                    │
│                                                     │
└─────────────────────────────────────────────────────┘
```

**What each validator receives:**

| Component | Formula | Description |
|-----------|---------|-------------|
| `true_stake` | — | Original locked stake, returned in full |
| `reward` | `floor(bonuses * true_stake / total_stake)` | Proportional share of accumulated bonuses |
| `credit` | `deposited - true_stake` | Excess from election (if any) |
| **Total payout** | `true_stake + reward + credit` | Everything sent back in one message |

**Rounding remainder:**

```
remainder = total_bonuses - sum(floor(bonuses * stake_i / total_stake) for each validator)
```

This is at most `N - 1` nanoTON (where N = number of validators). It stays on the elector's balance and rolls into the free balance for future `grams >> 3` allocations.

---

## Complete Money Flow Diagram

```
                    ┌──────────────────────────────────────────┐
                    │           ELECTOR CONTRACT               │
                    │                                          │
 new_stake() ──────>│  ┌────────────┐     ┌──────────────┐    │
 (deposit)          │  │  Elections  │────>│  Frozen Dict  │    │
                    │  │  members    │     │  true_stake   │    │
                    │  └────────────┘     └──────┬───────┘    │
                    │        │                    │            │
                    │        │ excess             │            │
                    │        v                    │            │
                    │  ┌────────────┐             │            │
                    │  │  Credits   │             │            │
                    │  │  (excess)  │             │            │
                    │  └─────┬──────┘             │            │
                    │        │                    │            │
                    │        │                    v            │
 tx fees ──────────>│        │           ┌──────────────┐     │
 (every block)      │        │           │   Bonuses     │     │
                    │        │           │ (1/8 of pool  │     │
                    │        │           │  + block fees) │     │
                    │        │           └──────┬───────┘     │
                    │        │                  │             │
                    │        │    ┌─────────────┘             │
                    │        │    │                            │
                    │        v    v                            │
 recover_stake() <──│  ┌──────────────┐                       │
 (withdrawal)       │  │   Payout:    │                       │
                    │  │  true_stake  │                       │
                    │  │  + reward    │                       │
                    │  │  + credit    │                       │
                    │  └──────────────┘                       │
                    │                                          │
                    │  ┌──────────────┐                       │
                    │  │  Remainder   │  (stays for next      │
                    │  │  7/8 of pool │   rounds' grams >> 3) │
                    │  └──────────────┘                       │
                    └──────────────────────────────────────────┘
```

---

## Summary Table

| Stage | Trigger | Grams In | Grams Out | Key Calculation |
|-------|---------|----------|-----------|-----------------|
| 1. Elections open | Config announces | `new_stake()` deposits | — | — |
| 2. Elections conducted | Config requests vset | — | — | `true_stake` = locked portion; `credit` = excess |
| 3. Round starts | New vset installed | — | — | `bonuses = free_balance >> 3` |
| 4. During round | Each block | tx fees accumulate | — | `bonuses += block_fees` |
| 5. Round ends | Next vset installed | — | — | `bonuses` finalized |
| 6. Unfreeze | Time passes | — | — | Complaints processed, possible slashing |
| 7. Withdrawal | `recover_stake()` | — | `stake + reward + credit` | `reward = bonuses * stake / total` |
