// runner config persistence — Register writes a JSON file describing the
// scope (org/repo) and the credentials Run will use to mint JIT configs.
//
// We DO NOT persist a registration token here: tokens expire after ~1h and
// would be useless by the time Run reads them. Instead we persist the
// caller's authentication credential (PAT / app token) — the same one passed
// to Register — so Run can mint fresh JIT configs on demand.
//
// Storing the PAT on disk is a known sharp edge. The file is created with
// 0600. Operators who want stronger isolation should either (a) provision
// the PAT via systemd EnvironmentFile/LoadCredential and pass it through an
// env var instead of --token (the binary reads `WEFT_RUNNER_GITHUB_TOKEN`
// first when present), or (b) front the runner with a GitHub App and rotate
// the installation token periodically out-of-band.

package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// PersistedConfig is the on-disk shape of `weft-runner-github register`.
// JSON-marshalled with omitempty on the optional fields so a hand-edited
// config (e.g. an operator switching a runner from repo to org scope) stays
// human-friendly.
type PersistedConfig struct {
	Owner         string   `json:"owner"`
	Repo          string   `json:"repo,omitempty"`
	Token         string   `json:"token"`
	Labels        []string `json:"labels,omitempty"`
	RunnerGroupID int      `json:"runner_group_id,omitempty"`
	// NamePrefix is used to name each ephemeral runner: <prefix>-<random>.
	// Defaults to "weft" — left explicit in the JSON so a cluster running
	// multiple runner daemons against the same org can disambiguate them
	// in the GitHub UI.
	NamePrefix string `json:"name_prefix,omitempty"`
}

func writeConfig(path string, cfg PersistedConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	// 0600: the file holds a token. Owner-only is the right default; an
	// operator who needs group readability should override after the fact.
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func readConfig(path string) (PersistedConfig, error) {
	var cfg PersistedConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w (run `weft-runner-github register` first)", path, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("decode %s: %w", path, err)
	}
	// Env override — see the package doc above on why we accept this
	// (LoadCredential-style operator workflows).
	if v := os.Getenv("WEFT_RUNNER_GITHUB_TOKEN"); v != "" {
		cfg.Token = v
	}
	if cfg.Owner == "" {
		return cfg, fmt.Errorf("config %s missing owner", path)
	}
	if cfg.Token == "" {
		return cfg, fmt.Errorf("config %s missing token (and WEFT_RUNNER_GITHUB_TOKEN unset)", path)
	}
	if cfg.NamePrefix == "" {
		cfg.NamePrefix = "weft"
	}
	return cfg, nil
}
