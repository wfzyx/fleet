package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"fleet/pkg/config"
	"fleet/pkg/integrations"
	"fleet/pkg/budget"
)

// The wire protocol is JSON-RPC 2.0 over a Unix domain socket, one JSON
// message per line. Requests without an id are notifications and get no
// response. After events.subscribe the server also pushes notifications
// {"jsonrpc":"2.0","method":"event","params":<AgentEvent>} on that connection.

const (
	jsonRPCVersion = "2.0"
	// EventNotification is the method name of pushed event notifications.
	EventNotification = "event"

	// maxMessageBytes bounds one request line.
	maxMessageBytes = 4 << 20
	// maxSocketPath is the sun_path limit on Linux (108) minus the NUL byte.
	maxSocketPath = 107
)

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeNoMethod       = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeApplication    = -32000 // service errors; see RPCError.Kind
)

// Error kinds carried in RPCError.Kind.
const (
	KindNotFound    = "not_found"
	KindExists      = "exists"
	KindState       = "invalid_state"
	KindBusy        = "busy"
	KindBudget      = "budget_exceeded"
	KindInvalid     = "invalid_argument"
	KindUnavailable = "unavailable"
	KindCleanup     = "cleanup"
	KindOther       = "error"
)

// SocketPath returns $XDG_RUNTIME_DIR/fleet/fleet.sock for the given paths.
func SocketPath(p config.Paths) string { return filepath.Join(p.RuntimeDir, "fleet.sock") }

// ---------------------------------------------------------------- messages

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type rpcNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// RPCError is an error returned by the server. Clients can use errors.Is with
// the service sentinels (ErrNoSuchTask, ErrBusy, budget.ErrBudgetExceeded, ...).
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Kind is a coarse error class (not_found, invalid_state, ...).
	Kind string `json:"kind,omitempty"`
	// Sentinel names the precise service error, for example "no_such_task".
	Sentinel string `json:"sentinel,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// sentinels maps stable wire names to errors; order is the match precedence.
var sentinels = []struct {
	name string
	err  error
}{
	{"cleanup", ErrCleanup},
	{"no_such_space", ErrNoSuchSpace},
	{"space_exists", ErrSpaceExists},
	{"space_in_use", ErrSpaceInUse},
	{"no_such_task", ErrNoSuchTask},
	{"task_exists", ErrTaskExists},
	{"invalid_transition", ErrInvalidTransition},
	{"busy", ErrBusy},
	{"no_project", ErrNoProject},
	{"no_criteria", ErrNoCriteria},
	{"empty_diff", ErrEmptyDiff},
	{"not_approved", ErrNotApproved},
	{"stale_review", ErrStaleReview},
	{"no_pr", ErrNoPR},
	{"use_merge", ErrUseMerge},
	{"no_github", ErrNoGitHub},
	{"checks_failing", ErrChecksFailing},
	{"checks_pending", ErrChecksPending},
	{"conflicts", ErrConflicts},
	{"pr_closed", ErrPRClosed},
	{"shutdown", ErrShutdown},
	{"invalid_argument", ErrInvalidArgument},
	{"budget_exceeded", budget.ErrBudgetExceeded},
	{"not_found", integrations.ErrNotFound},
	{"not_mergeable", integrations.ErrNotMergeable},
}

func sentinelName(err error) string {
	for _, s := range sentinels {
		if errors.Is(err, s.err) {
			return s.name
		}
	}
	return ""
}

// Is lets errors.Is match the service sentinels across the wire.
func (e *RPCError) Is(target error) bool {
	for _, s := range sentinels {
		if s.name == e.Sentinel {
			return s.err == target
		}
	}
	return false
}

func errorKind(err error) string {
	switch {
	case errors.Is(err, ErrCleanup):
		return KindCleanup
	case errors.Is(err, ErrNoSuchTask), errors.Is(err, ErrNoSuchSpace), errors.Is(err, integrations.ErrNotFound):
		return KindNotFound
	case errors.Is(err, ErrTaskExists), errors.Is(err, ErrSpaceExists):
		return KindExists
	case errors.Is(err, budget.ErrBudgetExceeded):
		return KindBudget
	case errors.Is(err, ErrBusy), errors.Is(err, ErrSpaceInUse):
		return KindBusy
	case errors.Is(err, ErrNoGitHub), errors.Is(err, ErrShutdown):
		return KindUnavailable
	case errors.Is(err, ErrInvalidArgument), errors.Is(err, ErrNoProject), errors.Is(err, ErrNoCriteria):
		return KindInvalid
	case errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrNotApproved), errors.Is(err, ErrStaleReview),
		errors.Is(err, ErrNoPR), errors.Is(err, ErrUseMerge), errors.Is(err, ErrChecksFailing),
		errors.Is(err, ErrChecksPending), errors.Is(err, ErrConflicts), errors.Is(err, ErrPRClosed),
		errors.Is(err, ErrEmptyDiff):
		return KindState
	}
	return KindOther
}

// ---------------------------------------------------------------- params

// Parameter and result types shared by the server and Client.
type (
	// DelegateParams are the parameters of tasks.delegate.
	DelegateParams struct {
		Space    string   `json:"space"`
		Task     string   `json:"task"`
		Brief    string   `json:"brief"`
		Criteria []string `json:"criteria,omitempty"`
		Title    string   `json:"title,omitempty"`
		Project  string   `json:"project,omitempty"`
		JiraKey  string   `json:"jira_key,omitempty"`
	}
	// SpawnParams are the parameters of tasks.spawn.
	SpawnParams struct {
		Task   string `json:"task"`
		Branch string `json:"branch,omitempty"`
	}
	// ReportParams are the parameters of tasks.report.
	ReportParams struct {
		Task   string `json:"task"`
		Status string `json:"status"`
		Note   string `json:"note,omitempty"`
	}
	// TaskParams select one task (tasks.get, tasks.review, tasks.merge).
	TaskParams struct {
		Task string `json:"task"`
	}
	// CloseParams are the parameters of tasks.close.
	CloseParams struct {
		Task string `json:"task"`
		Note string `json:"note,omitempty"`
	}
	// AddSpaceParams are the parameters of spaces.add.
	AddSpaceParams struct {
		Space config.Space `json:"space"`
	}
	// AddProjectParams are the parameters of spaces.add_project.
	AddProjectParams struct {
		Space   string         `json:"space"`
		Project config.Project `json:"project"`
	}
	// RemoveSpaceParams are the parameters of spaces.remove.
	RemoveSpaceParams struct {
		ID    string `json:"id"`
		Force bool   `json:"force,omitempty"`
	}
	// SpaceBudgetParams are the parameters of spaces.budget.
	SpaceBudgetParams struct {
		Space string `json:"space"`
		BudgetUpdate
	}
	// FleetBudgetParams are the parameters of fleet.budget.
	FleetBudgetParams struct {
		MaxUSD float64 `json:"max_usd"`
	}
	// CostParams are the parameters of cost.
	CostParams struct {
		Space string `json:"space"`
		Task  string `json:"task,omitempty"`
	}
	// EventsParams are the parameters of events.list.
	EventsParams struct {
		Limit int `json:"limit,omitempty"`
	}
	// ChatParams are the parameters of leads.chat.
	ChatParams struct {
		Space   string `json:"space"`
		Message string `json:"message"`
	}
	// ChatResult is the result of leads.chat.
	ChatResult struct {
		Reply string `json:"reply"`
	}
	// HistoryParams are the parameters of leads.history.
	HistoryParams struct {
		Space string `json:"space"`
	}
	// DiffResult is the result of tasks.diff.
	DiffResult struct {
		Diff string `json:"diff"`
	}
	// Status is the result of status.
	Status struct {
		PID    int `json:"pid"`
		Spaces int `json:"spaces"`
		Tasks  int `json:"tasks"`
		Active int `json:"active"`
	}
)

// ---------------------------------------------------------------- server

// Server serves a Service on a Unix domain socket.
type Server struct {
	svc  *Service
	path string

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup

	methods map[string]methodFunc
}

type methodFunc func(ctx context.Context, c *serverConn, params json.RawMessage) (any, error)

// NewServer returns a server for svc listening on path. Use SocketPath for the
// default location.
func NewServer(svc *Service, path string) *Server {
	s := &Server{svc: svc, path: path, conns: map[net.Conn]struct{}{}}
	s.methods = s.buildMethods()
	return s
}

// Path returns the socket path.
func (s *Server) Path() string { return s.path }

// Listen binds the socket (directory 0700, socket 0600). A stale socket left
// by a crashed server is replaced; a live one yields an error.
func (s *Server) Listen() error {
	if len(s.path) > maxSocketPath {
		return fmt.Errorf("service: socket path too long (%d > %d): %s", len(s.path), maxSocketPath, s.path)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("service: create %s: %w", dir, err)
	}
	if fi, err := os.Lstat(s.path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("service: %s exists and is not a socket", s.path)
		}
		if c, derr := net.DialTimeout("unix", s.path, time.Second); derr == nil {
			_ = c.Close()
			return fmt.Errorf("service: another fleet server is listening on %s", s.path)
		}
		if err := os.Remove(s.path); err != nil {
			return fmt.Errorf("service: remove stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("service: listen %s: %w", s.path, err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("service: chmod socket: %w", err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	return nil
}

// Serve accepts connections until Close. It returns nil after a Close.
func (s *Server) Serve() error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		return errors.New("service: Serve called before Listen")
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("service: accept: %w", err)
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return nil
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.handleConn(conn)
	}
}

// Close stops accepting, disconnects clients, removes the socket and waits for
// handlers to return.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.ln
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	var err error
	if ln != nil {
		err = ln.Close() // also unlinks the socket file
	}
	s.wg.Wait()
	return err
}

type serverConn struct {
	conn net.Conn
	wmu  sync.Mutex
	ctx  context.Context

	subMu  sync.Mutex
	cancel func() // event subscription, nil when not subscribed
}

func (c *serverConn) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err = c.conn.Write(append(data, '\n'))
	return err
}

func (s *Server) handleConn(conn net.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &serverConn{conn: conn, ctx: ctx}
	var reqs sync.WaitGroup
	defer func() {
		cancel()
		c.subMu.Lock()
		if c.cancel != nil {
			c.cancel()
		}
		c.subMu.Unlock()
		_ = conn.Close()
		reqs.Wait()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		s.wg.Done()
	}()

	br := bufio.NewReaderSize(conn, 64<<10)
	for {
		line, err := readLine(br, maxMessageBytes)
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				_ = c.write(rpcResponse{JSONRPC: jsonRPCVersion, ID: json.RawMessage("null"),
					Error: &RPCError{Code: codeInvalidRequest, Message: "request too large"}})
			}
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		reqs.Add(1)
		go func() {
			defer reqs.Done()
			s.handleLine(ctx, c, line)
		}()
	}
}

var errLineTooLong = errors.New("line too long")

// readLine reads one LF-terminated line of at most max bytes. A final line
// without LF before EOF is returned together with io.EOF on the next call.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > max {
			return nil, errLineTooLong
		}
		switch {
		case err == nil:
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(buf) > 0:
			return buf, nil
		default:
			return nil, err
		}
	}
}

func (s *Server) handleLine(ctx context.Context, c *serverConn, line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		_ = c.write(rpcResponse{JSONRPC: jsonRPCVersion, ID: json.RawMessage("null"),
			Error: &RPCError{Code: codeParse, Message: "parse error: " + err.Error()}})
		return
	}
	notification := len(req.ID) == 0
	reply := func(result any, err error) {
		if notification {
			return
		}
		resp := rpcResponse{JSONRPC: jsonRPCVersion, ID: req.ID}
		if err != nil {
			resp.Error = toRPCError(err)
		} else if data, merr := json.Marshal(result); merr != nil {
			resp.Error = &RPCError{Code: codeInternal, Message: "encode result: " + merr.Error()}
		} else {
			resp.Result = data
		}
		_ = c.write(resp)
	}
	if req.JSONRPC != jsonRPCVersion || req.Method == "" {
		reply(nil, &RPCError{Code: codeInvalidRequest, Message: "invalid request: want jsonrpc 2.0 with a method"})
		return
	}
	m, ok := s.methods[req.Method]
	if !ok {
		reply(nil, &RPCError{Code: codeNoMethod, Message: "method not found: " + req.Method})
		return
	}
	reply(m(ctx, c, req.Params))
}

func toRPCError(err error) *RPCError {
	var re *RPCError
	if errors.As(err, &re) {
		return re
	}
	return &RPCError{Code: codeApplication, Message: err.Error(), Kind: errorKind(err), Sentinel: sentinelName(err)}
}

// decodeParams strictly decodes params into v; absent params decode as {}.
func decodeParams(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &RPCError{Code: codeInvalidParams, Message: "invalid params: " + err.Error(), Kind: KindInvalid}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return &RPCError{Code: codeInvalidParams, Message: "invalid params: trailing data", Kind: KindInvalid}
	}
	return nil
}

// method adapts a typed handler.
func method[P any](f func(ctx context.Context, p P) (any, error)) methodFunc {
	return func(ctx context.Context, _ *serverConn, raw json.RawMessage) (any, error) {
		var p P
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		return f(ctx, p)
	}
}

func (s *Server) buildMethods() map[string]methodFunc {
	svc := s.svc
	return map[string]methodFunc{
		"ping": func(context.Context, *serverConn, json.RawMessage) (any, error) {
			return map[string]bool{"pong": true}, nil
		},
		"status": func(context.Context, *serverConn, json.RawMessage) (any, error) {
			tasks := svc.ListTasks(TaskFilter{})
			st := Status{PID: os.Getpid(), Spaces: len(svc.ListSpaces()), Tasks: len(tasks)}
			for _, t := range tasks {
				if !t.Status.Terminal() {
					st.Active++
				}
			}
			return st, nil
		},
		"spaces.list": func(context.Context, *serverConn, json.RawMessage) (any, error) {
			return svc.ListSpaces(), nil
		},
		"spaces.add": method(func(_ context.Context, p AddSpaceParams) (any, error) {
			return struct{}{}, svc.AddSpace(p.Space)
		}),
		"spaces.add_project": method(func(_ context.Context, p AddProjectParams) (any, error) {
			return struct{}{}, svc.AddProject(p.Space, p.Project)
		}),
		"spaces.remove": method(func(ctx context.Context, p RemoveSpaceParams) (any, error) {
			return struct{}{}, svc.RemoveSpace(ctx, p.ID, p.Force)
		}),
		"spaces.budget": method(func(_ context.Context, p SpaceBudgetParams) (any, error) {
			return struct{}{}, svc.SetSpaceBudget(p.Space, p.BudgetUpdate)
		}),
		"fleet.budget": method(func(_ context.Context, p FleetBudgetParams) (any, error) {
			return struct{}{}, svc.SetFleetBudget(p.MaxUSD)
		}),
		"cost": method(func(_ context.Context, p CostParams) (any, error) {
			return svc.Cost(p.Space, p.Task)
		}),
		"tasks.list": method(func(_ context.Context, p TaskFilter) (any, error) {
			return svc.ListTasks(p), nil
		}),
		"tasks.get": method(func(_ context.Context, p TaskParams) (any, error) {
			return svc.GetTask(p.Task)
		}),
		"tasks.diff": method(func(ctx context.Context, p TaskParams) (any, error) {
			d, err := svc.Diff(ctx, p.Task)
			return DiffResult{Diff: d}, err
		}),
		"leads.status": func(context.Context, *serverConn, json.RawMessage) (any, error) {
			return svc.LeadStatuses(), nil
		},
		"leads.chat": method(func(ctx context.Context, p ChatParams) (any, error) {
			reply, err := svc.Chat(ctx, p.Space, p.Message)
			return ChatResult{Reply: reply}, err
		}),
		"leads.history": method(func(_ context.Context, p HistoryParams) (any, error) {
			return svc.ChatHistory(p.Space)
		}),
		"tasks.delegate": method(func(_ context.Context, p DelegateParams) (any, error) {
			var opts []DelegateOption
			if p.Title != "" {
				opts = append(opts, WithTitle(p.Title))
			}
			if p.Project != "" {
				opts = append(opts, WithProject(p.Project))
			}
			if p.JiraKey != "" {
				opts = append(opts, WithJiraKey(p.JiraKey))
			}
			return svc.Delegate(p.Space, p.Task, p.Brief, p.Criteria, opts...)
		}),
		"tasks.spawn": method(func(ctx context.Context, p SpawnParams) (any, error) {
			return svc.Spawn(ctx, p.Task, p.Branch)
		}),
		"tasks.report": method(func(ctx context.Context, p ReportParams) (any, error) {
			return svc.Report(ctx, p.Task, p.Status, p.Note)
		}),
		"tasks.review": method(func(ctx context.Context, p TaskParams) (any, error) {
			return svc.Review(ctx, p.Task)
		}),
		"tasks.merge": method(func(ctx context.Context, p TaskParams) (any, error) {
			return svc.Merge(ctx, p.Task)
		}),
		"tasks.close": method(func(ctx context.Context, p CloseParams) (any, error) {
			return svc.CloseTask(ctx, p.Task, p.Note)
		}),
		"events.list": method(func(_ context.Context, p EventsParams) (any, error) {
			return svc.Events(p.Limit)
		}),
		"events.subscribe": func(_ context.Context, c *serverConn, _ json.RawMessage) (any, error) {
			return map[string]bool{"subscribed": true}, s.subscribe(c)
		},
	}
}

// subscribe starts pushing events to the connection (once per connection).
func (s *Server) subscribe(c *serverConn) error {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if c.cancel != nil {
		return nil
	}
	ch, cancel := s.svc.Subscribe(256)
	c.cancel = cancel
	go func() {
		for {
			select {
			case e, ok := <-ch:
				if !ok {
					return
				}
				data, err := json.Marshal(e)
				if err != nil {
					continue
				}
				if c.write(rpcNotification{JSONRPC: jsonRPCVersion, Method: EventNotification, Params: data}) != nil {
					return
				}
			case <-c.ctx.Done():
				return
			}
		}
	}()
	return nil
}
