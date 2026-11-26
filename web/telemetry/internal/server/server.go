package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/rs/zerolog/log"
	"gosuda.org/randflake"
	"telemetry.gosuda.org/telemetry/internal/api"
	"telemetry.gosuda.org/telemetry/internal/types"
)

const (
	_RANDFLAKE_RENEW_WINDOW = int64(time.Minute * 9)
	_RANDFLAKE_SAFE_WINDOW  = int64(time.Second * 30)
)

var (
	ErrRandflakeLeaseCreate = errors.New("server: failed to create randflake lease")
)

type Server struct {
	mux *httprouter.Router

	ps     types.PersistenceService
	stopCh chan struct{}

	lease        *types.RandflakeLease
	randflake    *randflake.Generator
	randflakeKey []byte
}

var _ types.InternalServiceProvider = (*serverServiceProvider)(nil)

type serverServiceProvider struct {
	types.PersistenceService
	s *Server
}

func (g *serverServiceProvider) GenerateID() (int64, error) {
	return g.s.randflake.Generate()
}

func (g *serverServiceProvider) GenerateIDString() (string, error) {
	return g.s.randflake.GenerateString()
}

type ServerConfig struct {
	PersistenceService types.PersistenceService
	RandflakeSecret    string `env:"RANDFLAKE_SECRET,required"`
}

// NewServer creates a new server instance
func NewServer(c *ServerConfig) (*Server, error) {
	g := &Server{
		ps:     c.PersistenceService,
		mux:    httprouter.New(),
		stopCh: make(chan struct{}),
	}

	ctx := context.Background()

	log.Debug().Msg("pinging persistence service")
	err := g.ps.Ping(ctx)
	if err != nil {
		log.Error().Err(err).Msg("failed to ping persistence service")
		return nil, err
	}
	log.Debug().Msg("persistence service ping successful")

	log.Debug().Msg("creating initial randflake lease")
	retry := 0
	for retry < 3 {
		lease, err := g.ps.RandflakeLeaseCreate(ctx)
		if err != nil {
			log.Error().Err(err).Msg("failed to create randflake lease")
			retry++
			continue
		}
		g.lease = lease
		break
	}
	if g.lease == nil {
		log.Error().Msg("failed to create randflake lease")
		return nil, ErrRandflakeLeaseCreate
	}
	log.Debug().Int64("expires_at", g.lease.ExpiresAt).Int64("nodeid", g.lease.NodeID).Msg("randflake lease created")

	randflakeSecretKey := sha256.Sum256([]byte(c.RandflakeSecret))
	g.randflakeKey = randflakeSecretKey[:16]
	rf, err := randflake.NewGenerator(
		g.lease.NodeID,
		g.lease.CreatedAt/int64(time.Second),
		(g.lease.ExpiresAt-_RANDFLAKE_SAFE_WINDOW)/int64(time.Second),
		g.randflakeKey,
	)
	if err != nil {
		log.Error().Err(err).Msg("failed to create randflake generator")
		return nil, err
	}
	g.randflake = rf

	// Start the randflake worker
	go g.randflakeWorker()

	is := &serverServiceProvider{
		PersistenceService: g.ps,
		s:                  g,
	}

	api.RegisterRoutes(g.mux, is)

	return g, nil
}

func (g *Server) randflakeWorker() {
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now().UnixNano()

			if now > g.lease.ExpiresAt {
				log.Error().Msg("randflake lease expired")
				lease, err := g.ps.RandflakeLeaseCreate(context.Background())
				if err != nil {
					log.Error().Err(err).Msg("failed to create randflake lease")
					continue
				}
				g.lease = lease
				rf, err := randflake.NewGenerator(
					g.lease.NodeID,
					g.lease.CreatedAt/int64(time.Second),
					(g.lease.ExpiresAt-_RANDFLAKE_SAFE_WINDOW)/int64(time.Second),
					g.randflakeKey,
				)
				if err != nil {
					log.Error().Err(err).Msg("failed to create randflake generator")
					return
				}
				g.randflake = rf
				log.Debug().Int64("expires_at", g.lease.ExpiresAt).Int64("nodeid", g.lease.NodeID).Msg("randflake lease created")
			}

			if now > g.lease.ExpiresAt-_RANDFLAKE_RENEW_WINDOW {
				log.Debug().Int64("now", now).Int64("expires_at", g.lease.ExpiresAt).Msg("randflake lease about to expire, trying to extend")
				func() {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
					defer cancel()
					lease, err := g.ps.RandflakeLeaseExtend(ctx, g.lease)
					if err != nil {
						log.Error().Err(err).Msg("failed to extend randflake lease")
						return
					}
					g.lease = lease
					g.randflake.UpdateLease(lease.CreatedAt/int64(time.Second), lease.ExpiresAt/int64(time.Second))
					log.Debug().Int64("expires_at", g.lease.ExpiresAt).Msg("randflake lease extended")
				}()
			}

			func() {
				gcStart := time.Now()
				log.Debug().Msg("running randflake gc")
				err := g.ps.RandflakeGC(context.Background())
				if err != nil {
					log.Error().Err(err).Msg("failed to run randflake gc")
				}
				log.Debug().Dur("duration", time.Since(gcStart)).Msg("randflake gc completed")
			}()
		case <-g.stopCh:
			return
		}
	}
}

type CORSServer struct {
	http.Handler
}

var (
	envOnce       sync.Once
	_IP_HEADER    string
	_HOST_HEADER  string
	_PROTO_HEADER string
)

func (s *CORSServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	envOnce.Do(func() {
		_IP_HEADER = os.Getenv("IP_HEADER")
		_HOST_HEADER = os.Getenv("HOST_HEADER")
		_PROTO_HEADER = os.Getenv("PROTO_HEADER")
	})

	if _IP_HEADER != "" {
		parts := strings.Split(r.Header.Get(_IP_HEADER), ", ")
		parts = strings.Split(parts[len(parts)-1], ",")
		r.RemoteAddr = strings.TrimSpace(parts[0])
	}

	if _HOST_HEADER != "" {
		parts := strings.Split(r.Header.Get(_HOST_HEADER), ", ")
		parts = strings.Split(parts[len(parts)-1], ",")
		r.URL.Host = strings.TrimSpace(parts[0])
	}

	if _PROTO_HEADER != "" {
		parts := strings.Split(r.Header.Get(_PROTO_HEADER), ", ")
		parts = strings.Split(parts[len(parts)-1], ",")
		r.URL.Scheme = strings.TrimSpace(parts[0])
	}

	origin := r.Header.Get("Origin")
	if origin != "" {
		// Echo origin and allow credentials for browsers
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		// Avoid caching responses for different origins incorrectly
		w.Header().Add("Vary", "Origin")
	} else {
		// No Origin header: be permissive
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}

	// Handle preflight requests by reflecting requested methods/headers
	if r.Method == http.MethodOptions {
		// Prefer Access-Control-Request-Method if provided
		if acrm := r.Header.Get("Access-Control-Request-Method"); acrm != "" {
			w.Header().Set("Access-Control-Allow-Methods", acrm)
		} else {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		}

		// Prefer Access-Control-Request-Headers if provided
		if acrh := r.Header.Get("Access-Control-Request-Headers"); acrh != "" {
			w.Header().Set("Access-Control-Allow-Headers", acrh)
		} else {
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}

		w.WriteHeader(http.StatusOK)
		return
	}

	// Non-preflight responses: include some CORS headers
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Max-Age", "86400")
	s.Handler.ServeHTTP(w, r)
}

func (g *Server) Serve(ln net.Listener) error {
	return http.Serve(ln, &CORSServer{Handler: g.mux})
}

func (g *Server) Shutdown() {
	// Shutdown logic can be implemented here
}
