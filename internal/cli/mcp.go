package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/castrowithcee/callbell-cli/internal/application"
	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/output"
	"github.com/castrowithcee/callbell-cli/internal/redact"
)

const (
	mcpProtocolVersion    = "2026-07-28"
	mcpRequestTimeout     = 30 * time.Second
	mcpCacheTTLMillis     = 60 * 60 * 1000
	maxMCPMessageBytes    = 1 << 20
	maxMCPInFlight        = 16
	mcpParseError         = -32700
	mcpInvalidRequest     = -32600
	mcpMethodNotFound     = -32601
	mcpInvalidParams      = -32602
	mcpInternalError      = -32603
	mcpUnsupportedVersion = -32022
)

var errMCPMessageTooLarge = errors.New("MCP message exceeds size limit")

func newMCPCommand(opts *Options, registry *capability.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve the fixed agent tools over MCP stdio",
		Args:  noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			server := newMCPServer(opts, registry, c.OutOrStdout(), c.ErrOrStderr())
			return server.serve(c.Context(), c.InOrStdin())
		},
	}
}

type mcpServer struct {
	opts     *Options
	registry *capability.Registry
	stdout   io.Writer
	stderr   io.Writer
	timeout  time.Duration

	coreMu   sync.Mutex
	outMu    sync.Mutex
	errMu    sync.Mutex
	mu       sync.Mutex
	pending  map[string]*mcpPending
	wg       sync.WaitGroup
	writeErr error
}

type mcpPending struct {
	cancel    context.CancelFunc
	cancelled bool
}

func newMCPServer(opts *Options, registry *capability.Registry, stdout, stderr io.Writer) *mcpServer {
	if opts.Redactor == nil {
		opts.Redactor = &redact.Redactor{}
	}
	return &mcpServer{
		opts: opts, registry: registry, stdout: stdout, stderr: stderr,
		timeout: mcpRequestTimeout, pending: make(map[string]*mcpPending),
	}
}

func (s *mcpServer) serve(ctx context.Context, input io.Reader) error {
	reader := bufio.NewReader(input)
	for {
		line, err := readMCPLine(reader)
		switch {
		case err == nil:
			s.handle(ctx, line)
		case errors.Is(err, io.EOF):
			s.wg.Wait()
			return s.outputError()
		case errors.Is(err, errMCPMessageTooLarge):
			s.writeResponse(mcpErrorResponse(nil, mcpParseError,
				fmt.Sprintf("MCP message exceeds %d bytes", maxMCPMessageBytes), nil))
		default:
			s.cancelAll()
			s.wg.Wait()
			return fmt.Errorf("read MCP stdin: %w", err)
		}
	}
}

func readMCPLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	tooLarge := false
	for {
		fragment, prefix, err := reader.ReadLine()
		if len(line)+len(fragment) > maxMCPMessageBytes {
			tooLarge = true
		} else if !tooLarge {
			line = append(line, fragment...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) && (len(line) > 0 || tooLarge) {
				if tooLarge {
					return nil, errMCPMessageTooLarge
				}
				return line, nil
			}
			return nil, err
		}
		if !prefix {
			if tooLarge {
				return nil, errMCPMessageTooLarge
			}
			return line, nil
		}
	}
}

type mcpMessage struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

func (s *mcpServer) handle(parent context.Context, line []byte) {
	message, code, text := decodeMCPMessage(line)
	if code != 0 {
		// JSON-RPC notifications never receive a response, including when their params are malformed.
		if len(message.ID) == 0 && message.Method != "" {
			return
		}
		s.writeResponse(mcpErrorResponse(message.ID, code, text, nil))
		return
	}
	if len(message.ID) == 0 {
		if message.Method == "notifications/cancelled" {
			s.cancel(message.Params)
		}
		return
	}
	if message.Method == "initialize" {
		s.writeResponse(mcpErrorResponse(message.ID, mcpUnsupportedVersion,
			"This server supports MCP 2026-07-28 per-request metadata", map[string]any{
				"supported": []string{mcpProtocolVersion}, "requested": legacyProtocolVersion(message.Params),
			}))
		return
	}

	requested, err := requestProtocolVersion(message.Params)
	if err != nil {
		s.writeResponse(mcpErrorResponse(message.ID, mcpInvalidParams, err.Error(), nil))
		return
	}
	if requested != mcpProtocolVersion {
		s.writeResponse(mcpErrorResponse(message.ID, mcpUnsupportedVersion,
			"Unsupported protocol version", map[string]any{
				"supported": []string{mcpProtocolVersion}, "requested": requested,
			}))
		return
	}

	switch message.Method {
	case "server/discover":
		if err := onlyParams(message.Params, "_meta"); err != nil {
			s.writeResponse(mcpErrorResponse(message.ID, mcpInvalidParams, err.Error(), nil))
			return
		}
		s.writeResponse(mcpResultResponse(message.ID, map[string]any{
			"resultType":        "complete",
			"supportedVersions": []string{mcpProtocolVersion},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"ttlMs":             mcpCacheTTLMillis,
			"cacheScope":        "public",
			"_meta":             mcpServerMeta(),
		}))
	case "tools/list":
		if err := onlyParams(message.Params, "_meta", "cursor"); err != nil {
			s.writeResponse(mcpErrorResponse(message.ID, mcpInvalidParams, err.Error(), nil))
			return
		}
		if cursorPresent(message.Params) {
			s.writeResponse(mcpErrorResponse(message.ID, mcpInvalidParams, "pagination cursor is not supported", nil))
			return
		}
		s.writeResponse(mcpResultResponse(message.ID, map[string]any{
			"resultType": "complete", "tools": mcpTools(),
			"ttlMs": mcpCacheTTLMillis, "cacheScope": "public", "_meta": mcpServerMeta(),
		}))
	case "tools/call":
		s.startToolCall(parent, message)
	default:
		s.writeResponse(mcpErrorResponse(message.ID, mcpMethodNotFound, "Method not found", nil))
	}
}

func decodeMCPMessage(line []byte) (mcpMessage, int, string) {
	var fields map[string]json.RawMessage
	if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &fields) != nil || fields == nil {
		return mcpMessage{}, mcpParseError, "Parse error: invalid JSON"
	}
	var rpcVersion, method string
	if json.Unmarshal(fields["jsonrpc"], &rpcVersion) != nil || rpcVersion != "2.0" ||
		json.Unmarshal(fields["method"], &method) != nil || method == "" {
		return mcpMessage{ID: validMCPID(fields["id"])}, mcpInvalidRequest, "Invalid Request"
	}
	id := fields["id"]
	if len(id) > 0 && len(validMCPID(id)) == 0 {
		return mcpMessage{}, mcpInvalidRequest, "Invalid Request"
	}
	params := fields["params"]
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(params, &object) != nil || object == nil {
		return mcpMessage{ID: id, Method: method}, mcpInvalidParams, "params must be an object"
	}
	return mcpMessage{ID: id, Method: method, Params: params}, 0, ""
}

func validMCPID(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return raw
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var number json.Number
	if decoder.Decode(&number) == nil {
		return raw
	}
	return nil
}

func requestProtocolVersion(raw json.RawMessage) (string, error) {
	var params map[string]json.RawMessage
	if json.Unmarshal(raw, &params) != nil {
		return "", errors.New("params must be an object")
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(params["_meta"], &meta) != nil || meta == nil {
		return "", errors.New("params._meta must be an object")
	}
	var protocol string
	if json.Unmarshal(meta["io.modelcontextprotocol/protocolVersion"], &protocol) != nil || protocol == "" {
		return "", errors.New("params._meta must declare the protocol version")
	}
	var capabilities map[string]json.RawMessage
	if json.Unmarshal(meta["io.modelcontextprotocol/clientCapabilities"], &capabilities) != nil || capabilities == nil {
		return "", errors.New("params._meta must declare client capabilities")
	}
	if info, ok := meta["io.modelcontextprotocol/clientInfo"]; ok {
		var client struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if json.Unmarshal(info, &client) != nil || client.Name == "" || client.Version == "" {
			return "", errors.New("params._meta client info is invalid")
		}
	}
	return protocol, nil
}

func legacyProtocolVersion(raw json.RawMessage) string {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(raw, &params)
	return params.ProtocolVersion
}

func onlyParams(raw json.RawMessage, allowed ...string) error {
	var params map[string]json.RawMessage
	if json.Unmarshal(raw, &params) != nil {
		return errors.New("params must be an object")
	}
	set := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		set[name] = true
	}
	for name := range params {
		if !set[name] {
			return fmt.Errorf("unknown parameter %q", name)
		}
	}
	return nil
}

func cursorPresent(raw json.RawMessage) bool {
	var params map[string]json.RawMessage
	_ = json.Unmarshal(raw, &params)
	cursor, ok := params["cursor"]
	return ok && len(bytes.TrimSpace(cursor)) > 0 && !bytes.Equal(bytes.TrimSpace(cursor), []byte(`""`))
}

func (s *mcpServer) startToolCall(parent context.Context, message mcpMessage) {
	var params struct {
		Name           string          `json:"name"`
		Arguments      json.RawMessage `json:"arguments"`
		InputResponses json.RawMessage `json:"inputResponses"`
		RequestState   string          `json:"requestState"`
		Meta           json.RawMessage `json:"_meta"`
	}
	decoder := json.NewDecoder(bytes.NewReader(message.Params))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&params) != nil || params.Name == "" || len(params.InputResponses) > 0 || params.RequestState != "" {
		s.writeResponse(mcpErrorResponse(message.ID, mcpInvalidParams, "invalid tool call parameters", nil))
		return
	}
	if !knownMCPTool(params.Name) {
		s.writeResponse(mcpErrorResponse(message.ID, mcpInvalidParams, "Unknown tool: "+params.Name, nil))
		return
	}
	if len(params.Arguments) == 0 {
		params.Arguments = json.RawMessage(`{}`)
	}
	var arguments map[string]json.RawMessage
	if json.Unmarshal(params.Arguments, &arguments) != nil || arguments == nil {
		s.writeResponse(mcpErrorResponse(message.ID, mcpInvalidParams, "tool arguments must be an object", nil))
		return
	}

	key := string(bytes.TrimSpace(message.ID))
	requestContext, cancel := context.WithTimeout(parent, s.timeout)
	pending := &mcpPending{cancel: cancel}
	s.mu.Lock()
	if _, exists := s.pending[key]; exists {
		s.mu.Unlock()
		cancel()
		s.writeResponse(mcpErrorResponse(message.ID, mcpInvalidRequest, "request ID is already in progress", nil))
		return
	}
	if len(s.pending) >= maxMCPInFlight {
		s.mu.Unlock()
		cancel()
		s.writeResponse(mcpErrorResponse(message.ID, mcpInternalError, "too many in-flight requests", nil))
		return
	}
	s.pending[key] = pending
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		defer cancel()
		result, audit, err := s.callTool(requestContext, params.Name, params.Arguments)
		s.writeAudit(audit)
		response := mcpResultResponse(message.ID, toolResult(result, err, requestContext, s.opts.Redactor))

		s.mu.Lock()
		if !pending.cancelled {
			s.writeResponse(response)
		}
		delete(s.pending, key)
		s.mu.Unlock()
	}()
}

func (s *mcpServer) callTool(ctx context.Context, name string, raw json.RawMessage) (any, []byte, error) {
	var audit bytes.Buffer
	s.coreMu.Lock()
	core, err := applicationCore(s.opts, s.registry, name == "callbell.invoke")
	s.coreMu.Unlock()
	if err != nil {
		return nil, nil, err
	}

	switch name {
	case "callbell.search":
		var request application.SearchRequest
		if err := decodeMCPArguments(raw, &request); err != nil {
			return nil, nil, err
		}
		response, err := core.Search(request)
		return response, nil, err
	case "callbell.describe":
		var request application.DescribeRequest
		if err := decodeMCPArguments(raw, &request); err != nil {
			return nil, nil, err
		}
		response, err := core.Describe(request)
		return response, nil, err
	case "callbell.invoke":
		var request application.InvokeRequest
		if err := decodeMCPArguments(raw, &request); err != nil {
			return nil, nil, err
		}
		core.SetAudit(&audit)
		response, err := core.Invoke(ctx, request)
		return response, audit.Bytes(), err
	default:
		return nil, nil, fmt.Errorf("unknown MCP tool")
	}
}

func decodeMCPArguments(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &application.InvalidRequestError{Message: "tool arguments do not satisfy the request contract"}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return &application.InvalidRequestError{Message: "tool arguments must contain exactly one object"}
	}
	return nil
}

func (s *mcpServer) cancel(raw json.RawMessage) {
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
		Reason    string          `json:"reason"`
		Meta      json.RawMessage `json:"_meta"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&params) != nil || len(validMCPID(params.RequestID)) == 0 {
		return
	}
	key := string(bytes.TrimSpace(params.RequestID))
	s.mu.Lock()
	if pending := s.pending[key]; pending != nil && !pending.cancelled {
		pending.cancelled = true
		pending.cancel()
	}
	s.mu.Unlock()
}

func (s *mcpServer) cancelAll() {
	s.mu.Lock()
	for _, pending := range s.pending {
		pending.cancelled = true
		pending.cancel()
	}
	s.mu.Unlock()
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func mcpTools() []mcpTool {
	return []mcpTool{
		{
			Name: "callbell.search", Description: "Search the configured operation catalog",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"provider":{"type":"string"},"connection":{"type":"string"},"effect":{"type":"string","enum":["read","create","update","delete","execute"]},"limit":{"type":"integer"}},"additionalProperties":false}`),
		},
		{
			Name: "callbell.describe", Description: "Describe one versioned operation contract",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"operation":{"type":"string"},"version":{"type":"integer"},"connection":{"type":"string"}},"required":["operation"],"additionalProperties":false}`),
		},
		{
			Name: "callbell.invoke", Description: "Invoke one operation through the Callbell application core",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"operation":{"type":"string"},"version":{"type":"integer"},"connection":{"type":"string"},"arguments":{"type":"object"},"confirm":{"type":"boolean"}},"required":["operation"],"additionalProperties":false}`),
		},
	}
}

func knownMCPTool(name string) bool {
	return name == "callbell.search" || name == "callbell.describe" || name == "callbell.invoke"
}

func mcpServerMeta() map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/serverInfo": map[string]string{"name": "callbell", "version": version},
	}
}

func toolResult(data any, err error, ctx context.Context, redactor interface{ Error(error) string }) map[string]any {
	result := map[string]any{"resultType": "complete", "_meta": mcpServerMeta()}
	if err != nil {
		code := codeFor(err)
		message := redactor.Error(err)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = output.CodeTimeout
			message = "request deadline exceeded"
		}
		result["content"] = []map[string]string{{"type": "text", "text": string(code) + ": " + message}}
		result["isError"] = true
		return result
	}
	encoded, marshalErr := json.Marshal(data)
	if marshalErr != nil {
		result["content"] = []map[string]string{{"type": "text", "text": "runtime: encode tool result"}}
		result["isError"] = true
		return result
	}
	result["content"] = []map[string]string{{"type": "text", "text": string(encoded)}}
	result["structuredContent"] = data
	result["isError"] = false
	return result
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpRPCError    `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func mcpResultResponse(id json.RawMessage, result any) mcpResponse {
	return mcpResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func mcpErrorResponse(id json.RawMessage, code int, message string, data any) mcpResponse {
	if len(id) == 0 {
		id = json.RawMessage(`null`)
	}
	return mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpRPCError{Code: code, Message: message, Data: data}}
}

func (s *mcpServer) writeResponse(response mcpResponse) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if s.writeErr == nil {
		s.writeErr = json.NewEncoder(s.stdout).Encode(response)
	}
}

func (s *mcpServer) outputError() error {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if s.writeErr != nil {
		return fmt.Errorf("write MCP stdout: %w", s.writeErr)
	}
	return nil
}

func (s *mcpServer) writeAudit(audit []byte) {
	if len(bytes.TrimSpace(audit)) == 0 {
		return
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	writeAudit(s.stderr, audit, s.opts.Redactor)
}
