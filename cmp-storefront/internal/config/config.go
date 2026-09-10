// Package config loads the run definition. JSON, not YAML, so the tool builds
// and runs with zero third-party dependencies.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type Account struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type ShopifyCfg struct {
	Enabled      bool              `json:"enabled"`
	Shop         string            `json:"shop"`
	Domain       string            `json:"domain"`
	Version      string            `json:"version"`
	TokenEnv     string            `json:"token_env"`
	ExtraHeaders map[string]string `json:"extra_headers"`
}

type ShoplineCfg struct {
	Enabled  bool   `json:"enabled"`
	Handle   string `json:"handle"`
	Domain   string `json:"domain"`
	Version  string `json:"version"`
	TokenEnv string `json:"token_env"`
	// ExtraHeaders are sent verbatim on every request. SHOPLINE rejects
	// product and collection queries from a token that is not bound to a sales
	// channel ("The channelHandle is required", PARAM_ILLEGAL), and the
	// parameter is not documented -- this makes satisfying it a config change
	// rather than a code change.
	ExtraHeaders map[string]string `json:"extra_headers"`
}

type Fixtures struct {
	ProductHandles    []string  `json:"product_handles"`
	CollectionHandles []string  `json:"collection_handles"`
	SearchTerms       []string  `json:"search_terms"`
	Accounts          []Account `json:"accounts"`
	PageSize          int       `json:"page_size"`
}

// Throttle configures pacing for one platform. Mode "qps" limits request rate;
// mode "compute_budget" limits observed server time per wall second, which is
// how SHOPLINE documents its Storefront limit.
type Throttle struct {
	Mode                   string  `json:"mode"`
	QPS                    float64 `json:"qps"`
	BudgetSecondsPerSecond float64 `json:"budget_seconds_per_second"`
	Safety                 float64 `json:"safety"`
}

type Run struct {
	Scenarios      []string            `json:"scenarios"`
	SamplesPerScen int                 `json:"samples_per_scenario"`
	Warmup         int                 `json:"warmup_per_scenario"`
	Concurrency    int                 `json:"concurrency"`
	PairGapMS      int                 `json:"pair_gap_ms"`
	QPSReadOnly    float64             `json:"qps_readonly"`
	QPSAuth        float64             `json:"qps_auth"`
	Timeout        string              `json:"timeout"`
	Identity       bool                `json:"accept_encoding_identity"`
	Seed           int64               `json:"seed"`
	BootstrapIters int                 `json:"bootstrap_iters"`
	Throttle       map[string]Throttle `json:"throttle"`
}

type Gates struct {
	SizeDeltaMax        float64 `json:"size_delta_max"`
	ReqSizeAbsTolerance int     `json:"req_size_abs_tolerance_bytes"`
	MaxSemanticDepth    int     `json:"max_semantic_depth"`
	MaxErrorRate        float64 `json:"max_error_rate"`
}

type Config struct {
	Shopify  ShopifyCfg  `json:"shopify"`
	Shopline ShoplineCfg `json:"shopline"`
	Fixtures Fixtures    `json:"fixtures"`
	Run      Run         `json:"run"`
	Gates    Gates       `json:"gates"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.applyDefaults()
	return &c, c.validate()
}

func (c *Config) applyDefaults() {
	if c.Fixtures.PageSize == 0 {
		c.Fixtures.PageSize = 10
	}
	if c.Run.SamplesPerScen == 0 {
		c.Run.SamplesPerScen = 200
	}
	if c.Run.Warmup == 0 {
		c.Run.Warmup = 20
	}
	if c.Run.Concurrency == 0 {
		c.Run.Concurrency = 1
	}
	if c.Run.PairGapMS == 0 {
		c.Run.PairGapMS = 30
	}
	if c.Run.QPSReadOnly == 0 {
		c.Run.QPSReadOnly = 4
	}
	if c.Run.Throttle == nil {
		c.Run.Throttle = map[string]Throttle{}
	}
	if c.Run.QPSAuth == 0 {
		c.Run.QPSAuth = 0.5
	}
	if c.Run.Timeout == "" {
		c.Run.Timeout = "10s"
	}
	if c.Run.BootstrapIters == 0 {
		c.Run.BootstrapIters = 2000
	}
	if c.Run.Seed == 0 {
		c.Run.Seed = 1
	}
	if c.Gates.SizeDeltaMax == 0 {
		c.Gates.SizeDeltaMax = 0.10
	}
	if c.Gates.ReqSizeAbsTolerance == 0 {
		c.Gates.ReqSizeAbsTolerance = 256
	}
	if c.Gates.MaxSemanticDepth == 0 {
		c.Gates.MaxSemanticDepth = 2
	}
	if c.Gates.MaxErrorRate == 0 {
		c.Gates.MaxErrorRate = 0.01
	}
	if c.Shopify.Version == "" {
		c.Shopify.Version = "2026-07"
	}
	if c.Shopline.Version == "" {
		c.Shopline.Version = "v20250301"
	}
	if c.Shopify.TokenEnv == "" {
		c.Shopify.TokenEnv = "SHOPIFY_STOREFRONT_TOKEN"
	}
	if c.Shopline.TokenEnv == "" {
		c.Shopline.TokenEnv = "SHOPLINE_STOREFRONT_TOKEN"
	}
}

func (c *Config) validate() error {
	if !c.Shopify.Enabled && !c.Shopline.Enabled {
		return fmt.Errorf("no platform enabled")
	}
	if c.Shopify.Enabled && c.Shopify.Shop == "" && c.Shopify.Domain == "" {
		return fmt.Errorf("shopify: set shop or domain")
	}
	if c.Shopline.Enabled && c.Shopline.Handle == "" && c.Shopline.Domain == "" {
		return fmt.Errorf("shopline: set handle or domain")
	}
	return nil
}

// RequireFixtures is checked by the commands that generate load, not by Load:
// the `fixtures` command exists precisely to populate an empty pool, and
// `probe` needs no fixtures at all.
func (c *Config) RequireFixtures() error {
	if len(c.Fixtures.ProductHandles) == 0 {
		return fmt.Errorf("fixtures.product_handles is empty -- run: cmpbench fixtures -config <file>")
	}
	return nil
}

func (c *Config) TimeoutDuration() time.Duration {
	d, err := time.ParseDuration(c.Run.Timeout)
	if err != nil {
		return 10 * time.Second
	}
	return d
}

// Token resolves a token from the environment. Tokens are never stored in the
// config file.
func Token(envName string) (string, error) {
	v := os.Getenv(envName)
	if v == "" {
		return "", fmt.Errorf("environment variable %s is empty", envName)
	}
	return v, nil
}
