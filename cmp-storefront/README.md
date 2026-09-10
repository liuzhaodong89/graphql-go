# cmpbench — Shopify vs SHOPLINE Storefront API latency comparison

Compares pre-checkout Storefront GraphQL latency under enforced parity:
identical scenarios, identical success criteria, a payload-size budget, and a
query-depth budget.

## Build

```
go build ./cmd/cmpbench
```

No third-party dependencies.

## Use

```
./setup-tokens.sh                              # 1. credentials (hidden input) + config.json + smoke test
. ./tokens.sh                                  # 2. load them into this shell

./cmpbench fixtures  -config config.json       # 3. fill handle pools from the live stores
./cmpbench probe     -config config.json       # 4. does the live schema match the adapter?
./cmpbench preflight -config config.json       # 5. gates + per-field size attribution, minimal load
./cmpbench bench     -config config.json -out report
```

`fixtures` keeps only handles that exist on **both** stores. A handle present on
one side fails every pair it appears in, and those failures cluster wherever the
catalogues diverge rather than falling randomly.

`probe` introspects the live schema and reports absent or deprecated fields.
`preflight` and `bench` then run **capability negotiation** before any
measurement (see below), so an undocumented or unscoped field becomes an
explicit, logged removal rather than a silent one-sided failure.

With only one platform enabled the tool runs in **baseline mode**: it collects
timings and payload sizes for that platform and issues no comparison verdict.
This is how the Shopify side is validated before the SHOPLINE adapter is
complete.

## Credentials

Tokens live in environment variables only. The config file names the variable
(`token_env`), never the value, and `config.json` plus `*.env` / `tokens.sh` are
gitignored.

```
cp tokens.example.sh tokens.sh   # fill in with an editor, not with echo
. ./tokens.sh
```

Or without writing them to disk at all:

```
read -rs SHOPIFY_STOREFRONT_TOKEN && export SHOPIFY_STOREFRONT_TOKEN
```

Avoid `export TOK=value` typed directly: it lands in shell history and terminal
scrollback.

Nothing the tool writes carries credentials. Server error messages that reflect
request headers back are redacted before they reach a sample, and a test asserts
the token and the login password never appear in the report JSON, the markdown,
the endpoint URLs, or a raw sample dump.

**Scopes needed**

| Platform | Scope | Needed for |
|---|---|---|
| Shopify | `unauthenticated_read_product_listings` | S2-S7 |
| Shopify | `unauthenticated_read_product_inventory` | inventory field only; negotiated away without it |
| SHOPLINE | unauthenticated read scopes | S2-S7 |
| SHOPLINE | `unauthenticated_write_customer_information` | S1 only |

Use a Shopify development store and a SHOPLINE test store.

## Capability negotiation

Two published schemas are not enough to write the query pair by hand:

- SHOPLINE's `Image` type is not reachable in the public docs, so its field
  list is unknown until the server answers.
- Shopify's `quantityAvailable` returns **null**, not an error, when the token
  lacks `unauthenticated_read_product_inventory` — a silent size gap.
- `customerAccessTokenCreate` is live only on stores still using legacy
  customer accounts.

Guessing any of these produces either a one-sided failure (which discards every
pair) or a one-sided null (which quietly opens a size gap). So before
measuring, the tool probes each scenario on both platforms and:

1. drops a field a platform rejects — **from both platforms**, so the two sides
   keep asking for the same information;
2. drops a field that resolves to null on one platform only;
3. **blocks** the scenario if the lost field was mandatory, rather than
   silently shrinking it into a different comparison.

Every removal is printed with the server error that caused it. Fields marked
`Optional` on a scenario may be negotiated away; everything else is load-bearing.

## Rate limiting

The two platforms meter differently, so they get different pacers:

- **Shopify** limits Storefront calls per IP by request rate → `qps` mode.
- **SHOPLINE** limits by **compute time** (about one second of server time per
  app per IP per second) → `compute_budget` mode, which tracks observed server
  time in a sliding window. A fixed QPS that is safe for cheap scenarios would
  throttle on expensive ones.

Both back off multiplicatively on a throttle, because a throttled sample is a
discarded pair and discards that cluster on slow requests bias the comparison.

## What the gates enforce

| Gate | Rule | Failure verdict |
|---|---|---|
| Depth | semantic depth ≤ 2 on the query actually sent | run refuses to start |
| Status | both sides return usable data for the same input | pair discarded |
| Size | median canonical `data` bytes within 10% | `INCOMPARABLE_SIZE` |
| Error rate | ≤ 1% per platform per scenario | `UNRELIABLE_ERROR_RATE` |

Semantic depth counts business-object nesting. Relay connection wrappers
(`edges`/`node`/`nodes`) are excluded: they add an identical level on both
platforms, and counting them would make depth ≤ 2 unsatisfiable for any list.

## Why the measurement is built this way

**ServerTime, not total time.** `TTFB − (DNS + TCP + TLS)` is the primary
metric. The two platforms sit behind different edge networks; total time mostly
measures how far the client is from each POP.

**Paired, interleaved, order-randomised.** Each logical request runs on both
platforms within ~30ms, in randomised order. Statistics run on the *within-pair
difference*, so time-varying network conditions cancel instead of accumulating
into whichever platform was measured during the worse hour.

**A GraphQL 200 is not a success.** The status gate additionally requires no
`errors` array, non-null `data`, every declared required path present and
non-empty, and an echo check that the returned entity is the one requested.
`{"data":{"product":null}}` is the not-found path — fast, and meaningless.

**Discards are reported, not hidden.** If one platform fails precisely on its
slow requests, dropping those samples would flatter it. The report gives the
discard-reason breakdown per platform plus a worst-case p95 that charges every
discard at the timeout ceiling. If the two p95s disagree, the headline number
is not trustworthy.

**Size is measured on canonicalised `data`, uncompressed.** Key order and
whitespace are normalised away, and `Accept-Encoding: identity` is forced:
compression ratio tracks field-name length, so comparing compressed sizes
compares naming conventions rather than payload volume. Wire bytes are reported
alongside, because they are what drives transfer time.

**Size failures name the field.** When a scenario blows the 10% budget, the
report ranks leaf paths by byte gap, so the fix is "the CDN URL field is 78% of
the gap", not "the delta is 18%".

## Known schema asymmetries (verified against both references)

| Semantic field | Shopify | SHOPLINE | Handling |
|---|---|---|---|
| product description | `description` + `descriptionHtml` | `descriptionHtml` only | both use `descriptionHtml` |
| variant list | `variants` (connection) | `variants` deprecated array; `variantsV2` is the connection | adapter maps + rewrites paths |
| image list | `images` (connection) | `images` deprecated array; `imagesV2` is the connection | adapter maps + rewrites paths |
| inventory count | `quantityAvailable` (token access required) | `inventoryQuantity` | renamed by the adapter; negotiated away if either side cannot serve it |
| timestamps | `DateTime` | `Date` | serialised width differs; preflight measures it |
| connection extras | — | `totalCount`, `filters` | deliberately not selected |
| `CustomerAccessToken` | `{accessToken, expiresAt: DateTime}` | `{accessToken, expiresAt: Date}` | S1 reported separately, never in the aggregate |
| variant price | `MoneyV2 {amount, currencyCode}` (an object) | `Money` (a scalar) | excluded: it breaks both depth and size symmetry |
| products `query` grammar | `title:'x' AND updated_at:>'...'` | same grammar | S7 uses a structured filter, not free-text search |
| rate limiting | per-IP request rate | per-IP **compute time** | separate pacing models |

## Queries

Both platforms render their query from one canonical selection tree, so the two
requests cannot drift apart the way two hand-maintained query strings would.
The rendered pair is byte-identical apart from the three confirmed renames,
which is asserted by a test. Semantic depth of the shipped set: S2 1, S3 2,
S4 2, S5 1, S6 2, S7 1, S1 2.

## Fixture requirement

The 10% size budget is met by seeding both stores from one fixture set:
identical titles, identical `descriptionHtml` source strings, identical variant
and image counts, identical SKU widths. No amount of query tuning substitutes
for this — `first:N` must stay equal on both sides, so it cannot be used to
close a gap.

Use a Shopify development store and a SHOPLINE test store. Do not point load at
production storefronts.
