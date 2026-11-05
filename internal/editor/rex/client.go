package rex

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// A Rex control connection: a WebSocket on the server's Unix socket
// carrying JSON requests, responses and events. Read blocks on the socket
// (run it from a goroutine if you like); Next drains what was read. The
// Client is safe for concurrent use: Request may be called while another
// goroutine is in Read.

const (
	Protocol    = "rex.control.v1"
	APIRevision = 7
)

var (
	ErrNotUnixURL       = errors.New("rex: not a unix:// url")
	ErrNameTooLong      = errors.New("rex: socket path too long")
	ErrResponseTooLarge = errors.New("rex: handshake response too large")
	ErrConnectionClosed = errors.New("rex: connection closed")
	ErrNotWelcome       = errors.New("rex: not a welcome")
	ErrBadMessage       = errors.New("rex: bad message")
)

// JSON values are decoded with encoding/json into any, with numbers as
// json.Number. Use Get, Str, Int, GetInt and GetStr to read them.

// MessageKind tells what a ClientMessage carries.
type MessageKind int

const (
	KindResponse MessageKind = iota
	KindEvent
	// KindClosed: the server closed the connection.
	KindClosed
)

// ClientMessage is a response, an event, or the server's close.
type ClientMessage struct {
	Kind     MessageKind
	Response *Response
	Event    *Event
	Closed   *Close
}

// Result is either Ok (a JSON value, nil for JSON null/absent) or Err.
type Result struct {
	Ok  any
	Err *Error
}

type Response struct {
	ID     uint64
	Seq    int64
	Result Result
}

type Error struct {
	Code    string
	Message string
	Status  int64
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Event is a server event, or a session event unwrapped from session_event.
type Event struct {
	Seq int64
	// SessionID is set for session events.
	SessionID *string
	Name      string
	// Payload is nil when the event has no fields.
	Payload any
}

type Welcome struct {
	MaxMessageBytes int
	APIRevision     int64
}

type Client struct {
	conn net.Conn

	mu         sync.Mutex // guards ws
	ws         WebSocket
	writeMu    sync.Mutex // guards writes, nextID, open
	nextID     uint64
	open       bool
	MaxMessage int // the server's limits.max_message_bytes
}

// Connect dials serverURL (REX_SERVER), then exchanges hello and welcome.
// Blocks until welcomed.
func Connect(serverURL string) (*Client, error) {
	path, err := SocketPath(serverURL)
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, nextID: 1, MaxMessage: 65536}
	if err := c.handshake(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// NewClient runs the handshake over an already connected conn.
func NewClient(conn net.Conn) (*Client, error) {
	c := &Client{conn: conn, nextID: 1, MaxMessage: 65536}
	if err := c.handshake(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) handshake() error {
	key := NewKey()
	var req bytes.Buffer
	// any host works on the Unix socket
	WriteRequest(&req, "localhost", "/api/control", Protocol, key)
	if _, err := c.conn.Write(req.Bytes()); err != nil {
		return err
	}
	head := make([]byte, 4096)
	n, end := 0, -1
	for end < 0 {
		if n == len(head) {
			return ErrResponseTooLarge
		}
		m, err := c.conn.Read(head[n:])
		n += m
		if m == 0 && err != nil {
			if err == io.EOF {
				return ErrConnectionClosed
			}
			return err
		}
		end = HeadEnd(head[:n])
	}
	if err := CheckResponse(string(head[:end]), key, Protocol); err != nil {
		return err
	}
	c.open = true
	c.ws.Feed(head[end:n])

	hello, _ := marshal(map[string]any{
		"type":         "hello",
		"api_revision": APIRevision,
		"client":       map[string]any{"kind": "other", "name": "rexmod"},
	})
	if err := c.write(OpText, hello); err != nil {
		return err
	}
	for {
		m, err := c.ws.Next()
		if err != nil {
			return err
		}
		if m != nil {
			switch m.Op {
			case OpText:
				v, err := decode(m.Data)
				if err != nil {
					return err
				}
				w, err := ParseWelcome(v)
				if err != nil {
					return err
				}
				c.MaxMessage = w.MaxMessageBytes
				c.ws.MaxMessage = w.MaxMessageBytes
				return nil
			case OpClose:
				return ErrConnectionClosed
			}
			continue
		}
		ok, err := c.Read()
		if err != nil {
			return err
		}
		if !ok {
			return ErrConnectionClosed
		}
	}
}

// Close sends a close frame, then closes the socket.
func (c *Client) Close() error {
	c.writeMu.Lock()
	if c.open {
		WriteClose(c.conn, 1000)
		c.open = false
	}
	c.writeMu.Unlock()
	return c.conn.Close()
}

// Conn is the underlying socket.
func (c *Client) Conn() net.Conn { return c.conn }

func (c *Client) write(op Opcode, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return WriteFrame(c.conn, op, payload)
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// DecodeJSON decodes a JSON value the way messages are: numbers stay
// json.Number, for Get, Str and Int.
func DecodeJSON(b []byte) (any, error) { return decode(b) }

func decode(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

type wireRequest struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Payload any    `json:"payload,omitempty"`
}

// EncodeRequest is the JSON of request id; a nil payload is left out.
func EncodeRequest(id uint64, method string, payload any) ([]byte, error) {
	return marshal(wireRequest{Type: "request", ID: strconv.FormatUint(id, 10), Method: method, Payload: payload})
}

// Request sends a request and returns its id. A nil payload is left out.
func (c *Client) Request(method string, payload any) (uint64, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	id := c.nextID
	c.nextID++
	b, err := EncodeRequest(id, method, payload)
	if err != nil {
		return 0, err
	}
	// the server closes the connection on a request over its limit
	if len(b) > c.MaxMessage {
		return 0, ErrMessageTooLarge
	}
	return id, WriteFrame(c.conn, OpText, b)
}

// Read does one blocking read from the socket. False when the server hung up.
func (c *Client) Read() (bool, error) {
	buf := make([]byte, 16384)
	n, err := c.conn.Read(buf)
	if n > 0 {
		c.mu.Lock()
		c.ws.Feed(buf[:n])
		c.mu.Unlock()
		return true, nil
	}
	if err == nil {
		return true, nil
	}
	if err == io.EOF || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, net.ErrClosed) {
		return false, nil
	}
	return false, err
}

// Next returns the next message read, or nil until more is read. Answers
// pings and skips unknown types.
func (c *Client) Next() (*ClientMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		m, err := c.ws.Next()
		if err != nil || m == nil {
			return nil, err
		}
		switch m.Op {
		case OpPing:
			if err := c.write(OpPong, m.Data); err != nil {
				return nil, err
			}
		case OpClose:
			c.writeMu.Lock()
			if c.open {
				// echo the code, as RFC 6455 asks
				code := uint16(1000)
				if m.Close.HasCode {
					code = m.Close.Code
				}
				WriteClose(c.conn, code)
				c.open = false
			}
			c.writeMu.Unlock()
			return &ClientMessage{Kind: KindClosed, Closed: m.Close}, nil
		case OpText:
			v, err := decode(m.Data)
			if err != nil {
				return nil, err
			}
			msg, err := ParseMessage(v)
			if err != nil {
				return nil, err
			}
			if msg != nil {
				return msg, nil
			}
		}
	}
}

// SocketPath is the percent-decoded path in a unix:// URL.
func SocketPath(u string) (string, error) {
	const scheme = "unix://"
	if !strings.HasPrefix(u, scheme) {
		return "", ErrNotUnixURL
	}
	p, err := url.PathUnescape(u[len(scheme):])
	if err != nil {
		return "", err
	}
	// sockaddr_un.sun_path
	if len(p) > 103 {
		return "", ErrNameTooLong
	}
	return p, nil
}

func ParseWelcome(root any) (*Welcome, error) {
	if s, _ := GetStr(root, "type"); s != "welcome" {
		return nil, ErrNotWelcome
	}
	if s, _ := GetStr(root, "protocol"); s != Protocol {
		return nil, ErrBadProtocol
	}
	max, ok := GetInt(Get(root, "limits"), "max_message_bytes")
	if !ok || max < 65536 {
		return nil, ErrNotWelcome
	}
	rev, _ := GetInt(root, "api_revision")
	return &Welcome{MaxMessageBytes: int(max), APIRevision: rev}, nil
}

// ParseMessage reads a response or event, or nil for a type we don't know.
func ParseMessage(root any) (*ClientMessage, error) {
	kind, ok := GetStr(root, "type")
	if !ok {
		return nil, ErrBadMessage
	}
	switch kind {
	case "response":
		idStr, ok := GetStr(root, "id")
		if !ok {
			return nil, ErrBadMessage
		}
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			return nil, nil // not one of ours
		}
		seq, _ := GetInt(root, "seq")
		r := &Response{ID: id, Seq: seq}
		if e, ok := Get(root, "error").(map[string]any); ok {
			code, _ := GetStr(e, "code")
			msg, _ := GetStr(e, "message")
			status, _ := GetInt(e, "status")
			r.Result.Err = &Error{Code: code, Message: msg, Status: status}
		} else {
			r.Result.Ok = Get(root, "payload")
		}
		return &ClientMessage{Kind: KindResponse, Response: r}, nil
	case "event":
		seq, _ := GetInt(root, "seq")
		event := Get(root, "payload")
		name, ok := GetStr(event, "type")
		if !ok {
			return nil, ErrBadMessage
		}
		var sid *string
		if name == "session_event" {
			inner := Get(event, "payload")
			if s, ok := GetStr(inner, "session_id"); ok {
				sid = &s
			}
			event = Get(inner, "event")
			if name, ok = GetStr(event, "type"); !ok {
				return nil, ErrBadMessage
			}
		}
		return &ClientMessage{Kind: KindEvent, Event: &Event{Seq: seq, SessionID: sid, Name: name, Payload: Get(event, "payload")}}, nil
	}
	return nil, nil
}

// Get is a field of an object, or nil.
func Get(v any, name string) any {
	if o, ok := v.(map[string]any); ok {
		return o[name]
	}
	return nil
}

func Str(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// Int reads an integer (a json.Number without a fraction, or a Go int type).
func Int(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := strconv.ParseInt(string(n), 10, 64)
		return i, err == nil
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

func GetInt(v any, name string) (int64, bool)  { return Int(Get(v, name)) }
func GetStr(v any, name string) (string, bool) { return Str(Get(v, name)) }
