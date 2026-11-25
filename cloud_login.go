package kosa8

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// planLimitCodes are refusals that mean "pick or pay for a plan", raised as
// KindPaymentRequired whatever the HTTP status.
var planLimitCodes = map[string]bool{"starter_limit": true, "plan_limit": true, "plan_feature": true, "plan_required": true}

// CloudCredentialsPath is where `kosa8 login` keeps the kosa8 Cloud key:
// ~/.kosa8/cloud.json. getenv decides HOME; with a test getenv (explicit),
// an unset HOME means no file.
func CloudCredentialsPath(getenv func(string) string, explicit bool) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	home := getenv("HOME")
	if home == "" {
		home = getenv("USERPROFILE")
	}
	if home == "" && !explicit {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".kosa8", "cloud.json")
}

// savedLogin reads the key and API address `kosa8 login` saved; empty when
// there is none or it cannot be read.
func savedLogin(getenv func(string) string, explicit bool) (key, apiURL string) {
	p := CloudCredentialsPath(getenv, explicit)
	if p == "" {
		return "", ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", ""
	}
	var c struct {
		Key    string `json:"key"`
		APIKey string `json:"api_key"`
		APIURL string `json:"api_url"`
	}
	if json.Unmarshal(b, &c) != nil {
		return "", ""
	}
	if c.Key == "" {
		c.Key = c.APIKey
	}
	return strings.TrimSpace(c.Key), strings.TrimSpace(c.APIURL)
}
