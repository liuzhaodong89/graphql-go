// slauth walks SHOPLINE's OAuth flow to obtain a Storefront access token.
//
//	slauth url      -handle <h> [-scope ...]        # print the URL the merchant opens
//	slauth exchange -handle <h> -code <code>        # code -> admin token -> storefront token
//	slauth refresh  -handle <h>                     # fresh Admin token (no code needed)
//	slauth mint     -handle <h>                     # refresh + mint a new Storefront token
//
// SHOPLINE has no client_credentials grant: every route to an Admin API token
// requires a merchant to approve in a browser. So this is two steps with a
// human in the middle, not one command.
//
// Credentials are read from a file of the form:
//
//	appkey:<value>
//	appsecret:<value>
//
// and are never printed.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type creds struct{ AppKey, AppSecret string }

func loadCreds(path string) (creds, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return creds{}, err
	}
	var c creds
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "appkey":
			c.AppKey = strings.TrimSpace(v)
		case "appsecret":
			c.AppSecret = strings.TrimSpace(v)
		}
	}
	if c.AppKey == "" || c.AppSecret == "" {
		return c, fmt.Errorf("%s: need both appkey: and appsecret: lines", path)
	}
	return c, nil
}

// sign implements SHOPLINE's POST signature: HMAC-SHA256 over (body + timestamp)
// keyed by the app secret, hex encoded.
func sign(secret, body, timestamp string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body + timestamp))
	return hex.EncodeToString(m.Sum(nil))
}

func nowMillis() string { return strconv.FormatInt(time.Now().UnixMilli(), 10) }

// redact keeps secrets out of anything printed, including error bodies that
// echo the request back.
func redact(s string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) >= 8 {
			s = strings.ReplaceAll(s, sec, "<REDACTED>")
		}
	}
	return s
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: slauth <url|exchange> -handle <store> [-creds file] [-code ...]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	handle := fs.String("handle", "", "store handle (the X in X.myshopline.com)")
	credFile := fs.String("creds", "", "path to the appkey/appsecret file")
	// "storefront_tokens" is NOT a SHOPLINE permission point -- it does not
	// appear in the access-scope reference. The requested scope must also be a
	// SUBSET of what the app is configured with in the Developer Center, so
	// keep the default minimal and override it to match the app.
	//
	// SHOPLINE's unauthenticated_* family covers cart, checkouts_info,
	// customer_information, message and metaobjects -- there is no
	// product-listing scope, so Storefront product reads are not gated by one.
	scope := fs.String("scope", "read_products", "comma-separated scopes to request; must be a subset of the app's configured scopes")
	redirect := fs.String("redirect", "https://www.baidu.com", "redirect URI registered in the Partner Portal")
	code := fs.String("code", "", "authorization code from the redirect (exchange only)")
	version := fs.String("version", "v20250301", "admin openapi version")
	out := fs.String("out", "tokens.sh", "file to write the storefront token into")
	_ = fs.Parse(os.Args[2:])

	if *handle == "" || *credFile == "" {
		fmt.Fprintln(os.Stderr, "error: -handle and -creds are required")
		os.Exit(2)
	}
	c, err := loadCreds(*credFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	switch cmd {
	case "url":
		u := fmt.Sprintf(
			"https://%s.myshopline.com/admin/oauth-web/#/oauth/authorize?appKey=%s&responseType=code&scope=%s&redirectUri=%s",
			*handle, url.QueryEscape(c.AppKey), url.QueryEscape(*scope), url.QueryEscape(*redirect))
		fmt.Println(u)
		fmt.Fprintf(os.Stderr, `
Preconditions for a CUSTOM app (the authorize page rejects the store otherwise):
  1. %s must be in the app's Available Store List
     (Developer Center > Apps > your app > Merchant Use > Configure the
     Available Store List > Merchant Store Handle). Every handle in that list
     must belong to the same merchant.
  2. The redirect URI must be registered verbatim as an App Callback URL,
     including or excluding the trailing slash exactly as passed here:
       %s
  3. The requested scope must be a subset of the app's configured scopes:
       %s
  4. Be logged into https://%s.myshopline.com/admin in the same browser first.
`, *handle, *redirect, *scope, *handle)
	case "refresh":
		// The Admin token lasts 10 hours and the authorization code is
		// single-use, so re-running `exchange` is not an option. Refresh is
		// keyed on appkey + store, which is what makes fixture seeding
		// repeatable across a long benchmark session.
		tok, err := adminToken(c, *handle, "", true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", redact(err.Error(), c.AppSecret))
			os.Exit(1)
		}
		fmt.Printf("export SHOPLINE_ADMIN_TOKEN='%s'\n", tok)
	case "mint":
		// SHOPLINE storefront tokens carry no exp claim but do stop working,
		// so a long benchmark session needs to re-mint without dragging the
		// merchant back through a browser. Refresh is keyed on appkey + store,
		// which makes that possible.
		admin, err := adminToken(c, *handle, "", true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", redact(err.Error(), c.AppSecret))
			os.Exit(1)
		}
		if err := mintStorefront(admin, *handle, *version, *out); err != nil {
			fmt.Fprintln(os.Stderr, "error:", redact(err.Error(), c.AppSecret, admin))
			os.Exit(1)
		}
	case "exchange":
		if *code == "" {
			fmt.Fprintln(os.Stderr, "error: -code is required (it expires 10 minutes after the redirect)")
			os.Exit(2)
		}
		if err := exchange(c, *handle, *code, *version, *out); err != nil {
			fmt.Fprintln(os.Stderr, "error:", redact(err.Error(), c.AppSecret))
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", cmd)
		os.Exit(2)
	}
}

// adminToken obtains an Admin API access token, either by exchanging a
// single-use authorization code or by refreshing.
func adminToken(c creds, handle, code string, refresh bool) (string, error) {
	path := "create"
	body := []byte("{}")
	if refresh {
		path = "refresh"
	} else {
		body, _ = json.Marshal(map[string]string{"code": code})
	}
	ts := nowMillis()

	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("https://%s.myshopline.com/admin/oauth/token/%s", handle, path),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("appkey", c.AppKey)
	req.Header.Set("timestamp", ts)
	req.Header.Set("sign", sign(c.AppSecret, string(body), ts))

	raw, status, err := do(req)
	if err != nil {
		return "", err
	}
	var tok struct {
		Data struct {
			AccessToken string `json:"accessToken"`
			ExpireTime  string `json:"expireTime"`
			Scope       string `json:"scope"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &tok)
	if tok.Data.AccessToken == "" {
		return "", fmt.Errorf("no accessToken from /%s (HTTP %d): %s\n\n  STORE_NOT_INSTALL_APP -> the app is no longer installed on this store. Storefront\n     tokens are revoked on uninstall, which is why one can stop working with no\n     exp claim in it. Re-run `slauth url` and re-authorize to reinstall.\n  OAUTH_CODE_INVALID -> the code expired (10 min) or was already used; re-open the authorize URL\n  REQUEST_NOT_IN_APP_IP_WHITELIST -> add this machine's egress IP in the Partner Portal\n  sign errors -> appkey/appsecret mismatch",
			path, status, truncate(string(raw), 400))
	}
	fmt.Fprintf(os.Stderr, "  admin token OK  scope=%s  expires=%s\n", tok.Data.Scope, tok.Data.ExpireTime)
	return tok.Data.AccessToken, nil
}

func exchange(c creds, handle, code, version, out string) error {
	fmt.Println("step 1/3  authorization code -> Admin API token")
	admin, err := adminToken(c, handle, code, false)
	if err != nil {
		return err
	}

	fmt.Println("\nstep 2/3  mint Storefront access token")
	return mintStorefront(admin, handle, version, out)
}

func mintStorefront(admin, handle, version, out string) error {
	sfBody, _ := json.Marshal(map[string]any{
		"storefront_access_token": map[string]string{"title": "cmpbench-latency-benchmark"},
	})
	req2, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("https://%s.myshopline.com/admin/openapi/%s/storefront_access_tokens.json", handle, version),
		bytes.NewReader(sfBody))
	req2.Header.Set("Content-Type", "application/json; charset=utf-8")
	req2.Header.Set("Accept", "application/json")
	req2.Header.Set("Authorization", "Bearer "+admin)

	raw2, status2, err := do(req2)
	if err != nil {
		return err
	}
	var sfResp struct {
		StorefrontAccessToken struct {
			AccessToken string `json:"access_token"`
			AccessScope string `json:"access_scope"`
			Title       string `json:"title"`
		} `json:"storefront_access_token"`
	}
	_ = json.Unmarshal(raw2, &sfResp)
	sfTok := sfResp.StorefrontAccessToken.AccessToken
	if sfTok == "" {
		return fmt.Errorf("no storefront access_token (HTTP %d): %s\n\n  If this is a scope problem, the app needs the storefront_tokens scope.\n  If it is a version problem, try -version v20260301 or v20270301.",
			status2, redact(truncate(string(raw2), 400), admin))
	}
	fmt.Printf("  OK  scopes=%s\n", sfResp.StorefrontAccessToken.AccessScope)

	fmt.Println("\nstep 3/3  verify against the Storefront API")
	q, _ := json.Marshal(map[string]string{"query": "{products(first:3){nodes{handle title}}}"})
	req3, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("https://%s.myshopline.com/storefront/graph/%s/graphql.json", handle, version),
		bytes.NewReader(q))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Authorization", "Bearer "+sfTok)
	raw3, status3, err := do(req3)
	if err != nil {
		return err
	}
	fmt.Printf("  HTTP %d : %s\n", status3, redact(truncate(string(raw3), 400), sfTok))

	old, _ := os.ReadFile(out)
	var kept []string
	for _, line := range strings.Split(string(old), "\n") {
		// Preserve the Shopify token already in this file.
		if strings.Contains(line, "SHOPIFY_STOREFRONT_TOKEN") {
			kept = append(kept, line)
		}
	}
	content := "# Written by slauth/mint scripts. Gitignored. Do not commit or paste.\n" +
		strings.Join(kept, "\n")
	if len(kept) > 0 {
		content += "\n"
	}
	content += "export SHOPLINE_STOREFRONT_TOKEN='" + sfTok + "'\n"
	// The Admin token is needed for fixture seeding and product listing. It
	// lasts 10 hours while the authorization code is single-use, so losing it
	// here would mean another round-trip through the merchant's browser.
	// Refresh it later with `slauth refresh`.
	content += "export SHOPLINE_ADMIN_TOKEN='" + admin + "'\n"
	if err := os.WriteFile(out, []byte(content), 0o600); err != nil {
		return err
	}
	fmt.Printf("\nwrote %s (mode 600)\n", out)
	return nil
}

func do(req *http.Request) ([]byte, int, error) {
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
