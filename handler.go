package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bytedance/sonic"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.uber.org/zap"
)

func init() {
	protocol.RegiserMarshaler(sonic.ConfigFastest.Marshal)
	protocol.RegiserUnmarshaler(sonic.ConfigFastest.Unmarshal)
	protocol.RegiserEncoder(func(w io.Writer) protocol.JSONEncoder {
		enc := sonic.ConfigFastest.NewEncoder(w)
		return enc
	})
	protocol.RegiserDecoder(func(r io.Reader) protocol.JSONDecoder {
		dec := sonic.ConfigFastest.NewDecoder(r)
		return dec
	})
}

type handler struct {
	protocol.UnimplementedServer
	jsonrpc2.Conn

	logger       *zap.Logger
	noLinterName bool

	request chan protocol.DocumentURI
	command []string
	rootURI uri.URI
	rootDir uri.URI
}

func NewServer(ctx context.Context, conn jsonrpc2.Conn, logger *zap.Logger, noLinterName bool) protocol.Server {
	handler := &handler{
		Conn:         conn,
		logger:       logger,
		noLinterName: noLinterName,
		request:      make(chan protocol.DocumentURI),
	}
	go handler.linter(ctx)

	return handler
}

func (h *handler) errToDiagnostics(err error) []protocol.Diagnostic {
	var message string
	switch e := err.(type) {
	case *exec.ExitError:
		message = string(e.Stderr)
	default:
		h.logger.Debug("golangci-lint-langserver: errToDiagnostics message", zap.String("message", message))
		message = e.Error()
	}
	return []protocol.Diagnostic{
		{
			Severity: protocol.ErrorDiagnosticSeverity,
			Message:  message,
		},
	}
}

type Issue struct {
	FromLinter  string   `json:"FromLinter"`
	Text        string   `json:"Text"`
	Severity    string   `json:"Severity"`
	SourceLines []string `json:"SourceLines"`
	Replacement any      `json:"Replacement"`
	LineRange   struct {
		From int `json:"From"`
		To   int `json:"To"`
	} `json:"LineRange,omitempty"`
	Pos struct {
		Filename string `json:"Filename"`
		Offset   int    `json:"Offset"`
		Line     int    `json:"Line"`
		Column   int    `json:"Column"`
	} `json:"Pos"`
	ExpectNoLint         bool   `json:"ExpectNoLint"`
	ExpectedNoLintLinter string `json:"ExpectedNoLintLinter"`
}

type Result struct {
	Issues []Issue `json:"Issues"`
	Report struct {
		Linters []struct {
			Name             string `json:"Name"`
			Enabled          bool   `json:"Enabled,omitempty"`
			EnabledByDefault bool   `json:"EnabledByDefault,omitempty"`
		} `json:"Linters"`
	} `json:"Report"`
}

func (h *handler) SeverityFromString(severity string) protocol.DiagnosticSeverity {
	switch strings.ToLower(severity) {
	case "error":
		return protocol.ErrorDiagnosticSeverity
	case "warning":
		return protocol.WarningDiagnosticSeverity
	case "information":
		return protocol.InformationDiagnosticSeverity
	case "hint":
		return protocol.HintDiagnosticSeverity
	default:
		return protocol.WarningDiagnosticSeverity
	}
}

func (h *handler) lint(docURI protocol.DocumentURI) ([]protocol.Diagnostic, error) {
	path := uri.New(string(docURI))
	dir, file := filepath.Split(path.Filename())

	args := make([]string, 0, len(h.command))
	args = append(args, h.command[1:]...)
	args = append(args, dir)
	//nolint:gosec
	cmd := exec.Command(h.command[0], args...)
	if strings.HasPrefix(path.Filename(), h.rootDir.Filename()) {
		cmd.Dir = h.rootDir.Filename()
		file = path.Filename()[len(h.rootDir.Filename())+1:]
	} else {
		cmd.Dir = dir
	}
	h.logger.Info("golangci-lint-langserver: golingci-lint cmd", zap.Any("cmd", cmd))

	b, err := cmd.Output()
	if len(b) == 0 {
		// golangci-lint would output critical error to stderr rather than stdout
		// https://github.com/nametake/golangci-lint-langserver/issues/24
		return h.errToDiagnostics(err), nil
	}

	data := bytes.Split(b, []byte("\n"))
	var result Result
	if err := sonic.ConfigFastest.Unmarshal(data[0], &result); err != nil {
		return h.errToDiagnostics(err), nil
	}

	h.logger.Info("golangci-lint-langserver: golingci-lint", zap.Any("result", result))

	diagnostics := make([]protocol.Diagnostic, 0, len(result.Issues))
	for _, issue := range result.Issues {
		issue := issue
		if file != issue.Pos.Filename {
			continue
		}

		d := protocol.Diagnostic{
			Range: protocol.Range{
				Start: protocol.Position{
					Line:      uint32(max(int(issue.Pos.Line-1), 0)),
					Character: uint32(max(int(issue.Pos.Column-1), 0)),
				},
				End: protocol.Position{
					Line:      uint32(max(int(issue.Pos.Line-1), 0)),
					Character: uint32(max(int(issue.Pos.Column-1), 0)),
				},
			},
			Severity: h.SeverityFromString(issue.Severity),
			Source:   issue.FromLinter,
			Message:  h.diagnosticMessage(&issue),
		}
		diagnostics = append(diagnostics, d)
	}

	return diagnostics, nil
}

func (h *handler) diagnosticMessage(issue *Issue) string {
	if h.noLinterName {
		return issue.Text
	}

	return fmt.Sprintf("%s: %s", issue.FromLinter, issue.Text)
}

func (h *handler) linter(ctx context.Context) {
	for {
		u, ok := <-h.request
		if !ok {
			break
		}

		diagnostics, err := h.lint(u)
		if err != nil {
			h.logger.Fatal("diagnostics", zap.Error(err))

			continue
		}
		h.logger.Debug("linter", zap.Any("diagnostics", diagnostics))

		if err := h.Conn.Notify(
			ctx,
			protocol.MethodTextDocumentPublishDiagnostics,
			&protocol.PublishDiagnosticsParams{
				URI:         u,
				Diagnostics: diagnostics,
			}); err != nil {
			h.logger.Fatal("notify", zap.Error(err))
		}
	}
}

func (h *handler) Initialize(_ context.Context, params *protocol.InitializeParams) (*protocol.InitializeResult, error) {
	h.rootURI = params.WorkspaceFolders[0].URI
	h.rootDir = params.WorkspaceFolders[0].URI
	initOptions := params.InitializationOptions.(map[string]any)
	command, ok := initOptions["command"].([]string)
	if ok {
		h.command = command
	}
	if h.command == nil {
		h.command = []string{"golangci-lint", "run", "--out-format", "json"}
	}

	syncOpts := protocol.TextDocumentSyncOptions{
		OpenClose: true,
		Change:    protocol.IncrementalTextDocumentSyncKind,
		Save: protocol.NewTextDocumentSyncOptionsSave(protocol.SaveOptions{
			IncludeText: true,
		}),
	}
	textDocumentSync := protocol.NewServerCapabilitiesTextDocumentSync(syncOpts)

	return &protocol.InitializeResult{
		Capabilities: protocol.ServerCapabilities{
			TextDocumentSync: textDocumentSync,
		},
		ServerInfo: &protocol.ServerInfo{
			Name: "golangci-lint-langserver",
		},
	}, nil
}

func (h *handler) Shutdown(context.Context) (err error) {
	close(h.request)
	return nil
}

func (h *handler) DidOpenTextDocument(_ context.Context, params *protocol.DidOpenTextDocumentParams) (err error) {
	h.request <- params.TextDocument.URI
	return nil
}

func (h *handler) DidSaveTextDocument(_ context.Context, params *protocol.DidSaveTextDocumentParams) (err error) {
	h.request <- params.TextDocument.URI
	return nil
}

func (h *handler) WillSaveTextDocument(ctx context.Context, params *protocol.WillSaveTextDocumentParams) error {
	h.request <- params.TextDocument.URI
	return nil
}

func (h *handler) DidChangeTextDocument(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error {
	h.request <- params.TextDocument.URI
	return nil
}
