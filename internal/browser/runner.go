package browser

import (
	"context"
	"time"
)

type Runner interface {
	Run(context.Context, Action, time.Duration) (Observation, error)
}
