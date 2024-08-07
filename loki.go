package lokiclient

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

type (
	SendErr string
	Logger  interface {
		Write(context.Context, []*Entry) error
	}
	Stream map[string]string
	Value  []any
	Entry  struct {
		Stream Stream  `json:"stream"`
		Values []Value `json:"values"`
	}
	ClientConfig struct {
		Address           string
		MaxRetries        int
		RetryInterval     time.Duration
		BatchSize         int
		SyncInterval      time.Duration
		ChannelBufferSize int
		LogLevel          LogLevel
	}
	Client struct {
		config     ClientConfig
		httpClient *http.Client
		in         chan *Entry
		notify     chan bool
		fallbacks  []Logger
		mu         sync.Mutex
		wg         sync.WaitGroup
	}
	WriterOption func(*ClientConfig)
	LogLevel     int
)

const (
	TraceLevel LogLevel = iota
	DebugLevel
	InfoLevel
	WarnLevel
	ErrorLevel
)

const (
	ErrSendFailed SendErr = "failed to send log entries"
)

func (e SendErr) Error() string {
	return string(e)
}

func init() {
	gob.Register(Stream{})
}

func WithRetry(max int, pause time.Duration) WriterOption {
	return func(c *ClientConfig) {
		c.MaxRetries = max
		c.RetryInterval = pause
	}
}

func WithBatchSync(batchSize int, interval time.Duration) WriterOption {
	return func(c *ClientConfig) {
		c.BatchSize = batchSize
		c.SyncInterval = interval
	}
}

func WithLogLevel(level LogLevel) WriterOption {
	return func(c *ClientConfig) {
		c.LogLevel = level
	}
}

func WithChannelBufferSize(size int) WriterOption {
	return func(c *ClientConfig) {
		c.ChannelBufferSize = size
	}
}

func NewStream(app, module, function, traceId string) Stream {
	return Stream{
		"app":     app,
		"mod":     module,
		"func":    function,
		"traceId": traceId,
	}
}

func NewStreamCustom(m map[string]string) Stream {
	return Stream(m)
}

func NewValue(params ...any) Value {
	return Value(params)
}

func newEntry(stream Stream, value Value) *Entry {
	return &Entry{
		Stream: stream,
		Values: []Value{value},
	}
}

func NewClient(config ClientConfig, options ...WriterOption) *Client {
	for _, option := range options {
		option(&config)
	}

	if config.BatchSize == 0 {
		config.BatchSize = 1
	}

	if config.ChannelBufferSize == 0 {
		config.ChannelBufferSize = config.BatchSize * 5
	}

	c := &Client{
		config:     config,
		httpClient: &http.Client{},
		in:         make(chan *Entry, config.ChannelBufferSize),
		notify:     make(chan bool, 100),
		fallbacks:  make([]Logger, 0),
	}

	return c
}

func (c *Client) AddFallback(l Logger) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fallbacks = append(c.fallbacks, l)
}

func (c *Client) SetHttpClient(client *http.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.httpClient = client
}

func (c *Client) log(ctx context.Context, level LogLevel, s Stream, v Value) error {
	if level < c.config.LogLevel {
		return nil
	}

	values := make(Value, 0, len(v)+2)
	values = append(values, time.Now().UnixNano(), level.String())
	values = append(values, v...)

	e := newEntry(s, values)

	select {
	case c.in <- e:
		if len(c.in) >= c.config.BatchSize {
			select {
			case c.notify <- true:
			default:
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) Trace(ctx context.Context, s Stream, v Value) error {
	return c.log(ctx, TraceLevel, s, v)
}

func (c *Client) Debug(ctx context.Context, s Stream, v Value) error {
	return c.log(ctx, DebugLevel, s, v)
}

func (c *Client) Info(ctx context.Context, s Stream, v Value) error {
	return c.log(ctx, InfoLevel, s, v)
}

func (c *Client) Warn(ctx context.Context, s Stream, v Value) error {
	return c.log(ctx, WarnLevel, s, v)
}

func (c *Client) Error(ctx context.Context, s Stream, v Value) error {
	return c.log(ctx, ErrorLevel, s, v)
}

func (c *Client) Write(ctx context.Context, entries []*Entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var errs error
	for i := 0; i <= c.config.MaxRetries; i++ {
		if err := c.send(ctx, entries); err != nil {
			errs = errors.Join(err)
			select {
			case <-time.After(c.config.RetryInterval):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}

	for _, fallback := range c.fallbacks {
		if err := fallback.Write(ctx, entries); err != nil {
			errs = errors.Join(err)
			continue
		}
		return nil
	}

	return errs
}

func (c *Client) send(ctx context.Context, entries []*Entry) error {
	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("failed to marshal entries: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.config.Address, bytes.NewBuffer(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode/100 != 2 {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("unexpected status code: %s, body: %s", res.Status, string(body))
	}

	return nil
}

func (c *Client) Sync(ctx context.Context) {
	c.wg.Add(1)
	go c.syncWorker(ctx)
}

func (c *Client) syncWorker(ctx context.Context) {
	defer c.wg.Done()

	tick := make(<-chan time.Time)
	if c.config.SyncInterval > 0 {
		ticker := time.NewTicker(c.config.SyncInterval)
		tick = ticker.C
		defer ticker.Stop()
	}

	flush := func(len int) {
		buffer := make([]*Entry, 0)
		for i := 0; i < len; i++ {
			if e := readOrReturn(c.in); e != nil {
				buffer = append(buffer, e)
			}
		}
		if err := c.Write(ctx, Group(buffer)); err != nil {
			log.Printf("Failed to write entries: %v", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			flush(len(c.in))
			return
		case <-tick:
			flush(c.config.BatchSize)
		case <-c.notify:
			flush(c.config.BatchSize)
		}
	}
}

func (c *Client) Flush(ctx context.Context) error {
	c.notify <- true
	return nil
}

func (c *Client) Close() error {
	close(c.in)
	c.wg.Wait()
	return nil
}

func Group(entries []*Entry) []*Entry {
	out := make([]*Entry, 0)
	g := make(map[string]int)
	for _, entry := range entries {
		var buffer bytes.Buffer
		enc := gob.NewEncoder(&buffer)
		err := enc.Encode(entry.Stream)
		if err != nil {
			continue
		}
		key := hex.EncodeToString(buffer.Bytes())
		n, ok := g[key]
		if !ok {
			g[key] = len(out)
			e := &Entry{
				Stream: entry.Stream,
				Values: entry.Values,
			}
			out = append(out, e)
			continue
		}
		out[n].Values = append(out[n].Values, entry.Values...)
	}
	return out
}

func (l LogLevel) String() string {
	switch l {
	case TraceLevel:
		return "TRACE"
	case DebugLevel:
		return "DEBUG"
	case InfoLevel:
		return "INFO"
	case WarnLevel:
		return "WARN"
	case ErrorLevel:
		return "ERROR"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", l)
	}
}

// Extra non-blocking measure
// Not really needed, I know
func readOrReturn(in chan *Entry) *Entry {
	select {
	case out := <-in:
		{
			return out
		}
	default:
		{
			return nil
		}
	}
}
