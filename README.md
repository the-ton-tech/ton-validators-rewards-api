# validators-statistics

HTTP API that returns current TON validator statistics — stakes, per-block rewards, pool addresses, and nominator breakdowns — fetched directly from TON liteservers via the [tongo](https://github.com/tonkeeper/tongo) library.

## Quick start

```bash
go build -o validators-statistics .
./validators-statistics
```

The server listens on port `8080` by default. Override with the `PORT` environment variable:

```bash
PORT=3000 ./validators-statistics
```

### Docker

```bash
docker build -t validators-statistics .
docker run -p 8080:8080 validators-statistics
```

On first launch the TON global config is downloaded and cached in memory for 7 days (per process). While the app is running, the lite client is refreshed after TTL so config can be reloaded without restart. The client connects to all available liteservers in parallel.

## API

### `GET /health`

Returns `{"status":"ok"}`.

### `GET /api/validators`

Returns all current validators with stakes, rewards, pool addresses, and nominators.

Query parameters:

| Parameter    | Type   | Description                                      |
|--------------|--------|--------------------------------------------------|
| `seqno`      | uint32 | Masterchain block seqno (defaults to latest)     |
| `nominators` | string | Set to `false` to skip nominator data            |

Response:

```json
{
  "response_time_ms": 12345,
  "block": {
    "seqno": 57486221,
    "time": "2026-02-20T13:51:37Z"
  },
  "validation_round": {
    "start": "2026-02-20T12:09:44Z",
    "end": "2026-02-21T06:22:00Z"
  },
  "elector_balance": 966674188286983322,
  "total_stake": 457752122739238021,
  "reward_per_block": 2928989965,
  "validators": [
    {
      "rank": 1,
      "pubkey": "e33f0e53552f951e...",
      "effective_stake": 2127654606060000,
      "weight": 0.004648,
      "per_block_reward": 13614090,
      "total_stake": 2376902585342169,
      "pool": "Ef_bmCmMPsrHKOC4hV8foWBs2TEUAggQ1Wfe6EAqjrI3sGNI",
      "owner_address": "EQB7...",
      "validator_address": "Ef9T...",
      "nominators": [
        {
          "address": "Ef9dcnCvPwcmBf-JbyIyY47LYCJ3obFCpRG-XhXMV1er1myc",
          "weight": 1.0,
          "per_block_reward": 13614090,
          "effective_stake": 2127654606060000,
          "stake": 2376902585342169
        }
      ]
    }
  ]
}
```

#### Top-level fields

| Field | Description |
|---|---|
| `response_time_ms` | Server-side response time in milliseconds |
| `block.seqno` | Masterchain block sequence number |
| `block.time` | Block timestamp (UTC, RFC 3339) |
| `validation_round.start` | Current validation round start time |
| `validation_round.end` | Current validation round end time |
| `elector_balance` | Elector contract balance (nanoTON) |
| `total_stake` | Sum of all active validators' effective stakes (nanoTON) |
| `reward_per_block` | Total fees collected in the target block (nanoTON) |

#### Validator fields

| Field | Description |
|---|---|
| `rank` | Position in the validator list, sorted by effective stake (descending) |
| `pubkey` | Validator's public key (hex-encoded Ed25519) |
| `effective_stake` | Validator's true stake locked in the Elector contract (nanoTON) |
| `weight` | Fraction of the total effective stake held by this validator (0–1) |
| `per_block_reward` | Estimated reward this validator earns per masterchain block (nanoTON) |
| `pool` | Pool smart contract address (bounceable, base64url) |
| `validator_address` | Validator's wallet address (the one that controls the node) |
| `owner_address` | The single owner who deposited funds. Only present for Single Nominator pools |
| `pool_type` | Contract type: `"Nominator Pool"`, `"Single Nominator"`, etc. |
| `validator_stake` | Validator's own funds deposited into the pool (nanoTON). Nominator Pool only |
| `nominators_stake` | Sum of all nominator deposits in the pool (nanoTON). Nominator Pool only |
| `total_stake` | Total funds in the pool. For Nominator Pool: `validator_stake + nominators_stake`. For others: approximated from contract balance + effective stake |
| `validator_reward_share` | Fraction of staking rewards kept by the validator (0.3 = 30%). Nominator Pool only |
| `nominators_count` | Number of nominators in the pool. Nominator Pool only |
| `nominators` | List of individual nominators. Nominator Pool only |

#### Nominator fields (inside `nominators` array)

| Field | Description |
|---|---|
| `address` | Nominator's wallet address (non-bounceable, base64url) |
| `weight` | Nominator's share of the total nominators' deposit (0–1) |
| `per_block_reward` | Estimated per-block reward after the validator's cut (nanoTON) |
| `effective_stake` | Nominator's proportional share of the effective stake locked in the Elector (nanoTON) |
| `stake` | Nominator's raw deposit in the pool contract (nanoTON) |

Key distinction: `stake` / `total_stake` is what was deposited into the pool, while `effective_stake` is what the Elector actually locked. These differ because the Elector may accept less than the full pool balance.

### `GET /api/validators/{pubkey}`

Returns a single validator entry by hex public key.

Query parameters: same as above (`seqno`, `nominators`).

## Project structure

```
main.go                Entry point — wires service and API layers
model/model.go         JSON response types
model/rpccount.go      Per-request RPC call counter (context-based)
service/service.go     Service struct with DI for liteapi client
service/client.go      TON lite client initialization + config caching
service/stats.go       FetchStats() — core data-fetching orchestrator
service/pool.go        Pool detection, past_elections parsing
service/blockchain.go  Block lookup, round info, validator extraction
api/handler.go         HTTP handlers, ValidatorService interface
Dockerfile             Multi-stage build (scratch)
```

Dependency graph (no cycles):

```
main → api     → model
     → service → model
```

`api` depends on `service` only through the `ValidatorService` interface (DI).

## How it works

1. Connects to TON liteservers using the global config
2. Resolves the target masterchain block (latest or by seqno)
3. Fetches in parallel: config param 34 (validators), past elections (pool addresses + stakes), elector balance, and block reward
4. Past elections data is cached by election IDs — reparsed only when elections change
5. Optionally fetches nominator lists for each pool in parallel (up to 100 concurrent RPC calls)
6. Returns the assembled JSON response

All TON amount values are in nanoTON.
