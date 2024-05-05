package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bytedance/sonic"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	pprotocol "go.lsp.dev/protocol/protocol"
	"go.lsp.dev/uri"
	"go.uber.org/zap"
)

func init() {
	pprotocol.RegiserMarshaler(sonic.ConfigFastest.Marshal)
	pprotocol.RegiserUnmarshaler(sonic.ConfigFastest.Unmarshal)
	pprotocol.RegiserEncoder(func(w io.Writer) pprotocol.JSONEncoder {
		enc := sonic.ConfigFastest.NewEncoder(w)
		return enc
	})
	pprotocol.RegiserDecoder(func(r io.Reader) pprotocol.JSONDecoder {
		dec := sonic.ConfigFastest.NewDecoder(r)
		return dec
	})
}

type handler struct {
	protocol.Server
	jsonrpc2.Conn

	logger       *zap.Logger
	noLinterName bool

	request chan pprotocol.DocumentURI
	command []string
	rootURI uri.URI
	rootDir uri.URI
}

func NewServer(ctx context.Context, conn jsonrpc2.Conn, logger *zap.Logger, noLinterName bool) protocol.Server {
	handler := &handler{
		Conn:         conn,
		logger:       logger,
		noLinterName: noLinterName,
		request:      make(chan pprotocol.DocumentURI),
	}
	go handler.linter(ctx)

	return handler
}

func (h *handler) errToDiagnostics(err error) []pprotocol.Diagnostic {
	var message string
	switch e := err.(type) {
	case *exec.ExitError:
		message = string(e.Stderr)
	default:
		h.logger.Debug("golangci-lint-langserver: errToDiagnostics message", zap.String("message", message))
		message = e.Error()
	}
	return []pprotocol.Diagnostic{
		{
			Severity: pprotocol.ErrorDiagnosticSeverity,
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

func (h *handler) SeverityFromString(severity string) pprotocol.DiagnosticSeverity {
	switch strings.ToLower(severity) {
	case "error":
		return pprotocol.ErrorDiagnosticSeverity
	case "warning":
		return pprotocol.WarningDiagnosticSeverity
	case "information":
		return pprotocol.InformationDiagnosticSeverity
	case "hint":
		return pprotocol.HintDiagnosticSeverity
	default:
		return pprotocol.WarningDiagnosticSeverity
	}
}

func (h *handler) lint(docURI pprotocol.DocumentURI) ([]pprotocol.Diagnostic, error) {
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

	diagnostics := make([]pprotocol.Diagnostic, 0, len(result.Issues))
	for _, issue := range result.Issues {
		issue := issue
		if file != issue.Pos.Filename {
			continue
		}

		d := pprotocol.Diagnostic{
			Range: pprotocol.Range{
				Start: pprotocol.Position{
					Line:      uint32(max(int(issue.Pos.Line-1), 0)),
					Character: uint32(max(int(issue.Pos.Column-1), 0)),
				},
				End: pprotocol.Position{
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
			pprotocol.MethodTextDocumentPublishDiagnostics,
			&pprotocol.PublishDiagnosticsParams{
				URI:         pprotocol.DocumentURI(u),
				Diagnostics: diagnostics,
			}); err != nil {
			h.logger.Fatal("notify", zap.Error(err))
		}
	}
}

func (h *handler) Initialize(_ context.Context, params *protocol.InitializeParams) (*protocol.InitializeResult, error) {
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

	textDocumentSync := pprotocol.NewServerCapabilitiesTextDocumentSync(pprotocol.TextDocumentSyncOptions{
		Change:    pprotocol.NoneTextDocumentSyncKind,
		OpenClose: true,
		Save: pprotocol.NewTextDocumentSyncOptionsSave(pprotocol.SaveOptions{
			IncludeText: true,
		}),
	})
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

func (h *handler) DidOpen(_ context.Context, params *protocol.DidOpenTextDocumentParams) (err error) {
	h.request <- pprotocol.DocumentURI(params.TextDocument.URI)
	return nil
}

func (h *handler) DidSave(_ context.Context, params *protocol.DidSaveTextDocumentParams) (err error) {
	h.request <- pprotocol.DocumentURI(params.TextDocument.URI)
	return nil
}

func (h *handler) WillSave(ctx context.Context, params *protocol.WillSaveTextDocumentParams) error {
	h.request <- pprotocol.DocumentURI(params.TextDocument.URI)
	return nil
}

func (*handler) CancelRequest(ctx context.Context, params *protocol.CancelParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) Progress(ctx context.Context, params *protocol.ProgressParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) SetTrace(ctx context.Context, params *protocol.SetTraceParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) Exit(ctx context.Context) error {
	return jsonrpc2.ErrInternal
}

func (*handler) Initialized(ctx context.Context, params *protocol.InitializedParams) error {
	return jsonrpc2.ErrInternal
}

// func (*handler) NotebookDocumentDidChange(ctx context.Context, params *protocol.DidChangeNotebookDocumentParams) error {
// 	return jsonrpc2.ErrInternal
// }
//
// func (*handler) NotebookDocumentDidClose(ctx context.Context, params *protocol.DidCloseNotebookDocumentParams) error {
// 	return jsonrpc2.ErrInternal
// }
//
// func (*handler) NotebookDocumentDidOpen(ctx context.Context, params *protocol.DidOpenNotebookDocumentParams) error {
// 	return jsonrpc2.ErrInternal
// }
//
// func (*handler) NotebookDocumentDidSave(ctx context.Context, params *protocol.DidSaveNotebookDocumentParams) error {
// 	return jsonrpc2.ErrInternal
// }

func (*handler) TextDocumentDidChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) TextDocumentDidClose(ctx context.Context, params *protocol.DidCloseTextDocumentParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) TextDocumentDidOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) TextDocumentDidSave(ctx context.Context, params *protocol.DidSaveTextDocumentParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) TextDocumentWillSave(ctx context.Context, params *protocol.WillSaveTextDocumentParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) WindowWorkDoneProgressCancel(ctx context.Context, params *protocol.WorkDoneProgressCancelParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) WorkspaceDidChangeConfiguration(ctx context.Context, params *protocol.DidChangeConfigurationParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) WorkspaceDidChangeWatchedFiles(ctx context.Context, params *protocol.DidChangeWatchedFilesParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) WorkspaceDidChangeWorkspaceFolders(ctx context.Context, params *protocol.DidChangeWorkspaceFoldersParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) WorkspaceDidCreateFiles(ctx context.Context, params *protocol.CreateFilesParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) WorkspaceDidDeleteFiles(ctx context.Context, params *protocol.DeleteFilesParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) WorkspaceDidRenameFiles(ctx context.Context, params *protocol.RenameFilesParams) error {
	return jsonrpc2.ErrInternal
}

func (*handler) CallHierarchyIncomingCalls(ctx context.Context, params *protocol.CallHierarchyIncomingCallsParams) ([]*protocol.CallHierarchyIncomingCall, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) CallHierarchyOutgoingCalls(ctx context.Context, params *protocol.CallHierarchyOutgoingCallsParams) ([]*protocol.CallHierarchyOutgoingCall, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) CodeActionResolve(ctx context.Context, params *protocol.CodeAction) (*protocol.CodeAction, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) CodeLensResolve(ctx context.Context, params *protocol.CodeLens) (*protocol.CodeLens, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) CompletionItemResolve(ctx context.Context, params *protocol.CompletionItem) (*protocol.CompletionItem, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) DocumentLinkResolve(ctx context.Context, params *protocol.DocumentLink) (*protocol.DocumentLink, error) {
	return nil, jsonrpc2.ErrInternal
}

// func (*handler) InlayHintResolve(ctx context.Context, params *protocol.InlayHint) (*protocol.InlayHint, error) {
// 	return nil, jsonrpc2.ErrInternal
// }

// func (*handler) TextDocumentCodeAction(ctx context.Context, params *protocol.CodeActionParams) (*protocol.TextDocumentCodeActionResult, error) {
// 	return nil, jsonrpc2.ErrInternal
// }

func (*handler) TextDocumentCodeLens(ctx context.Context, params *protocol.CodeLensParams) ([]*protocol.CodeLens, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) TextDocumentColorPresentation(ctx context.Context, params *protocol.ColorPresentationParams) ([]*protocol.ColorPresentation, error) {
	return nil, jsonrpc2.ErrInternal
}

// func (*handler) TextDocumentCompletion(ctx context.Context, params *protocol.CompletionParams) (*protocol.TextDocumentCompletionResult, error) {
// 	return nil, jsonrpc2.ErrInternal
// }
//
// func (*handler) TextDocumentDeclaration(ctx context.Context, params *protocol.DeclarationParams) (*protocol.TextDocumentDeclarationResult, error) {
// 	return nil, jsonrpc2.ErrInternal
// }
//
// func (*handler) TextDocumentDefinition(ctx context.Context, params *protocol.DefinitionParams) (*protocol.TextDocumentDefinitionResult, error) {
// 	return nil, jsonrpc2.ErrInternal
// }
//
// func (*handler) TextDocumentDiagnostic(ctx context.Context, params *protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
// 	return nil, jsonrpc2.ErrInternal
// }

func (*handler) TextDocumentDocumentColor(ctx context.Context, params *protocol.DocumentColorParams) ([]*protocol.ColorInformation, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) TextDocumentDocumentHighlight(ctx context.Context, params *protocol.DocumentHighlightParams) ([]*protocol.DocumentHighlight, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) TextDocumentDocumentLink(ctx context.Context, params *protocol.DocumentLinkParams) ([]*protocol.DocumentLink, error) {
	return nil, jsonrpc2.ErrInternal
}

// func (*handler) TextDocumentDocumentSymbol(ctx context.Context, params *protocol.DocumentSymbolParams) (*protocol.TextDocumentDocumentSymbolResult, error) {
// 	return nil, jsonrpc2.ErrInternal
// }

func (*handler) TextDocumentFoldingRange(ctx context.Context, params *protocol.FoldingRangeParams) ([]*protocol.FoldingRange, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) TextDocumentFormatting(ctx context.Context, params *protocol.DocumentFormattingParams) ([]*protocol.TextEdit, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) TextDocumentHover(ctx context.Context, params *protocol.HoverParams) (*protocol.Hover, error) {
	return nil, jsonrpc2.ErrInternal
}

// func (*handler) TextDocumentImplementation(ctx context.Context, params *protocol.ImplementationParams) (*protocol.TextDocumentImplementationResult, error) {
// 	return nil, jsonrpc2.ErrInternal
// }
//
// func (*handler) TextDocumentInlayHint(ctx context.Context, params *protocol.InlayHintParams) ([]*protocol.InlayHint, error) {
// 	return nil, jsonrpc2.ErrInternal
// }
//
// func (*handler) TextDocumentInlineCompletion(ctx context.Context, params *protocol.InlineCompletionParams) (*protocol.TextDocumentInlineCompletionResult, error) {
// 	return nil, jsonrpc2.ErrInternal
// }
//
// func (*handler) TextDocumentInlineValue(ctx context.Context, params *protocol.InlineValueParams) ([]*protocol.InlineValue, error) {
// 	return nil, jsonrpc2.ErrInternal
// }

func (*handler) TextDocumentLinkedEditingRange(ctx context.Context, params *protocol.LinkedEditingRangeParams) (*protocol.LinkedEditingRanges, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) TextDocumentMoniker(ctx context.Context, params *protocol.MonikerParams) ([]*protocol.Moniker, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) TextDocumentOnTypeFormatting(ctx context.Context, params *protocol.DocumentOnTypeFormattingParams) ([]*protocol.TextEdit, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) TextDocumentPrepareCallHierarchy(ctx context.Context, params *protocol.CallHierarchyPrepareParams) ([]*protocol.CallHierarchyItem, error) {
	return nil, jsonrpc2.ErrInternal
}

// func (*handler) TextDocumentPrepareRename(ctx context.Context, params *protocol.PrepareRenameParams) (*protocol.PrepareRenameResult, error) {
// 	return nil, jsonrpc2.ErrInternal
// }
//
// func (*handler) TextDocumentPrepareTypeHierarchy(ctx context.Context, params *protocol.TypeHierarchyPrepareParams) ([]*protocol.TypeHierarchyItem, error) {
// 	return nil, jsonrpc2.ErrInternal
// }

func (*handler) TextDocumentRangeFormatting(ctx context.Context, params *protocol.DocumentRangeFormattingParams) ([]*protocol.TextEdit, error) {
	return nil, jsonrpc2.ErrInternal
}

// func (*handler) TextDocumentRangesFormatting(ctx context.Context, params *protocol.DocumentRangesFormattingParams) ([]*protocol.TextEdit, error) {
// 	return nil, jsonrpc2.ErrInternal
// }

func (*handler) TextDocumentReferences(ctx context.Context, params *protocol.ReferenceParams) ([]*protocol.Location, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) TextDocumentRename(ctx context.Context, params *protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) TextDocumentSelectionRange(ctx context.Context, params *protocol.SelectionRangeParams) ([]*protocol.SelectionRange, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) TextDocumentSemanticTokensFull(ctx context.Context, params *protocol.SemanticTokensParams) (*protocol.SemanticTokens, error) {
	return nil, jsonrpc2.ErrInternal
}

// func (*handler) TextDocumentSemanticTokensFullDelta(ctx context.Context, params *protocol.SemanticTokensDeltaParams) (*protocol.TextDocumentSemanticTokensFullDeltaResult, error) {
// 	return nil, jsonrpc2.ErrInternal
// }

func (*handler) TextDocumentSemanticTokensRange(ctx context.Context, params *protocol.SemanticTokensRangeParams) (*protocol.SemanticTokens, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) TextDocumentSignatureHelp(ctx context.Context, params *protocol.SignatureHelpParams) (*protocol.SignatureHelp, error) {
	return nil, jsonrpc2.ErrInternal
}

// func (*handler) TextDocumentTypeDefinition(ctx context.Context, params *protocol.TypeDefinitionParams) (*protocol.TextDocumentTypeDefinitionResult, error) {
// 	return nil, jsonrpc2.ErrInternal
// }

func (*handler) TextDocumentWillSaveWaitUntil(ctx context.Context, params *protocol.WillSaveTextDocumentParams) ([]*protocol.TextEdit, error) {
	return nil, jsonrpc2.ErrInternal
}

// func (*handler) TypeHierarchySubtypes(ctx context.Context, params *protocol.TypeHierarchySubtypesParams) ([]*protocol.TypeHierarchyItem, error) {
// 	return nil, jsonrpc2.ErrInternal
// }
//
// func (*handler) TypeHierarchySupertypes(ctx context.Context, params *protocol.TypeHierarchySupertypesParams) ([]*protocol.TypeHierarchyItem, error) {
// 	return nil, jsonrpc2.ErrInternal
// }
//
// func (*handler) WorkspaceDiagnostic(ctx context.Context, params *protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
// 	return nil, jsonrpc2.ErrInternal
// }

func (*handler) WorkspaceExecuteCommand(ctx context.Context, params *protocol.ExecuteCommandParams) (any, error) {
	return nil, jsonrpc2.ErrInternal
}

// func (*handler) WorkspaceSymbol(ctx context.Context, params *protocol.WorkspaceSymbolParams) (*protocol.WorkspaceSymbolResult, error) {
// 	return nil, jsonrpc2.ErrInternal
// }

func (*handler) WorkspaceWillCreateFiles(ctx context.Context, params *protocol.CreateFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) WorkspaceWillDeleteFiles(ctx context.Context, params *protocol.DeleteFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, jsonrpc2.ErrInternal
}

func (*handler) WorkspaceWillRenameFiles(ctx context.Context, params *protocol.RenameFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, jsonrpc2.ErrInternal
}

// func (*handler) WorkspaceSymbolResolve(ctx context.Context, params *protocol.WorkspaceSymbol) (*protocol.WorkspaceSymbol, error) {
// 	return nil, jsonrpc2.ErrInternal
// }

func (h *handler) Request(ctx context.Context, method string, params interface{}) (interface{}, error) {
	return nil, errors.New("unimplemented")
}
