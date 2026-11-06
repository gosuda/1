package opt

import (
	"math"
	"sync"
	"time"
)

type PlanMode uint8

const (
	CustomPlan PlanMode = iota + 1
	GenericPlan
)

// ParameterPolicy keeps only aggregate cost observations, never parameter
// values, plans or readers. Its zero value is ready for concurrent use.
type ParameterPolicy struct {
	mu                       sync.Mutex
	custom                   uint32
	mean, planning, variance float64
	generic                  float64
	hasGeneric               bool
}

// ObserveCustom costs must use the same units as ObserveGeneric. PlanningUnits
// is the measured planning duration converted by the caller's cost calibration.
func (p *ParameterPolicy) ObserveCustom(executionCost float64, planning time.Duration, unitsPerSecond float64) error {
	if !validCost(executionCost) || planning < 0 || !validCost(unitsPerSecond) || unitsPerSecond == 0 {
		return ErrInvalid
	}
	planningUnits := planning.Seconds() * unitsPerSecond
	if !validCost(planningUnits) {
		return ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.custom = min(p.custom+1, 32)
	weight := 1 / float64(p.custom)
	delta := executionCost - p.mean
	p.mean += weight * delta
	p.planning += weight * (planningUnits - p.planning)
	// A 32-observation exponential window bounds both state and the influence
	// of obsolete parameter distributions after the initial sample.
	p.variance = (1 - weight) * (p.variance + weight*delta*delta)
	if math.IsInf(p.variance, 0) {
		p.variance = math.MaxFloat64
	}
	return nil
}
func (p *ParameterPolicy) ObserveGeneric(executionCost float64) error {
	if !validCost(executionCost) {
		return ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.generic, p.hasGeneric = executionCost, true
	return nil
}
func (p *ParameterPolicy) Mode() PlanMode {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.custom < 5 || !p.hasGeneric {
		return CustomPlan
	}
	// A noisy custom population is not evidence that a generic plan is robust.
	if p.variance > p.mean*p.mean {
		return CustomPlan
	}
	if p.generic <= p.mean+p.planning {
		return GenericPlan
	}
	return CustomPlan
}
func validCost(value float64) bool { return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0) }
