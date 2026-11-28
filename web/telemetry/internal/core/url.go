package core

import (
	"fmt"
	"net/url"
	"strings"
)

// NormalizeURL normalizes a URL to "host + pathname".
// If parsing fails, it attempts to add "http://" and parse again.
func NormalizeURL(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("empty url")
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		u, err = url.Parse("http://" + raw)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("invalid url: %w", err)
		}
	}

	host := u.Host
	// strip port if present
	if i := strings.Index(host, ":"); i != -1 {
		host = host[:i]
	}

	path := u.Path
	if path == "" {
		path = "/"
	}
	// remove trailing slash except for root "/"
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		path = strings.TrimRight(path, "/")
	}

	return host + path, nil
}
