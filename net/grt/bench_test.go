package grt

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func runPresetE2EBenchmark(b *testing.B, preset Preset, payloadSize int) {
	b.Helper()
	sock, err := ListenUDP(&net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		b.Fatal(err)
	}
	defer sock.Close()
	authority := sock.LocalAddr().String()
	appID := [16]byte{0xbe, 0xef}

	id, err := GenerateServerIdentity(authority, appID)
	if err != nil {
		b.Fatal(err)
	}

	scfg := NewServerConfig(id, appID, preset,
		WithServerName(authority),
		WithMaxLiveSent(65536),
		WithNowUnix(func() int64 { return 1700000000 }),
	)
	l := ListenOver(sock, scfg)
	defer l.Close()

	ccfg := NewClientConfig(authority, appID, preset,
		WithExpectedKey(id.IdentityPub),
		WithMaxLiveSent(65536),
		WithNowUnix(func() int64 { return 1700000000 }),
	)

	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte(i)
	}

	b.SetBytes(int64(payloadSize))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		cli, err := Dial(ctx, l.Addr().String(), ccfg)
		if err != nil {
			cancel()
			b.Fatal(err)
		}

		srv, err := l.Accept(ctx)
		if err != nil {
			cli.Close()
			cancel()
			b.Fatal(err)
		}

		if err := cli.WaitConfirmed(ctx); err != nil {
			cli.Close()
			srv.Close()
			cancel()
			b.Fatal(err)
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			s, err := srv.AcceptStream(ctx)
			if err != nil {
				return
			}
			data, _ := io.ReadAll(s)
			s.Write(data)
			s.Close()
		}()

		s, err := cli.OpenStreamSync(ctx)
		if err != nil {
			cli.Close()
			srv.Close()
			cancel()
			b.Fatal(err)
		}
		if _, err := s.Write(payload); err != nil {
			cli.Close()
			srv.Close()
			cancel()
			b.Fatal(err)
		}
		if err := s.Close(); err != nil {
			cli.Close()
			srv.Close()
			cancel()
			b.Fatal(err)
		}

		echo, err := io.ReadAll(s)
		if err != nil {
			cli.Close()
			srv.Close()
			cancel()
			b.Fatal(err)
		}
		if len(echo) != len(payload) {
			b.Fatalf("echoed len %d, want %d", len(echo), len(payload))
		}
		<-done

		cli.Close()
		srv.Close()
		cancel()
	}
}

func runPresetStreamingBenchmark(b *testing.B, preset Preset, chunkSize, totalChunks int) {
	b.Helper()
	sock, err := ListenUDP(&net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		b.Fatal(err)
	}
	defer sock.Close()
	authority := sock.LocalAddr().String()
	appID := [16]byte{0xbe, 0xef}

	id, err := GenerateServerIdentity(authority, appID)
	if err != nil {
		b.Fatal(err)
	}

	scfg := NewServerConfig(id, appID, preset,
		WithServerName(authority),
		WithMaxLiveSent(65536),
		WithNowUnix(func() int64 { return 1700000000 }),
	)
	l := ListenOver(sock, scfg)
	defer l.Close()

	ccfg := NewClientConfig(authority, appID, preset,
		WithExpectedKey(id.IdentityPub),
		WithMaxLiveSent(65536),
		WithNowUnix(func() int64 { return 1700000000 }),
	)

	chunk := make([]byte, chunkSize)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	totalBytes := int64(chunkSize * totalChunks)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cli, err := Dial(ctx, l.Addr().String(), ccfg)
	if err != nil {
		b.Fatal(err)
	}
	defer cli.Close()

	srv, err := l.Accept(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer srv.Close()

	if err := cli.WaitConfirmed(ctx); err != nil {
		b.Fatal(err)
	}

	b.SetBytes(totalBytes)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		readDone := make(chan error, 1)
		go func() {
			s, err := srv.AcceptStream(ctx)
			if err != nil {
				readDone <- err
				return
			}
			buf := make([]byte, 64*1024)
			var total int64
			for {
				n, err := s.Read(buf)
				total += int64(n)
				if err != nil {
					if err == io.EOF {
						break
					}
					readDone <- err
					return
				}
			}
			if total != totalBytes {
				readDone <- io.ErrUnexpectedEOF
				return
			}
			readDone <- s.Close()
		}()

		s, err := cli.OpenStreamSync(ctx)
		if err != nil {
			b.Fatal(err)
		}
		for c := 0; c < totalChunks; c++ {
			if _, err := s.Write(chunk); err != nil {
				b.Fatal(err)
			}
		}
		if err := s.Close(); err != nil {
			b.Fatal(err)
		}

		if err := <-readDone; err != nil {
			b.Fatalf("server stream read error: %v", err)
		}
	}
}

// E2E Benchmarks (Handshake + Stream Lifecycle + Teardown)
func BenchmarkPresetE2E_LowResource(b *testing.B) {
	runPresetE2EBenchmark(b, PresetLowResource, 0)
}

func BenchmarkPresetE2E_Standard1RTT(b *testing.B) {
	runPresetE2EBenchmark(b, PresetStandard1RTT, 64*1024)
}

func BenchmarkPresetE2E_LowLatency(b *testing.B) {
	runPresetE2EBenchmark(b, PresetLowLatency, 64*1024)
}

func BenchmarkPresetE2E_HighThroughput(b *testing.B) {
	runPresetE2EBenchmark(b, PresetHighThroughput, 128*1024)
}

// Streaming Benchmarks (Continuous Bulk Streaming on Established Session)
func BenchmarkPresetStreaming_LowLatency(b *testing.B) {
	runPresetStreamingBenchmark(b, PresetLowLatency, 16*1024, 16) // 256 KB
}

func BenchmarkPresetStreaming_HighThroughput(b *testing.B) {
	runPresetStreamingBenchmark(b, PresetHighThroughput, 32*1024, 32) // 1 MB
}
