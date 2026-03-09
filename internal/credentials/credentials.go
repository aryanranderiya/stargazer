package credentials

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Credentials holds persistent auth configuration.
type Credentials struct {
	Tokens []string `json:"tokens"`
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "stargazer", "credentials.json"), nil
}

// Load reads credentials from disk. Returns an empty struct on missing file.
func Load() (*Credentials, error) {
	p, err := configPath()
	if err != nil {
		return &Credentials{}, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &Credentials{}, nil
		}
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return &Credentials{}, nil
	}
	return &c, nil
}

// Save writes credentials to disk with 0600 permissions.
func Save(c *Credentials) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}
