//go:build windows || (linux && cgo)

package main

import (
	"fmt"
	"net/url"
	"os"
)

// SignalingURL returns only a configured HTTPS origin, never an insecure default.
func (*App) SignalingURL() (string, error) {
	u, err := url.Parse(os.Getenv("SCREENSHARE_URL"))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("set SCREENSHARE_URL to an HTTPS origin, for example https://share.example.com")
	}
	return "https://" + u.Host, nil
}
