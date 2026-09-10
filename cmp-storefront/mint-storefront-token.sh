#!/bin/sh
# Mint a Storefront API access token from an app's client credentials.
#
#   ./mint-storefront-token.sh <shop.myshopify.com> <client_id> <secret-file>
#
# The secret file may be a bare secret or "label:secret" (the label is stripped).
# Chain: client_credentials grant -> Admin API token -> storefrontAccessTokenCreate
#        -> tokens.sh (mode 600) -> verification query.
#
# Requires: the app and the store are in the SAME Shopify organization (this is
# what the client_credentials grant is limited to), and the app already holds at
# least one unauthenticated_* scope -- a storefront token inherits its scopes
# from the app that creates it, so an app with none cannot mint one.
set -eu

SHOP=${1:?usage: mint-storefront-token.sh <shop.myshopify.com> <client_id> <secret-file>}
CID=${2:?missing client_id}
SECFILE=${3:?missing secret file}
VER=${VER:-2025-01}
TMP=$(mktemp); trap 'rm -f "$TMP"' EXIT

RAW=$(tr -d ' \t\r\n' < "$SECFILE")
case "$RAW" in *:*) SEC=${RAW#*:} ;; *) SEC=$RAW ;; esac
[ -n "$SEC" ] || { echo "secret file is empty" >&2; exit 1; }
mask() { sed -e "s|$SEC|<SECRET>|g" -e "s|$CID|<CLIENT_ID>|g" "$TMP" | head -c 500; echo; }

echo "shop: $SHOP   api version: $VER"
echo
echo "step 1/3  client_credentials grant -> Admin API token"
CODE=$(curl -sS -m 25 -o "$TMP" -w '%{http_code}' -X POST \
  "https://$SHOP/admin/oauth/access_token" \
  --data-urlencode "grant_type=client_credentials" \
  --data-urlencode "client_id=$CID" \
  --data-urlencode "client_secret=$SEC" 2>&1) || CODE=curl-error
if [ "$CODE" != 200 ]; then
  printf '  FAILED (HTTP %s): ' "$CODE"; mask
  echo
  echo "  shop_not_permitted  -> the app and this store are in different Shopify"
  echo "                         organizations. client_credentials cannot reach it;"
  echo "                         you need the authorization code grant (install link)."
  echo "  invalid_client      -> client_id and client_secret do not match this app."
  exit 1
fi
ADMIN=$(sed -n 's/.*"access_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$TMP")
[ -n "$ADMIN" ] || { echo "  no access_token in response:"; mask; exit 1; }
echo "  OK (Admin token acquired; valid ~24h)"

echo
echo "step 2/3  storefrontAccessTokenCreate"
CODE=$(curl -sS -m 25 -o "$TMP" -w '%{http_code}' -X POST \
  "https://$SHOP/admin/api/$VER/graphql.json" \
  -H 'Content-Type: application/json' -H "X-Shopify-Access-Token: $ADMIN" \
  --data '{"query":"mutation M($i:StorefrontAccessTokenInput!){storefrontAccessTokenCreate(input:$i){storefrontAccessToken{accessToken title accessScopes{handle}}userErrors{field message}}}","variables":{"i":{"title":"cmpbench-latency-benchmark"}}}' 2>&1) || CODE=curl-error
SFTOK=$(sed -n 's/.*"accessToken"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$TMP")
if [ "$CODE" != 200 ] || [ -z "$SFTOK" ]; then
  printf '  FAILED (HTTP %s): ' "$CODE"
  sed -e "s|$ADMIN|<ADMIN_TOKEN>|g" "$TMP" | head -c 500; echo
  echo
  echo "  ACCESS_DENIED / 403 -> the app holds no unauthenticated_* scope. Add"
  echo "     unauthenticated_read_product_listings (and unauthenticated_read_product_inventory"
  echo "     if you want the inventory field) to the app, then re-run."
  echo "  There is NO scope named write_storefront_access_tokens -- do not look for one."
  exit 1
fi
echo "  OK  scopes: $(sed -n 's/.*"accessScopes":\[\(.*\)\],"*.*/\1/p' "$TMP" | head -c 200)"

echo
echo "step 3/3  verify against the Storefront API"
CODE=$(curl -sS -m 25 -o "$TMP" -w '%{http_code}' -X POST \
  "https://$SHOP/api/$VER/graphql.json" \
  -H 'Content-Type: application/json' -H "X-Shopify-Storefront-Access-Token: $SFTOK" \
  --data '{"query":"{products(first:3){nodes{handle}}}"}' 2>&1) || CODE=curl-error
printf '  HTTP %s : ' "$CODE"
sed "s|$SFTOK|<STOREFRONT_TOKEN>|g" "$TMP" | head -c 300; echo

OUT=$(dirname "$0")/tokens.sh
umask 077
printf '# Written by mint-storefront-token.sh. Gitignored. Do not commit or paste.\nexport SHOPIFY_STOREFRONT_TOKEN=%s\n' "'$SFTOK'" > "$OUT"
chmod 600 "$OUT"
echo
echo "wrote $OUT (mode 600)"
if [ "$CODE" = 200 ] && ! grep -q '"errors"' "$TMP"; then
  echo "Ready:  . ./tokens.sh && ./cmpbench fixtures -config config.json"
else
  echo "Token minted, but the Storefront API did not serve data. If the error says"
  echo "'Online Store channel is locked', fix that in the admin first -- no token"
  echo "can work around it."
fi
