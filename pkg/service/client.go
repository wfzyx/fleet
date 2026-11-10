package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"fleet/pkg/config"
	"fleet/pkg/schema"
)

// ErrClientClosed is returned by calls on a closed or disconnected Client.
var ErrClientClosed = errors.New("service: client closed")

// Client talks to a fleet Server over its Unix socket. It is safe for
// concurrent use; calls may be in flight simultaneously.
type Client struct {
	conn   net.Conn
	events chan schema.AgentEvent

	wmu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcResponse
	err     error // set once the connection is down
	done    chan struct{}
}

// Dial connects to the server socket at path.
func Dial(ctx context.Context, path string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("service: connect %s: %w (is the fleet server running?)", path, err)
	}
	c := &Client{
		conn: conn, events: make(chan schema.AgentEvent, 256),
		pending: map[int64]chan rpcResponse{}, done: make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// DialDefault connects to the socket under the XDG runtime directory.
func DialDefault(ctx context.Context) (*Client, error) {
	p, err := config.NewPaths()
	if err != nil {
		return nil, err
	}
	return Dial(ctx, SocketPath(p))
}

func (c *Client) readLoop() {
	br := bufio.NewReaderSize(c.conn, 64<<10)
	var rerr error
	for {
		line, err := readLine(br, maxMessageBytes)
		if err != nil {
			rerr = err
			break
		}
		var probe struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		if probe.Method != "" && len(probe.ID) == 0 {
			if probe.Method == EventNotification {
				var n rpcNotification
				var e schema.AgentEvent
				if json.Unmarshal(line, &n) == nil && json.Unmarshal(n.Params, &e) == nil {
					select {
					case c.events <- e:
					default: // consumer too slow; drop
					}
				}
			}
			continue
		}
		var resp rpcResponse
		if json.Unmarshal(line, &resp) != nil {
			continue
		}
		var id int64
		if json.Unmarshal(resp.ID, &id) != nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
	c.mu.Lock()
	c.err = ErrClientClosed
	if rerr != nil && !errors.Is(rerr, net.ErrClosed) && !errors.Is(rerr, io.EOF) {
		c.err = fmt.Errorf("%w: %v", ErrClientClosed, rerr)
	}
	pend := c.pending
	c.pending = map[int64]chan rpcResponse{}
	c.mu.Unlock()
	for _, ch := range pend {
		close(ch)
	}
	close(c.events)
	close(c.done)
}

// Events returns pushed event notifications (after Subscribe). The channel is
// closed when the connection ends.
func (c *Client) Events() <-chan schema.AgentEvent { return c.events }

// Done is closed when the connection has ended.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close disconnects.
func (c *Client) Close() error {
	err := c.conn.Close()
	<-c.done
	return err
}

// Call invokes method with params and decodes the result into result (which
// may be nil). Server errors are returned as *RPCError.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("service: encode params: %w", err)
	}
	if params == nil {
		raw = nil
	}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}

	idRaw, _ := json.Marshal(id)
	data, err := json.Marshal(rpcRequest{JSONRPC: jsonRPCVersion, ID: idRaw, Method: method, Params: raw})
	if err != nil {
		forget()
		return err
	}
	c.wmu.Lock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, werr := c.conn.Write(append(data, '\n'))
	c.wmu.Unlock()
	if werr != nil {
		forget()
		return fmt.Errorf("service: send %s: %w", method, werr)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.err
		}
		if resp.Error != nil {
			return resp.Error
		}
		if result == nil || len(resp.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("service: decode %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		forget()
		return ctx.Err()
	}
}

// Subscribe starts the event stream; read it with Events.
func (c *Client) Subscribe(ctx context.Context) error {
	return c.Call(ctx, "events.subscribe", nil, nil)
}

// Ping checks that the server answers.
func (c *Client) Ping(ctx context.Context) error { return c.Call(ctx, "ping", nil, nil) }

// Delegate is the typed form of tasks.delegate.
func (c *Client) Delegate(ctx context.Context, p DelegateParams) (schema.Task, error) {
	var t schema.Task
	return t, c.Call(ctx, "tasks.delegate", p, &t)
}

// Spawn is the typed form of tasks.spawn.
func (c *Client) Spawn(ctx context.Context, task, branch string) (schema.Task, error) {
	var t schema.Task
	return t, c.Call(ctx, "tasks.spawn", SpawnParams{Task: task, Branch: branch}, &t)
}

// Report is the typed form of tasks.report.
func (c *Client) Report(ctx context.Context, task, status, note string) (schema.Task, error) {
	var t schema.Task
	return t, c.Call(ctx, "tasks.report", ReportParams{Task: task, Status: status, Note: note}, &t)
}

// Review is the typed form of tasks.review.
func (c *Client) Review(ctx context.Context, task string) (ReviewResult, error) {
	var r ReviewResult
	return r, c.Call(ctx, "tasks.review", TaskParams{Task: task}, &r)
}

// Merge is the typed form of tasks.merge.
func (c *Client) Merge(ctx context.Context, task string) (schema.Task, error) {
	var t schema.Task
	return t, c.Call(ctx, "tasks.merge", TaskParams{Task: task}, &t)
}

// Tasks is the typed form of tasks.list.
func (c *Client) Tasks(ctx context.Context, f TaskFilter) ([]schema.Task, error) {
	var ts []schema.Task
	return ts, c.Call(ctx, "tasks.list", f, &ts)
}

// Spaces is the typed form of spaces.list.
func (c *Client) Spaces(ctx context.Context) ([]config.Space, error) {
	var sp []config.Space
	return sp, c.Call(ctx, "spaces.list", nil, &sp)
}

// GetTask is the typed form of tasks.get.
func (c *Client) GetTask(ctx context.Context, task string) (TaskRecord, error) {
	var r TaskRecord
	return r, c.Call(ctx, "tasks.get", TaskParams{Task: task}, &r)
}

// Diff is the typed form of tasks.diff.
func (c *Client) Diff(ctx context.Context, task string) (string, error) {
	var r DiffResult
	err := c.Call(ctx, "tasks.diff", TaskParams{Task: task}, &r)
	return r.Diff, err
}

// CloseTask is the typed form of tasks.close.
func (c *Client) CloseTask(ctx context.Context, task, note string) (schema.Task, error) {
	var t schema.Task
	return t, c.Call(ctx, "tasks.close", CloseParams{Task: task, Note: note}, &t)
}

// Chat is the typed form of leads.chat: it sends text to the space's lead and
// waits for the reply.
func (c *Client) Chat(ctx context.Context, space, text string) (string, error) {
	var r ChatResult
	err := c.Call(ctx, "leads.chat", ChatParams{Space: space, Message: text}, &r)
	return r.Reply, err
}

// ChatHistory is the typed form of leads.history: the persisted conversation
// with the space's lead, oldest first.
func (c *Client) ChatHistory(ctx context.Context, space string) ([]ChatMessage, error) {
	var msgs []ChatMessage
	err := c.Call(ctx, "leads.history", HistoryParams{Space: space}, &msgs)
	return msgs, err
}

// LeadStatuses is the typed form of leads.status.
func (c *Client) LeadStatuses(ctx context.Context) (map[string]string, error) {
	var m map[string]string
	return m, c.Call(ctx, "leads.status", nil, &m)
}

// Cost is the typed form of cost.
func (c *Client) Cost(ctx context.Context, space, task string) (CostReport, error) {
	var r CostReport
	return r, c.Call(ctx, "cost", CostParams{Space: space, Task: task}, &r)
}

// AddSpace is the typed form of spaces.add.
func (c *Client) AddSpace(ctx context.Context, sp config.Space) error {
	return c.Call(ctx, "spaces.add", AddSpaceParams{Space: sp}, nil)
}

// AddProject is the typed form of spaces.add_project.
func (c *Client) AddProject(ctx context.Context, space string, p config.Project) error {
	return c.Call(ctx, "spaces.add_project", AddProjectParams{Space: space, Project: p}, nil)
}

// RemoveSpace is the typed form of spaces.remove.
func (c *Client) RemoveSpace(ctx context.Context, id string, force bool) error {
	return c.Call(ctx, "spaces.remove", RemoveSpaceParams{ID: id, Force: force}, nil)
}

// SetSpaceBudget is the typed form of spaces.budget.
func (c *Client) SetSpaceBudget(ctx context.Context, space string, u BudgetUpdate) error {
	return c.Call(ctx, "spaces.budget", SpaceBudgetParams{Space: space, BudgetUpdate: u}, nil)
}

// SetFleetBudget is the typed form of fleet.budget.
func (c *Client) SetFleetBudget(ctx context.Context, maxUSD float64) error {
	return c.Call(ctx, "fleet.budget", FleetBudgetParams{MaxUSD: maxUSD}, nil)
}

// Status is the typed form of status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	return st, c.Call(ctx, "status", nil, &st)
}

// ListEvents is the typed form of events.list.
func (c *Client) ListEvents(ctx context.Context, limit int) ([]schema.AgentEvent, error) {
	var evs []schema.AgentEvent
	return evs, c.Call(ctx, "events.list", EventsParams{Limit: limit}, &evs)
}
