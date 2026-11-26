package core

import (
	"context"

	"telemetry.gosuda.org/telemetry/internal/types"
)

func DoHealthCheck(is types.InternalServiceProvider) error {
	err := is.Ping(context.Background())
	if err != nil {
		return err
	}

	_, err = is.GenerateID()
	if err != nil {
		return err
	}

	return nil
}
