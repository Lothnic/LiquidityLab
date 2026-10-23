// liquiditylabd exposes bounded local replay and simulated paper services.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	"github.com/liquiditylab/liquiditylab/internal/replayrpc"
	"google.golang.org/grpc"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) > 0 && args[0] == "paper" {
		return paperServer(args[1:])
	}
	if len(args) == 0 || args[0] != "replay" {
		return fmt.Errorf("usage: liquiditylabd <replay|paper> [flags]; paper requires an isolated schema, new synthetic account and explicit balances")
	}
	flags := flag.NewFlagSet("replay", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:50051", "loopback TCP address or unix:/absolute/path")
	limits := replayrpc.DefaultLimits()
	flags.IntVar(&limits.MaxEnvironments, "max-environments", limits.MaxEnvironments, "maximum active environment handles")
	flags.IntVar(&limits.MaxEnvironmentsPerBatch, "max-batch-size", limits.MaxEnvironmentsPerBatch, "maximum environments per batch")
	flags.IntVar(&limits.MaxBatches, "max-batches", limits.MaxBatches, "maximum active batches")
	flags.IntVar(&limits.MaxMessageBytes, "max-message-bytes", limits.MaxMessageBytes, "maximum request or response protobuf bytes")
	flags.IntVar(&limits.MaxOperationsPerBatch, "max-operations-per-batch", limits.MaxOperationsPerBatch, "maximum retained mutation identities per batch; raising this increases memory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	service, err := replayrpc.New(limits)
	if err != nil {
		return err
	}
	defer service.Shutdown()
	listener, cleanup, err := localListener(*listen)
	if err != nil {
		return err
	}
	defer cleanup()
	server := grpc.NewServer(grpc.MaxRecvMsgSize(limits.MaxMessageBytes), grpc.MaxSendMsgSize(limits.MaxMessageBytes), grpc.MaxConcurrentStreams(uint32(limits.MaxConcurrentRPCs)), grpc.ConnectionTimeout(5*time.Second))
	pb.RegisterReplayServiceServer(server, service)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	log.Printf("offline replay ready address=%s engine_instance_id=%s", listener.Addr(), service.InstanceID())
	select {
	case err := <-serveErr:
		if errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	graceful := make(chan struct{})
	go func() { server.GracefulStop(); close(graceful) }()
	select {
	case <-graceful:
	case <-time.After(5 * time.Second):
		server.Stop()
		<-graceful
	}
	return nil
}

func localListener(address string) (net.Listener, func(), error) {
	if strings.HasPrefix(address, "unix:") {
		path := strings.TrimPrefix(address, "unix:")
		if !filepath.IsAbs(path) {
			return nil, nil, fmt.Errorf("Unix socket path must be absolute")
		}
		// Never unlink an existing socket: it may belong to a running trainer.
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("Unix socket path exists or cannot be inspected")
		}
		listener, err := net.Listen("unix", path)
		if err != nil {
			return nil, nil, err
		}
		if err := os.Chmod(path, 0600); err != nil {
			listener.Close()
			return nil, nil, err
		}
		return listener, func() { listener.Close() }, nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid loopback address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, nil, fmt.Errorf("replay TCP listener requires a literal loopback IP")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, err
	}
	return listener, func() { listener.Close() }, nil
}
