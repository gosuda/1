package flowlet

import "github.com/nats-io/nats.go"

type Flowlet struct {
	conn *nats.Conn
}
