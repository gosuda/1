package record

import "time"

type State string

const (
	StateStandby State = "standby"   // Chain is in standby state, waiting for evaluation
	Running      State = "running"   // Chain is currently being evaluated
	Completed    State = "completed" // Chain evaluation has completed successfully
	Failed       State = "failed"    // Chain evaluation has failed
)

type ChainHistory struct {
	ChainName string    `json:"chain_name"` // Name of the chain
	EvalTime  time.Time `json:"eval_time"`  // Time when the chain was evaluated
	Flowlet   string    `json:"flowlet"`    // Name of the flowlet that was executed
	State     State     `json:"state"`      // State of the chain after evaluation
	Message   string    `json:"message"`    // Additional message or error description
}
