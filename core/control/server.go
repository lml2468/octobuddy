package control

import (
	"encoding/json"
	"net"
	"sync"
	"time"
)

// CommandHandler processes a decoded command and returns a response body (any
// JSON-marshalable value) or an error. It runs off the server's accept loop.
type CommandHandler func(cmdType string, body json.RawMessage) (any, error)

// Server is the control-bus server: it accepts client connections, dispatches
// their commands to a handler, and broadcasts events to every connected client.
// Transport-agnostic — give it any net.Listener (UDS in prod, net.Pipe in tests).
type Server struct {
	handler CommandHandler
	now     func() time.Time

	mu      sync.Mutex
	clients map[*client]struct{}
	closed  bool
}

type client struct {
	conn      net.Conn
	sendCh    chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

const clientSendQueue = 256

func newClient(conn net.Conn) *client {
	c := &client{conn: conn, sendCh: make(chan []byte, clientSendQueue), done: make(chan struct{})}
	go c.writeLoop()
	return c
}

// writeLoop serializes all writes to this connection. Decoupling writes from
// Broadcast means one slow client cannot stall the broadcaster (and thus the
// agent turn) — it just fills its own queue. Exits when done is closed.
func (c *client) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case line := <-c.sendCh:
			if _, err := c.conn.Write(line); err != nil {
				return
			}
		}
	}
}

// enqueue queues a line for the client. Never blocks and never panics: sendCh is
// never closed (close() signals via the separate done channel), so a send here
// can only fill the buffer (dropped via default) or lose the select to done (the
// client is going away) — neither sends on a closed channel.
func (c *client) enqueue(line []byte) {
	select {
	case c.sendCh <- line:
	case <-c.done:
		// client closing: drop.
	default:
		// queue full: drop. The client is too slow; dropping an event beats
		// stalling every other client and the agent turn.
	}
}

// close stops the write loop and closes the connection. It does NOT close
// sendCh — producers (Broadcast/writeTo) may still hold a reference to this
// client and call enqueue concurrently; closing sendCh would let those sends
// panic. Stopping via `done` keeps enqueue panic-free.
func (c *client) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

// NewServer constructs a Server. handler may be nil (commands then error);
// set it later with SetHandler to break construction cycles (the handler often
// needs the gateway, whose sink needs the server).
func NewServer(handler CommandHandler) *Server {
	return &Server{
		handler: handler,
		now:     time.Now,
		clients: make(map[*client]struct{}),
	}
}

// SetHandler installs the command handler after construction.
func (s *Server) SetHandler(h CommandHandler) {
	s.mu.Lock()
	s.handler = h
	s.mu.Unlock()
}

// Serve accepts connections on l until it is closed. Blocks; run in a goroutine.
func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		c := newClient(conn)
		s.mu.Lock()
		s.clients[c] = struct{}{}
		s.mu.Unlock()
		go s.handleConn(c)
	}
}

// ConnCount returns the number of connected clients.
func (s *Server) ConnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

// Close stops broadcasting and drops all clients.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	cs := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		cs = append(cs, c)
	}
	s.clients = make(map[*client]struct{})
	s.mu.Unlock()
	for _, c := range cs {
		c.close()
	}
}

func (s *Server) handleConn(c *client) {
	defer func() {
		s.mu.Lock()
		delete(s.clients, c)
		s.mu.Unlock()
		c.close()
	}()

	sc := NewScanner(c.conn)
	for sc.Scan() {
		env, err := Decode(sc.Bytes())
		if err != nil {
			s.writeTo(c, Envelope{Kind: KindEvent, Type: "error", TS: s.now().Unix(),
				Body: mustJSON(ErrorBody{Scope: "decode", Message: err.Error()})})
			continue
		}
		if env.Kind != KindCommand {
			continue // server ignores non-commands from clients
		}
		s.dispatch(c, env)
	}
}

func (s *Server) dispatch(c *client, env Envelope) {
	s.mu.Lock()
	h := s.handler
	s.mu.Unlock()
	if h == nil {
		s.respondErr(c, env.ID, "no handler")
		return
	}
	result, err := h(env.Type, env.Body)
	if err != nil {
		s.respondErr(c, env.ID, err.Error())
		return
	}
	s.writeTo(c, Envelope{
		Kind: KindResponse, ID: env.ID, Type: env.Type, TS: s.now().Unix(),
		Body: mustJSON(result),
	})
}

func (s *Server) respondErr(c *client, id, msg string) {
	s.writeTo(c, Envelope{
		Kind: KindResponse, ID: id, Type: "error", TS: s.now().Unix(),
		Body: mustJSON(ErrorBody{Scope: "command", Message: msg}),
	})
}

// Broadcast sends an event to all connected clients. Used by the gateway bridge.
func (s *Server) Broadcast(eventType string, body any) {
	env := Envelope{Kind: KindEvent, Type: eventType, TS: s.now().Unix(), Body: mustJSON(body)}
	line, err := Encode(env)
	if err != nil {
		return
	}
	s.mu.Lock()
	cs := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	for _, c := range cs {
		c.enqueue(line)
	}
}

func (s *Server) writeTo(c *client, env Envelope) {
	line, err := Encode(env)
	if err != nil {
		return
	}
	c.enqueue(line)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
