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
		MaxRetries        int
		RetryInterval     time.Duration
		MinBatchSize      int
		SyncInterval      time.Duration
		ChannelBufferSize int
		LogLevel          LogLevel
		HttpTimeout       time.Duration
		Fallbacks         []Logger
	}
	Client struct {
		addr       string
		config     ClientConfig
		httpClient *http.Client
		in         chan *Entry
		notify     chan bool
		wg         sync.WaitGroup
		mut        sync.Mutex
		closed     bool
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

func WithBatchSync(minBatchSize int, interval time.Duration) WriterOption {
	return func(c *ClientConfig) {
		c.MinBatchSize = minBatchSize
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

func WithFallback(loggers ...Logger) WriterOption {
	return func(c *ClientConfig) {
		c.Fallbacks = append(c.Fallbacks, loggers...)
	}
}

func WithHttpTimeout(timeout time.Duration) WriterOption {
	return func(c *ClientConfig) {
		c.HttpTimeout = timeout
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

func NewClient(addr string, options ...WriterOption) *Client {
	config := new(ClientConfig)
	config.LogLevel = InfoLevel
	config.MinBatchSize = 1
	config.ChannelBufferSize = config.MinBatchSize * 5
	config.HttpTimeout = 30 * time.Second
	config.Fallbacks = make([]Logger, 0)
	for _, option := range options {
		option(config)
	}

	c := &Client{
		addr: addr,
		httpClient: &http.Client{
			Timeout: config.HttpTimeout,
		},
		in:     make(chan *Entry, config.ChannelBufferSize),
		notify: make(chan bool, 100),
	}

	return c
}

func (c *Client) SetHttpClient(client *http.Client) {
	c.httpClient = client
}

func (c *Client) log(ctx context.Context, level LogLevel, s Stream, v Value) {
	if c.closed {
		return
	}
	if level < c.config.LogLevel {
		return
	}

	values := make(Value, 0, len(v)+2)
	values = append(values, time.Now().UnixNano(), level.String())
	values = append(values, v...)

	e := newEntry(s, values)

	select {
	case c.in <- e:
		if len(c.in) >= c.config.MinBatchSize {
			select {
			case c.notify <- true:
			default:
			}
		}
	case <-ctx.Done():
		log.Printf("Context cancelled while logging: %v", ctx.Err())
	}
}

func (c *Client) Trace(ctx context.Context, s Stream, v Value) {
	c.log(ctx, TraceLevel, s, v)
}

func (c *Client) Debug(ctx context.Context, s Stream, v Value) {
	c.log(ctx, DebugLevel, s, v)
}

func (c *Client) Info(ctx context.Context, s Stream, v Value) {
	c.log(ctx, InfoLevel, s, v)
}

func (c *Client) Warn(ctx context.Context, s Stream, v Value) {
	c.log(ctx, WarnLevel, s, v)
}

func (c *Client) Error(ctx context.Context, s Stream, v Value) {
	c.log(ctx, ErrorLevel, s, v)
}

func (c *Client) Write(ctx context.Context, entries []*Entry) error {
	var errs error
	for i := 0; i <= c.config.MaxRetries; i++ {
		if err := c.send(ctx, entries); err != nil {
			errs = errors.Join(errs, err)
			select {
			case <-time.After(c.config.RetryInterval):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}

	for _, fallback := range c.config.Fallbacks {
		if err := fallback.Write(ctx, entries); err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		return nil
	}

	return fmt.Errorf("%w: %v", ErrSendFailed, errs)
}

func (c *Client) send(ctx context.Context, entries []*Entry) error {
	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("failed to marshal entries: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.addr, bytes.NewBuffer(data))
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
	flush := func() {
		c.mut.Lock()
		defer c.mut.Unlock()
		if len := len(c.in); len != 0 {
			buffer := make([]*Entry, 0, len)
			for i := 0; i < len; i++ {
				buffer[i] = readOrReturn(c.in)
			}
			grouped, err := Group(buffer)
			if err != nil {
				log.Printf("Error grouping entries: %v", err)
			}
			if err := c.Write(ctx, grouped); err != nil {
				log.Printf("Failed to write entries: %v", err)
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			c.closed = true
			flush()
			return
		case <-tick:
			flush()
		case <-c.notify:
			flush()
		}
	}
}

func (c *Client) Flush(ctx context.Context) error {
	select {
	case c.notify <- true:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func Group(entries []*Entry) ([]*Entry, error) {
	out := make([]*Entry, 0)
	g := make(map[string]int)
	var errs error
	for _, entry := range entries {
		var buffer bytes.Buffer
		enc := gob.NewEncoder(&buffer)
		err := enc.Encode(entry.Stream)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("failed to encode entry: %w", err))
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
	return out, errs
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

func readOrReturn(in chan *Entry) *Entry {
	select {
	case out := <-in:
		return out
	default:
		return nil
	}
}
