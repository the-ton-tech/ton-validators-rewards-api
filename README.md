# ton-validators-rewards-api

HTTP API that returns TON validator rewards and staking data — stakes, per-block rewards, pool addresses, and nominator breakdowns — fetched directly from TON liteservers via the [tongo](https://github.com/tonkeeper/tongo) library.

## Quick start

```bash
go build -o ton-validators-rewards-api .
./ton-validators-rewards-api
```

The server listens on port `8080` by default. Override with the `PORT` environment variable:

```bash
PORT=3000 ./ton-validators-rewards-api
```

Environment variables are loaded from a `.env` file in the working directory (if present). Supported variables:

| Variable      | Description |
|---------------|-------------|
| `PORT`        | HTTP server port (default: `8080`) |
| `UPTRACE_DSN` | [Uptrace](https://uptrace.dev) DSN for tracing. If not set, no telemetry is sent. |

### Docker

```bash
docker build -t ton-validators-rewards-api .
docker run -p 8080:8080 ton-validators-rewards-api
```

To use a custom liteserver config (e.g. your own archival nodes), mount it and pass `-config`:

```bash
docker run -p 8080:8080 \
  -v $(pwd)/config.json:/config.json:ro \
  ton-validators-rewards-api -config /config.json
```

On first launch the TON global config is downloaded and cached in memory for 7 days (per process). When a custom config is provided via `-config`, it is used directly instead. While the app is running, the lite client is refreshed after TTL so config can be reloaded without restart. The client connects to all available liteservers in parallel.

## API

### `GET /swagger`

Swagger UI for interactive API exploration.

### `GET /api/openapi.yaml`

Raw OpenAPI 3.0 specification.

### `GET /health`

Returns `{"status":"ok"}`.

### `GET /api/validation-rounds`

Returns past and current validation rounds with boundaries, stakes, and bonuses.

Query parameters:

| Parameter     | Type  | Description                                                      |
|---------------|-------|------------------------------------------------------------------|
| `election_id` | int64 | Return the single round matching this election ID                |
| `block`       | uint32| Find the round containing this masterchain block seqno           |
| `unixtime`    | uint32| Unix timestamp (seconds). Looks up the masterchain block at this time and uses it as the anchor. |

`election_id`, `block`, and `unixtime` are mutually exclusive. If none is provided, uses the latest block.

Response:

```json
{
  "response_time_ms": 1234,
  "rounds": [
    {
      "election_id": 1740053384,
      "start": "2026-02-20T12:09:44Z",
      "end": "2026-02-21T06:22:00Z",
      "start_block": 57480000,
      "end_block": 57546000,
      "finished": true,
      "prev_election_id": 1772486024,
      "next_election_id": 1772658824
    }
  ]
}
```

### `GET /api/round-rewards`

Computes per-validator and per-nominator reward distribution for a finished validation round.

Query parameters:

| Parameter     | Type  | Description                                                      |
|---------------|-------|------------------------------------------------------------------|
| `election_id` | int64 | Election ID of the finished round                                |
| `block`       | uint32| Masterchain block seqno within the finished round                |
| `unixtime`    | uint32| Unix timestamp (seconds). Looks up the masterchain block at this time and uses it as the anchor. |
| `shallow`     | flag  | Set `shallow=1` to return only basic validator info (rank, pubkey, effective_stake, weight, reward, pool). Skips pool type detection, owner/validator addresses, nominator data, and returned-stake lookup — significantly faster. |

`election_id`, `block`, and `unixtime` are mutually exclusive. At least one is required.

Response:

```json
{
  "response_time_ms": 5432,
  "election_id": 1740053384,
  "prev_election_id": 1772486024,
  "next_election_id": 1772658824,
  "round_start": "2026-02-20T12:09:44Z",
  "round_end": "2026-02-21T06:22:00Z",
  "start_block": 57480000,
  "end_block": 57546000,
  "total_bonuses": "75327775732769",
  "total_stake": "457752122739238021",
  "validators": [
    {
      "rank": 1,
      "pubkey": "e33f0e53552f951e...",
      "effective_stake": "2127654606060000",
      "weight": 0.004648,
      "reward": "348738674222",
      "pool": "Ef_bmCmMPsrHKOC4hV8foWBs2TEUAggQ1Wfe6EAqjrI3sGNI",
      "pool_type": "nominator-pool-v1.0",
      "total_stake": "2387001628702831",
      "validator_stake": "7045821473255",
      "nominators_stake": "2379606506684221",
      "validator_reward_share": 0.3,
      "nominators_count": 1,
      "nominators": [
        {
          "address": "EQBdcnCvPwcmBf...",
          "weight": 1.0,
          "reward": "243360777736",
          "effective_stake": "2124977795706504",
          "stake": "2379606506684221"
        }
      ]
    }
  ]
}
```

### `GET /api/validators`

Returns all current validators with stakes, rewards, pool addresses, and nominators.

Query parameters:

| Parameter    | Type   | Description                                      |
|--------------|--------|--------------------------------------------------|
| `seqno`      | uint32 | Masterchain block seqno (defaults to latest). Mutually exclusive with `unixtime`. |
| `unixtime`   | uint32 | Unix timestamp (seconds). Looks up the masterchain block at this time. Mutually exclusive with `seqno`. |
| `shallow`    | flag   | Set `shallow=1` to return only basic validator info (rank, pubkey, effective_stake, weight, reward, pool). Skips pool type detection, owner/validator addresses, nominator data, and returned-stake lookup — significantly faster. |

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
  "elector_balance": "966674188286983322",
  "total_stake": "457752122739238021",
  "reward_per_block": "2928989965",
  "validators": [
    {
      "rank": 1,
      "pubkey": "e33f0e53552f951e...",
      "effective_stake": "2127654606060000",
      "weight": 0.004648,
      "reward": "13614090",
      "total_stake": "2376902585342169",
      "pool": "Ef_bmCmMPsrHKOC4hV8foWBs2TEUAggQ1Wfe6EAqjrI3sGNI",
      "pool_type": "nominator-pool-v1.0",
      "owner_address": "EQB7...",
      "validator_address": "Ef9T...",
      "validator_stake": "200000000000000",
      "nominators_stake": "2176902585342169",
      "validator_reward_share": 0.3,
      "nominators_count": 1,
      "nominators": [
        {
          "address": "EQAqR4RYauq7p3jqKGnD-eSYVDoOCak9g8ZsSNVHI9fevCzB",
          "weight": 1.0,
          "reward": "13614090",
          "effective_stake": "2127654606060000",
          "stake": "2176902585342169"
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
| `reward` | Estimated reward this validator earns per masterchain block (nanoTON) |
| `pool` | Pool smart contract address (bounceable, base64url) |
| `validator_address` | Validator's wallet address (the one that controls the node) |
| `owner_address` | The single owner who deposited funds. Only present for single-nominator pools |
| `pool_type` | Contract type: `"nominator-pool-v1.0"`, `"single-nominator-pool-v1.0"`, `"single-nominator-pool-v1.1"`, or `"other"` |
| `validator_stake` | Validator's own funds deposited into the pool (nanoTON). Nominator pool only |
| `nominators_stake` | Sum of all nominator deposits in the pool (nanoTON). Nominator pool only |
| `total_stake` | Total funds deposited by the pool: `effective_stake + credit` (leftover balance kept in the elector contract after election) |
| `validator_reward_share` | Fraction of staking rewards kept by the validator (0.3 = 30%). Nominator pool only |
| `nominators_count` | Number of nominators in the pool. Nominator pool only |
| `nominators` | List of individual nominators. Nominator pool only |

#### Nominator fields (inside `nominators` array)

| Field | Description |
|---|---|
| `address` | Nominator's wallet address (bounceable, base64url) |
| `weight` | Nominator's share of the total nominators' deposit (0–1) |
| `reward` | Estimated per-block reward after the validator's cut (nanoTON) |
| `effective_stake` | Nominator's proportional share of the effective stake locked in the Elector (nanoTON) |
| `stake` | Nominator's raw deposit in the pool contract (nanoTON) |

Key distinction: `stake` / `total_stake` is what was deposited into the pool, while `effective_stake` is what the Elector actually locked. These differ because the Elector may accept less than the full pool balance.

## Project structure

```
main.go                  Entry point — wires service, API, and Swagger routes
openapi.yaml             OpenAPI 3.0 specification (embedded in binary)
model/model.go           JSON response types
model/rpccount.go        Per-request RPC call counter (context-based)
service/service.go       Service struct with DI for liteapi client
service/client.go        TON lite client initialization + config caching
service/blocks.go        FetchPerBlockRewards() — per-block validator stats
service/rounds.go        FetchRoundRewards(), FetchValidationRounds() — round-level data
service/shared.go        Shared helpers: buildValidatorRows, fetchRoundData, etc.
service/pool.go          Pool type detection (by code hash), past_elections parsing
service/blockchain.go    Block lookup, round info, validator extraction
api/handler.go           HTTP handlers, ValidatorService interface
api/swagger.go           Swagger UI HTML template
Dockerfile               Multi-stage build (scratch)
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
5. Detects pool type by matching contract code hash (single `GetAccountState` call per pool)
6. Optionally fetches nominator lists for each pool in parallel
7. Returns the assembled JSON response

All TON amount values are in nanoTON.
