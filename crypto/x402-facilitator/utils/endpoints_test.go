package utils

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEndpointCandidatesKeepsInputOrder(t *testing.T) {
	defaults := []string{
		"https://primary.publicnode.com",
		"https://secondary.example.com",
	}

	input := append([]string{" https://custom.example.com "}, defaults...)
	candidates := EndpointCandidates(input)

	require.Equal(t, []string{
		"https://custom.example.com",
		"https://primary.publicnode.com",
		"https://secondary.example.com",
	}, candidates)
	require.Equal(t, []string{
		"https://primary.publicnode.com",
		"https://secondary.example.com",
	}, defaults, "defaults should not be mutated")
}

func TestEndpointCandidatesDeduplicates(t *testing.T) {
	candidates := EndpointCandidates([]string{
		"https://primary.publicnode.com",
		"https://PRIMARY.publicnode.com",
		"https://secondary.example.com",
	})

	require.Equal(t, []string{
		"https://primary.publicnode.com",
		"https://secondary.example.com",
	}, candidates)
}

func TestSelectEndpointFallsBackInOrder(t *testing.T) {
	candidates := []string{"first", "second", "third"}
	attempted := make([]string, 0, len(candidates))

	selected, err := SelectEndpoint(context.Background(), candidates, func(ctx context.Context, endpoint string) error {
		attempted = append(attempted, endpoint)
		if endpoint != "third" {
			return errors.New("unavailable")
		}
		return nil
	})

	require.NoError(t, err)
	require.Equal(t, "third", selected)
	require.Equal(t, candidates, attempted)
}

func TestSelectEndpointReturnsFirstWithoutProbe(t *testing.T) {
	selected, err := SelectEndpoint(context.Background(), []string{"first", "second"}, nil)

	require.NoError(t, err)
	require.Equal(t, "first", selected)
}

func TestDoWithEndpointRunsOperationUntilSuccess(t *testing.T) {
	attempted := make([]string, 0, 2)

	selected, err := DoWithEndpoint(context.Background(), []string{"first", "second"}, func(ctx context.Context, endpoint string) error {
		attempted = append(attempted, endpoint)
		if endpoint == "first" {
			return errors.New("unavailable")
		}
		return nil
	})

	require.NoError(t, err)
	require.Equal(t, "second", selected)
	require.Equal(t, []string{"first", "second"}, attempted)
}

func TestDoWithEndpointStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := DoWithEndpoint(ctx, []string{"first", "second"}, func(ctx context.Context, endpoint string) error {
		t.Fatalf("operation should not run after context cancellation")
		return nil
	})

	require.ErrorIs(t, err, context.Canceled)
}

func TestDoWithEndpointStopsOnContextError(t *testing.T) {
	attempted := 0

	_, err := DoWithEndpoint(context.Background(), []string{"first", "second"}, func(ctx context.Context, endpoint string) error {
		attempted++
		return context.DeadlineExceeded
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, attempted)
}
