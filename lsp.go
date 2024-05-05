package main

import (
	"errors"
	"io"
	"os"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// NewConn returns a new jsonrpc2.Conn backed by the given io.{Read,Write}Closer
// (which is usually os.Stdin and os.Stdout).
func _NewConn(readCloser io.ReadCloser, writeCloser io.WriteCloser) jsonrpc2.Conn {
	return jsonrpc2.NewConn(
		jsonrpc2.NewStream(
			&readWriteCloser{
				readCloser:  readCloser,
				writeCloser: writeCloser,
			},
		),
	)
}

// NewConn returns a new jsonrpc2.Conn backed by the given io.{Read,Write}Closer
// (which is usually os.Stdin and os.Stdout).
func NewConn(readCloser io.ReadCloser, writeCloser io.WriteCloser) jsonrpc2.Conn {
	f, err := os.Create("/Users/zchee/.local/state/nvim/golangci.log")
	if err != nil {
		panic(err)
	}
	stream := jsonrpc2.NewStream(
		&readWriteCloser{
			f:           f,
			readCloser:  readCloser,
			writeCloser: writeCloser,
		},
	)
	return jsonrpc2.NewConn(
		protocol.LoggingStream(stream, f))
}

type readWriteCloser struct {
	f           *os.File
	readCloser  io.ReadCloser
	writeCloser io.WriteCloser
}

func (r *readWriteCloser) Read(b []byte) (int, error) {
	return r.readCloser.Read(b)
}

func (r *readWriteCloser) Write(b []byte) (int, error) {
	return r.writeCloser.Write(b)
}

func (r *readWriteCloser) Close() error {
	return errors.Join(r.readCloser.Close(), r.writeCloser.Close(), r.f.Close())
}
