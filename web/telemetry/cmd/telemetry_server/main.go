package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.eu.org/envloader"
	"telemetry.gosuda.org/telemetry/internal/persistence"
	"telemetry.gosuda.org/telemetry/internal/server"
)

var pid = os.Getpid()

func init() {
	go func() {
		for {
			ppid := os.Getppid()
			if ppid == 1 {
				// send sigterm to self
				cmd := exec.Command("kill", "-TERM", strconv.Itoa(pid))
				cmd.Run()
				time.Sleep(time.Second * 60)
				os.Exit(1)
			}
			time.Sleep(time.Second * 1)
		}
	}()
}

func main() {
	envloader.LoadEnvFile(".env")
	configProvider := func(name string) (string, error) {
		if value, ok := os.LookupEnv(name); ok {
			return value, nil
		}
		return "", fmt.Errorf("environment variable %s not found", name)
	}

	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	zerolog.SetGlobalLevel(zerolog.DebugLevel)

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to listen on port")
	}
	defer ln.Close()
	fmt.Println("{\"port\":", ln.Addr().(*net.TCPAddr).Port, "}")

	log.Info().Msgf("Server starting on port %d", ln.Addr().(*net.TCPAddr).Port)

	dbconfig := &persistence.PersistenceClientConfig{
		DSN:             "root@localhost/database",
		ConnMaxIdleTime: time.Minute * 4,
		ConnMaxLifetime: 0,
		MaxIdleConns:    5,
		MaxOpenConns:    0,
	}
	err = envloader.BindStruct(dbconfig, configProvider)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to bind database config")
	}

	ps, err := persistence.NewPersistenceClient(context.Background(), dbconfig)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to create persistence client")
	}

	srvConfig := &server.ServerConfig{
		PersistenceService: ps,
	}
	err = envloader.BindStruct(srvConfig, configProvider)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to bind server config")
	}

	server, err := server.NewServer(
		srvConfig,
	)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to create server")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		err := server.Serve(ln)
		if err != nil && err != http.ErrServerClosed {
			log.Error().Err(err).Msg("Server failed")
		}
	}()

	<-sigCh
	log.Info().Msg("Received shutdown signal, shutting down server...")
	server.Shutdown()
	log.Info().Msg("Server shutdown complete")
}
