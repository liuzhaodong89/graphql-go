#!/bin/sh
# Identify what a Shopify token actually is, and optionally mint the Storefront
# token cmpbench needs.
#
#   ./verify-token.sh <shop.myshopify.com> <path-to-token-file>
#   ./verify-token.sh <shop.myshopify.com> <path-to-token-file> --mint
#
# The token value is never printed. Run this yourself: it probes your own
# store's auth endpoints, which only the store owner should be doing.
set -eu

SHOP=${1:?usage: verify-token.sh <shop.myshopify.com> <token-file> [--mint]}
FILE=${2:?usage: verify-token.sh <shop.myshopify.com> <token-file> [--mint]}
MINT=${3:-}
VER=2025-01
TMP=$(mktemp); trap 'rm -f "$TMP"' EXIT

TOK=$(tr -d ' \t\r\n' < "$FILE")
[ -n "$TOK" ] || { echo "token file is empty" >&2; exit 1; }
mask() { sed "s|$TOK|<REDACTED>|g" "$TMP" | head -c 400; echo; }

probe() {  # probe LABEL URL HEADER BODY
  _code=$(curl -sS -m 20 -o "$TMP" -w '%{http_code}' -X POST "$2" \
    -H 'Content-Type: application/json' -H "$3" --data "$4" 2>&1) || _code=curl-error
  printf '  %-22s HTTP %-12s ' "$1" "$_code"
  if [ "$_code" = 200 ] && ! grep -q '"errors"' "$TMP"; then echo "OK"; return 0; fi
  mask; return 1
}

echo "shop: $SHOP"
echo

echo "1. Storefront API (this is what cmpbench needs)"
if probe "storefront" "https://$SHOP/api/$VER/graphql.json" \
     "X-Shopify-Storefront-Access-Token: $TOK" '{"query":"{__typename}"}'; then
  echo
  echo "This IS a Storefront access token. Point cmpbench at it:"
  echo "  export SHOPIFY_STOREFRONT_TOKEN=\$(tr -d ' \\t\\r\\n' < $FILE)"
  exit 0
fi

echo
echo "2. Admin API (to see whether it is an Admin token instead)"
if ! probe "admin graphql" "https://$SHOP/admin/api/$VER/graphql.json" \
       "X-Shopify-Access-Token: $TOK" '{"query":"{shop{name}}"}'; then
  echo
  echo "Not valid for either API on $SHOP."
  echo "Check that the token belongs to THIS store, and that you copied the"
  echo "Storefront API access token (32 lowercase hex) rather than a secret or"
  echo "an API key. Admin > Settings > Apps and sales channels > Develop apps"
  echo "> your app > API credentials."
  exit 1
fi

echo
echo "This is an ADMIN API token, not a Storefront token."
if [ "$MINT" != "--mint" ]; then
  echo "It can mint the Storefront token you need. Re-run with --mint to do that:"
  echo "  $0 $SHOP $FILE --mint"
  echo "(needs the unauthenticated_read_product_listings scope on the app)"
  exit 1
fi

echo "minting a Storefront access token..."
curl -sS -m 20 -o "$TMP" -X POST "https://$SHOP/admin/api/$VER/graphql.json" \
  -H 'Content-Type: application/json' -H "X-Shopify-Access-Token: $TOK" \
  --data '{"query":"mutation($i:StorefrontAccessTokenInput!){storefrontAccessTokenCreate(input:$i){storefrontAccessToken{accessToken title}userErrors{field message}}}","variables":{"i":{"title":"cmpbench-latency-benchmark"}}}'

NEW=$(sed -n 's/.*"accessToken":"\([^"]*\)".*/\1/p' "$TMP")
if [ -z "$NEW" ]; then
  echo "mint failed:"; mask; exit 1
fi

OUT=$(dirname "$0")/tokens.sh
umask 077
printf '# Written by verify-token.sh. Gitignored. Do not commit or paste.\nexport SHOPIFY_STOREFRONT_TOKEN=%s\n' "'$NEW'" > "$OUT"
chmod 600 "$OUT"
echo "wrote $OUT (mode 600) with the new Storefront token"
echo
echo "Next:  . ./tokens.sh && ./cmpbench probe -config config.json"
