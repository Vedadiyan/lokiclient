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
	"time"
)

type (
	Logger interface {
		Write([]*Entry) error
	}
	Stream map[string]string
	Value  []any
	Entry  struct {
		Stream Stream  `json:"stream"`
		Values []Value `json:"values"`
	}
	Client struct {
		addr             string
		maxRetries       int
		retryInterval    time.Duration
		in               chan *Entry
		notify           chan bool
		fallbacks        []Logger
		batched          bool
		syncMinBufferLen int
		syncInterval     time.Duration
	}
	WriterOption func(*Client)
)

var (
	_defaultClient http.Client
)

func init() {
	transport := http.DefaultTransport.(*http.Transport)
	transport.MaxConnsPerHost = 25
	transport.MaxIdleConns = 5
	transport.MaxIdleConnsPerHost = 5
	_defaultClient = http.Client{
		Transport: transport,
	}
	gob.Register(Stream{})
}

func WithRetry(max int, pause time.Duration) WriterOption {
	return func(c *Client) {
		c.maxRetries = max
		c.retryInterval = pause
	}
}

func WithBatchSync(minBufferLen int, interval time.Duration) WriterOption {
	return func(c *Client) {
		c.batched = true
		c.syncMinBufferLen = minBufferLen
		c.syncInterval = interval
	}
}

func WithFallback(l Logger) WriterOption {
	return func(c *Client) {
		c.fallbacks = append(c.fallbacks, l)
	}
}

func NewStream(app string, module string, function string, traceId string) Stream {
	m := make(map[string]string)
	m["app"] = app
	m["mod"] = module
	m["func"] = function
	m["traceId"] = traceId
	return m
}

func NewStreamCustom(m map[string]string) Stream {
	return m
}

func NewValue(params ...any) Value {
	values := make([]any, 0)
	values = append(values, params...)
	return values
}

func newEntry(stream Stream, value Value) *Entry {
	entry := new(Entry)
	entry.Stream = stream
	entry.Values = []Value{value}
	return entry
}

func NewClient(addr string) *Client {
	c := new(Client)
	c.addr = addr
	c.in = make(chan *Entry, 1000)
	c.notify = make(chan bool, 100)
	c.fallbacks = make([]Logger, 0)
	return c
}

func (c *Client) Trace(s Stream, v Value) {
	c.log("TRACE", s, v)
}

func (c *Client) Debug(s Stream, v Value) {
	c.log("DEBUG", s, v)
}

func (c *Client) Info(s Stream, v Value) {
	c.log("INFO", s, v)
}

func (c *Client) Warn(s Stream, v Value) {
	c.log("WARN", s, v)
}

func (c *Client) Error(s Stream, v Value) {
	c.log("ERROR", s, v)
}

func (c *Client) Write(e []*Entry) error {
	var errs error
	for i := 0; i <= c.maxRetries; i++ {
		if err := c.send(e); err != nil {
			errs = errors.Join(err)
			<-time.After(c.retryInterval)
			continue
		}
		if errs != nil {
			consoleWarning(errs.Error())
		}
		return nil
	}
	for _, fallback := range c.fallbacks {
		if err := fallback.Write(e); err != nil {
			errs = errors.Join(err)
			continue
		}
		if errs != nil {
			consoleWarning(errs.Error())
		}
		return nil
	}
	return errs
}

func (c *Client) Sync(ctx context.Context) {
	if !c.batched {
		go c.simpleSync(ctx)
		return
	}
	go c.batchSync(ctx)
}

func (c *Client) send(e []*Entry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	res, err := _defaultClient.Post(c.addr, "application/json", bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	if res.StatusCode/200 != 2 {
		data, _ := io.ReadAll(res.Body)
		return fmt.Errorf("%s: %s", res.Status, string(data))
	}
	return nil
}

func (c *Client) log(l string, s Stream, v Value) {
	values := make([]any, 0)
	values = append(values, time.Now().UnixNano())
	values = append(values, l)
	values = append(values, v...)
	e := newEntry(s, values)
	c.in <- e
	if len(c.in) >= c.syncMinBufferLen {
		c.notify <- true
	}
}

func (c *Client) simpleSync(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			{
				return
			}
		case e := <-c.in:
			{
				go c.Write([]*Entry{e})
			}
		}
	}
}

func (c *Client) batchSync(ctx context.Context) {
	ticker := time.NewTicker(c.syncInterval)
	for {
		select {
		case <-ctx.Done():
			{
				return
			}
		case <-ticker.C:
			{
				l := len(c.in)
				if l <= c.syncMinBufferLen {
					continue
				}
				entries := make([]*Entry, 0)
				for i := 0; i < l; i++ {
					if e := readOrReturn(c.in); e != nil {
						entries = append(entries, e)
					}
				}
				go c.Write(Group(entries))
			}
		case <-c.notify:
			{
				entries := make([]*Entry, 0)
				for i := 0; i < c.syncMinBufferLen; i++ {
					if e := readOrReturn(c.in); e != nil {
						entries = append(entries, e)
					}
				}
				go c.Write(Group(entries))
			}
		}
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

func consoleWarning(msg string) {
	log.Println("\033[31m", "WARNING:", msg, "\033[0m")
}
