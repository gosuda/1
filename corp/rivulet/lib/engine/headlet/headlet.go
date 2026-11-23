package headlet

import (
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

const (
	ClusterName = "rivulet-cluster"
)

type Headlet struct {
	server *server.Server
	conn   *nats.Conn
}

type Config struct {
	Host        string
	QueuePort   int
	ClusterPort int
}

func New(cfg *Config) (*Headlet, error) {
	srv, err := server.NewServer(&server.Options{
		Host: cfg.Host,
		Port: cfg.QueuePort,
		Cluster: server.ClusterOpts{
			Name: ClusterName,
			Host: cfg.Host,
			Port: cfg.ClusterPort,
		},
	})
	if err != nil {
		return nil, err
	}

	conn, err := nats.Connect(srv.ClientURL())
	if err != nil {
		return nil, err
	}

	return &Headlet{
		server: srv,
		conn:   conn,
	}, nil
}
