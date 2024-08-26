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
	Value  string
	Entry  struct {
		Stream Stream  `json:"stream"`
		Values [][]any `json:"values"`
	}
	Entries struct {
		Streams []*Entry `json:"streams"`
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
		HttpClient        *http.Client
	}
	Client struct {
		addr   string
		config *ClientConfig
		in     chan *Entry
		notify chan bool
		wg     sync.WaitGroup
		mut    sync.Mutex
		drain  bool
	}
	WriterOption func(*ClientConfig)
	LogLevel     int
)

const (
	TRACE LogLevel = iota
	DEBUG
	INFO
	WARN
	ERROR
)

const (
	ERR_SEND_FAILED SendErr = "failed to send log entries"
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
		c.ChannelBufferSize = minBatchSize * 5
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

func WithHttpClient(client *http.Client) WriterOption {
	return func(c *ClientConfig) {
		c.HttpClient = client
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

func newEntry(stream Stream, value Value) *Entry {
	return &Entry{
		Stream: stream,
		Values: [][]any{{fmt.Sprintf("%d", time.Now().UnixNano()), value}},
	}
}

func NewClient(addr string, options ...WriterOption) *Client {
	config := new(ClientConfig)
	config.LogLevel = INFO
	config.MinBatchSize = 1
	config.ChannelBufferSize = config.MinBatchSize * 5
	config.HttpClient = http.DefaultClient
	config.HttpTimeout = 30 * time.Second
	config.HttpClient.Timeout = config.HttpTimeout
	config.Fallbacks = make([]Logger, 0)

	for _, option := range options {
		option(config)
	}

	c := &Client{
		config: config,
		addr:   addr,
		in:     make(chan *Entry, config.ChannelBufferSize),
		notify: make(chan bool, 100),
	}

	return c
}

func (c *Client) log(ctx context.Context, level LogLevel, s Stream, v Value) {
	if c.drain {
		return
	}
	if level < c.config.LogLevel {
		return
	}
	copy := copy(s)
	copy["level"] = level.String()
	e := newEntry(copy, v)

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

func (c *Client) Log(ctx context.Context, s Stream, v Value) {
	if c.drain {
		return
	}

	copy := copy(s)
	e := newEntry(copy, v)

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
	c.log(ctx, TRACE, s, v)
}

func (c *Client) Debug(ctx context.Context, s Stream, v Value) {
	c.log(ctx, DEBUG, s, v)
}

func (c *Client) Info(ctx context.Context, s Stream, v Value) {
	c.log(ctx, INFO, s, v)
}

func (c *Client) Warn(ctx context.Context, s Stream, v Value) {
	c.log(ctx, WARN, s, v)
}

func (c *Client) Error(ctx context.Context, s Stream, v Value) {
	c.log(ctx, ERROR, s, v)
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

	return fmt.Errorf("%w: %v", ERR_SEND_FAILED, errs)
}

func (c *Client) send(ctx context.Context, entries []*Entry) error {
	data, err := json.Marshal(Entries{Streams: entries})
	if err != nil {
		return fmt.Errorf("failed to marshal entries: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.addr, bytes.NewBuffer(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := c.config.HttpClient.Do(req)
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

	for {
		select {
		case <-ctx.Done():
			c.drain = true
			c.Flush(ctx)
			return
		case <-tick:
			c.Flush(ctx)
		case <-c.notify:
			c.Flush(ctx)
		}
	}
}

func (c *Client) Flush(ctx context.Context) error {
	c.mut.Lock()
	defer c.mut.Unlock()
	if len := len(c.in); len != 0 {
		buffer := make([]*Entry, len)
		for i := 0; i < len; i++ {
			buffer[i] = readOrReturn(c.in)
		}
		grouped, err := Group(buffer)
		if err != nil {
			return fmt.Errorf("error grouping entries: %v", err)
		}
		if err := c.Write(ctx, grouped); err != nil {
			return fmt.Errorf("failed to write entries: %v", err)
		}
	}
	return nil
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
	case TRACE:
		return "TRACE"
	case DEBUG:
		return "DEBUG"
	case INFO:
		return "INFO"
	case WARN:
		return "WARN"
	case ERROR:
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

func copy(m map[string]string) map[string]string {
	out := make(map[string]string)
	for key, value := range m {
		out[key] = value
	}
	return out
}
