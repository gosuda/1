package utils

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type EndpointOperation func(ctx context.Context, endpoint string) error

func EndpointCandidates(endpoints []string) []string {
	candidates := make([]string, 0, len(endpoints))
	seen := make(map[string]struct{}, len(endpoints))

	appendEndpoint := func(endpoint string) {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" {
			return
		}
		key := strings.ToLower(endpoint)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, endpoint)
	}

	for _, endpoint := range endpoints {
		appendEndpoint(endpoint)
	}

	return candidates
}

func SelectEndpoint(ctx context.Context, candidates []string, operation EndpointOperation) (string, error) {
	return DoWithEndpoint(ctx, candidates, operation)
}

func DoWithEndpoint(ctx context.Context, candidates []string, operation EndpointOperation) (string, error) {
	if len(candidates) == 0 {
		return "", errors.New("no endpoints configured")
	}
	if operation == nil {
		return candidates[0], nil
	}

	failures := make([]string, 0, len(candidates))
	for _, endpoint := range candidates {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" {
			continue
		}
		if err := operation(ctx, endpoint); err == nil {
			return endpoint, nil
		} else {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return "", err
			}
			failures = append(failures, fmt.Sprintf("%s: %v", endpoint, err))
		}
	}

	if len(failures) == 0 {
		return "", errors.New("no endpoints configured")
	}
	return "", fmt.Errorf("all endpoints failed: %s", strings.Join(failures, "; "))
}
