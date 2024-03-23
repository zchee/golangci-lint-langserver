package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bytedance/sonic"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.uber.org/zap"
)

type handler struct {
	protocol.Server
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
			Severity: protocol.DiagnosticSeverityError,
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
		return protocol.DiagnosticSeverityError
	case "warning":
		return protocol.DiagnosticSeverityWarning
	case "information":
		return protocol.DiagnosticSeverityInformation
	case "hint":
		return protocol.DiagnosticSeverityHint
	default:
		return protocol.DiagnosticSeverityWarning
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
	h.logger.Debug("golangci-lint-langserver: golingci-lint cmd", zap.Any("cmd", cmd))

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

	h.logger.Debug("golangci-lint-langserver: golingci-lint", zap.Any("result", result))

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
		h.logger.Info("linters", zap.Any("diagnostics", diagnostics))

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

func (h *handler) Initialize(ctx context.Context, params *protocol.InitializeParams) (*protocol.InitializeResult, error) {
	h.rootURI = uri.New(params.WorkspaceFolders[0].URI)
	h.rootDir = uri.New(params.WorkspaceFolders[0].URI)
	initOptions := params.InitializationOptions.(map[string]any)
	command, ok := initOptions["command"].([]string)
	if ok {
		h.command = command
	}
	if h.command == nil {
		h.command = []string{"golangci-lint", "run", "--out-format", "json"}
	}

	return &protocol.InitializeResult{
		Capabilities: protocol.ServerCapabilities{
			TextDocumentSync: protocol.TextDocumentSyncOptions{
				Change:    protocol.TextDocumentSyncKindNone,
				OpenClose: true,
				Save: &protocol.SaveOptions{
					IncludeText: true,
				},
			},
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

func (h *handler) DidOpen(_ context.Context, params *protocol.DidOpenTextDocumentParams) (err error) {
	h.request <- params.TextDocument.URI
	return nil
}

func (h *handler) DidSave(_ context.Context, params *protocol.DidSaveTextDocumentParams) (err error) {
	h.request <- params.TextDocument.URI
	return nil
}

func (h *handler) WillSave(ctx context.Context, params *protocol.WillSaveTextDocumentParams) error {
	h.request <- params.TextDocument.URI
	return nil
}

func (h *handler) Initialized(ctx context.Context, params *protocol.InitializedParams) error {
	return nil
}

func (h *handler) Exit(ctx context.Context) error {
	return nil
}

func (h *handler) WorkDoneProgressCancel(ctx context.Context, params *protocol.WorkDoneProgressCancelParams) error {
	return errors.New("unimplemented")
}

func (h *handler) LogTrace(ctx context.Context, params *protocol.LogTraceParams) error {
	return errors.New("unimplemented")
}

func (h *handler) SetTrace(ctx context.Context, params *protocol.SetTraceParams) error {
	return errors.New("unimplemented")
}

func (h *handler) CodeAction(ctx context.Context, params *protocol.CodeActionParams) ([]protocol.CodeAction, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) CodeLens(ctx context.Context, params *protocol.CodeLensParams) ([]protocol.CodeLens, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) CodeLensResolve(ctx context.Context, params *protocol.CodeLens) (*protocol.CodeLens, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) ColorPresentation(ctx context.Context, params *protocol.ColorPresentationParams) ([]protocol.ColorPresentation, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) Completion(ctx context.Context, params *protocol.CompletionParams) (*protocol.CompletionList, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) CompletionResolve(ctx context.Context, params *protocol.CompletionItem) (*protocol.CompletionItem, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) Declaration(ctx context.Context, params *protocol.DeclarationParams) ([]protocol.Location, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) DidChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error {
	return errors.New("unimplemented")
}

func (h *handler) DidChangeConfiguration(ctx context.Context, params *protocol.DidChangeConfigurationParams) error {
	return errors.New("unimplemented")
}

func (h *handler) DidChangeWatchedFiles(ctx context.Context, params *protocol.DidChangeWatchedFilesParams) error {
	return errors.New("unimplemented")
}

func (h *handler) DidChangeWorkspaceFolders(ctx context.Context, params *protocol.DidChangeWorkspaceFoldersParams) error {
	return errors.New("unimplemented")
}

func (h *handler) DidClose(ctx context.Context, params *protocol.DidCloseTextDocumentParams) error {
	return errors.New("unimplemented")
}

func (h *handler) DocumentColor(ctx context.Context, params *protocol.DocumentColorParams) ([]protocol.ColorInformation, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) DocumentHighlight(ctx context.Context, params *protocol.DocumentHighlightParams) ([]protocol.DocumentHighlight, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) DocumentLink(ctx context.Context, params *protocol.DocumentLinkParams) ([]protocol.DocumentLink, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) DocumentLinkResolve(ctx context.Context, params *protocol.DocumentLink) (*protocol.DocumentLink, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) DocumentSymbol(ctx context.Context, params *protocol.DocumentSymbolParams) ([]interface{}, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) ExecuteCommand(ctx context.Context, params *protocol.ExecuteCommandParams) (interface{}, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) FoldingRanges(ctx context.Context, params *protocol.FoldingRangeParams) ([]protocol.FoldingRange, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) Formatting(ctx context.Context, params *protocol.DocumentFormattingParams) ([]protocol.TextEdit, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) Hover(ctx context.Context, params *protocol.HoverParams) (*protocol.Hover, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) Implementation(ctx context.Context, params *protocol.ImplementationParams) ([]protocol.Location, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) OnTypeFormatting(ctx context.Context, params *protocol.DocumentOnTypeFormattingParams) ([]protocol.TextEdit, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) PrepareRename(ctx context.Context, params *protocol.PrepareRenameParams) (*protocol.Range, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) RangeFormatting(ctx context.Context, params *protocol.DocumentRangeFormattingParams) ([]protocol.TextEdit, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) References(ctx context.Context, params *protocol.ReferenceParams) ([]protocol.Location, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) Rename(ctx context.Context, params *protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) SignatureHelp(ctx context.Context, params *protocol.SignatureHelpParams) (*protocol.SignatureHelp, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) Symbols(ctx context.Context, params *protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) TypeDefinition(ctx context.Context, params *protocol.TypeDefinitionParams) ([]protocol.Location, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) WillSaveWaitUntil(ctx context.Context, params *protocol.WillSaveTextDocumentParams) ([]protocol.TextEdit, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) ShowDocument(ctx context.Context, params *protocol.ShowDocumentParams) (*protocol.ShowDocumentResult, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) WillCreateFiles(ctx context.Context, params *protocol.CreateFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) DidCreateFiles(ctx context.Context, params *protocol.CreateFilesParams) error {
	return errors.New("unimplemented")
}

func (h *handler) WillRenameFiles(ctx context.Context, params *protocol.RenameFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) DidRenameFiles(ctx context.Context, params *protocol.RenameFilesParams) error {
	return errors.New("unimplemented")
}

func (h *handler) WillDeleteFiles(ctx context.Context, params *protocol.DeleteFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) DidDeleteFiles(ctx context.Context, params *protocol.DeleteFilesParams) error {
	return errors.New("unimplemented")
}

func (h *handler) CodeLensRefresh(ctx context.Context) error {
	return errors.New("unimplemented")
}

func (h *handler) PrepareCallHierarchy(ctx context.Context, params *protocol.CallHierarchyPrepareParams) ([]protocol.CallHierarchyItem, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) IncomingCalls(ctx context.Context, params *protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) OutgoingCalls(ctx context.Context, params *protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) SemanticTokensFull(ctx context.Context, params *protocol.SemanticTokensParams) (*protocol.SemanticTokens, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) SemanticTokensFullDelta(ctx context.Context, params *protocol.SemanticTokensDeltaParams) (interface{}, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) SemanticTokensRange(ctx context.Context, params *protocol.SemanticTokensRangeParams) (*protocol.SemanticTokens, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) SemanticTokensRefresh(ctx context.Context) error {
	return errors.New("unimplemented")
}

func (h *handler) LinkedEditingRange(ctx context.Context, params *protocol.LinkedEditingRangeParams) (*protocol.LinkedEditingRanges, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) Moniker(ctx context.Context, params *protocol.MonikerParams) ([]protocol.Moniker, error) {
	return nil, errors.New("unimplemented")
}

func (h *handler) Request(ctx context.Context, method string, params interface{}) (interface{}, error) {
	return nil, errors.New("unimplemented")
}
