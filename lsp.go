package main

import (
	"errors"
	"io"

	"go.lsp.dev/jsonrpc2"
)

// NewConn returns a new jsonrpc2.Conn backed by the given io.{Read,Write}Closer
// (which is usually os.Stdin and os.Stdout).
func NewConn(readCloser io.ReadCloser, writeCloser io.WriteCloser) jsonrpc2.Conn {
	return jsonrpc2.NewConn(
		jsonrpc2.NewStream(
			&readWriteCloser{
				readCloser:  readCloser,
				writeCloser: writeCloser,
			},
		),
	)
}

type readWriteCloser struct {
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
	return errors.Join(r.readCloser.Close(), r.writeCloser.Close())
}
