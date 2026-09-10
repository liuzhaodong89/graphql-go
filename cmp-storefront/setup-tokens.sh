#!/bin/sh
# Interactive credential setup for cmpbench.
#
# Writes tokens.sh (mode 600, gitignored) and config.json, then smoke-tests
# both endpoints. Token values are read with echo disabled, so they never
# appear on screen, in shell history, or in terminal scrollback.
#
#   ./setup-tokens.sh
#
set -eu
cd "$(dirname "$0")"
umask 077

TOKENS=tokens.sh
CONFIG=config.json

ask() {           # ask VAR "prompt" "default"  -- visible
  _v=$1; _p=$2; _d=${3:-}
  if [ -n "$_d" ]; then printf '%s [%s]: ' "$_p" "$_d"; else printf '%s: ' "$_p"; fi
  IFS= read -r _r || _r=""
  [ -z "$_r" ] && _r=$_d
  eval "$_v=\$_r"
}

asksecret() {     # asksecret VAR "prompt"  -- hidden
  _v=$1; _p=$2
  printf '%s: ' "$_p"
  if [ -t 0 ]; then
    stty -echo 2>/dev/null || true
    IFS= read -r _r || _r=""
    stty echo 2>/dev/null || true
    printf '\n'
  else
    IFS= read -r _r || _r=""      # non-tty (piped): nothing to hide from
  fi
  eval "$_v=\$_r"
}

echo "cmpbench credential setup"
echo "-------------------------"
echo "Token values are hidden as you type and are written only to $TOKENS (mode 600)."
echo

ask SHOPIFY_SHOP    "Shopify store handle (the X in X.myshopify.com), blank to skip" ""
if [ -n "$SHOPIFY_SHOP" ]; then
  ask SHOPIFY_VERSION "Shopify API version" "2026-07"
  asksecret SHOPIFY_TOKEN "Shopify Storefront access token"
fi

echo
ask SHOPLINE_HANDLE "SHOPLINE store handle (the X in X.myshopline.com), blank to skip" ""
if [ -n "$SHOPLINE_HANDLE" ]; then
  ask SHOPLINE_VERSION "SHOPLINE API version" "v20250301"
  asksecret SHOPLINE_TOKEN "SHOPLINE Storefront access token"
fi

if [ -z "${SHOPIFY_SHOP:-}" ] && [ -z "${SHOPLINE_HANDLE:-}" ]; then
  echo "Nothing configured." >&2
  exit 1
fi

# --- write tokens.sh -------------------------------------------------------
{
  echo "# Written by setup-tokens.sh. Gitignored. Do not commit, print, or paste."
  [ -n "${SHOPIFY_SHOP:-}" ]    && printf 'export SHOPIFY_STOREFRONT_TOKEN=%s\n'  "$(printf '%s' "${SHOPIFY_TOKEN:-}"  | sed "s/'/'\\\\''/g; s/^/'/; s/\$/'/")"
  [ -n "${SHOPLINE_HANDLE:-}" ] && printf 'export SHOPLINE_STOREFRONT_TOKEN=%s\n' "$(printf '%s' "${SHOPLINE_TOKEN:-}" | sed "s/'/'\\\\''/g; s/^/'/; s/\$/'/")"
} > "$TOKENS"
chmod 600 "$TOKENS"
echo
echo "wrote $TOKENS (mode 600)"

# --- write config.json -----------------------------------------------------
sf_enabled=false; sl_enabled=false
[ -n "${SHOPIFY_SHOP:-}" ] && sf_enabled=true
[ -n "${SHOPLINE_HANDLE:-}" ] && sl_enabled=true

cat > "$CONFIG" <<CFG
{
  "shopify": {
    "enabled": $sf_enabled,
    "shop": "${SHOPIFY_SHOP:-}",
    "version": "${SHOPIFY_VERSION:-2026-07}",
    "token_env": "SHOPIFY_STOREFRONT_TOKEN"
  },
  "shopline": {
    "enabled": $sl_enabled,
    "handle": "${SHOPLINE_HANDLE:-}",
    "version": "${SHOPLINE_VERSION:-v20250301}",
    "token_env": "SHOPLINE_STOREFRONT_TOKEN"
  },
  "fixtures": {
    "page_size": 10,
    "product_handles": [],
    "collection_handles": [],
    "search_terms": [],
    "accounts": []
  },
  "run": {
    "samples_per_scenario": 200,
    "warmup_per_scenario": 20,
    "concurrency": 1,
    "pair_gap_ms": 30,
    "qps_readonly": 4,
    "qps_auth": 0.5,
    "timeout": "10s",
    "accept_encoding_identity": true,
    "seed": 1,
    "bootstrap_iters": 2000,
    "throttle": {
      "shopify": { "mode": "qps", "qps": 4 },
      "shopline": { "mode": "compute_budget", "budget_seconds_per_second": 1.0, "safety": 0.6 }
    }
  },
  "gates": {
    "size_delta_max": 0.10,
    "req_size_abs_tolerance_bytes": 256,
    "max_semantic_depth": 2,
    "max_error_rate": 0.01
  }
}
CFG
echo "wrote $CONFIG  (product_handles is empty -- fill it before running preflight)"

# --- smoke test ------------------------------------------------------------
# __typename needs no scope, so a failure here is auth or endpoint, not access.
smoke() {
  _name=$1; _url=$2; _hdr=$3
  printf '  %-9s ' "$_name"
  _code=$(curl -sS -o /tmp/cmpbench_smoke.$$ -w '%{http_code}' -X POST "$_url" \
    -H 'Content-Type: application/json' -H "$_hdr" \
    --data '{"query":"{__typename}"}' 2>/dev/null) || { echo "connection failed"; return 1; }
  if [ "$_code" = "200" ] && ! grep -q '"errors"' /tmp/cmpbench_smoke.$$; then
    echo "OK (HTTP 200, GraphQL responded)"
    rm -f /tmp/cmpbench_smoke.$$; return 0
  fi
  echo "FAILED (HTTP $_code): $(head -c 200 /tmp/cmpbench_smoke.$$)"
  rm -f /tmp/cmpbench_smoke.$$; return 1
}

echo
echo "smoke test:"
rc=0
[ -n "${SHOPIFY_SHOP:-}" ] && { smoke shopify \
  "https://$SHOPIFY_SHOP.myshopify.com/api/${SHOPIFY_VERSION:-2026-07}/graphql.json" \
  "X-Shopify-Storefront-Access-Token: ${SHOPIFY_TOKEN:-}" || rc=1; }
[ -n "${SHOPLINE_HANDLE:-}" ] && { smoke shopline \
  "https://$SHOPLINE_HANDLE.myshopline.com/storefront/graph/${SHOPLINE_VERSION:-v20250301}/graphql.json" \
  "Authorization: Bearer ${SHOPLINE_TOKEN:-}" || rc=1; }

echo
if [ $rc -eq 0 ]; then
  echo "Ready. Next:"
  echo "  1. add real product/collection handles to $CONFIG"
  echo "  2. . ./tokens.sh && go build ./cmd/cmpbench && ./cmpbench probe -config $CONFIG"
else
  echo "At least one endpoint failed. Check the store handle, API version, and token."
fi
exit $rc
