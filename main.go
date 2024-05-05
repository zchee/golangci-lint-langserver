package main

import (
	"context"
	"errors"
	"flag"
	"os"

	"go.lsp.dev/protocol"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	noLinterName = flag.Bool("nolintername", false, "don't show a linter name in message")
	// defaultSeverity = flag.String("severity", protocol.DiagnosticSeverityWarning.String(), "Default severity to use. Choices are: Err(or), Warn(ing), Info(rmation) or Hint")
)

func main() {
	l := zap.LevelFlag("loglevel", zapcore.DebugLevel, "output debug log")
	flag.Parse()

	logger, err := zap.NewDevelopment(zap.IncreaseLevel(l))
	if err != nil {
		panic(err)
	}
	// logger.Info("golangci-lint-langserver: connections opened")

	if err := run(context.Background(), logger); err != nil {
		logger.Fatal("run", zap.Error(err))
	}

	// logger.Info("golangci-lint-langserver: connections closed")
}

func run(ctx context.Context, logger *zap.Logger) (retErr error) {
	rwc := stdrwc{}
	conn := NewConn(rwc, rwc)
	defer func() {
		retErr = errors.Join(retErr, conn.Close())
	}()

	server := NewServer(ctx, conn, logger, *noLinterName)
	conn.Go(
		ctx,
		protocol.Handlers(protocol.ServerHandler(
			server,
			nil,
		)),
	)
	<-conn.Done()
	return nil
}

type stdrwc struct{}

func (stdrwc) Read(p []byte) (int, error) {
	return os.Stdin.Read(p)
}

func (stdrwc) Write(p []byte) (int, error) {
	return os.Stdout.Write(p)
}

func (stdrwc) Close() error {
	if err := os.Stdin.Close(); err != nil {
		return err
	}

	return os.Stdout.Close()
}
