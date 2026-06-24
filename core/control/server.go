package control

import (
	"crypto/subtle"
	"encoding/json"
	"io"
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

	// authToken is the GUI capability token (empty = unset). When empty, no
	// client can ever authenticate, so every privileged command is denied — the
	// fail-closed default. privileged is the set of command types that require an
	// authenticated connection. Both are configured once at startup via SetAuth,
	// before Serve, and read under mu.
	authToken  string
	privileged map[string]bool
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

// SetAuth configures the capability-token gate. token is the secret a client
// must present via an "auth" command (constant-time compared); privileged is
// the set of command types that require an authenticated connection. An empty
// token means no client can ever authenticate, so every privileged command —
// and the broadcast event stream — is denied (fail closed). Call before Serve.
func (s *Server) SetAuth(token string, privileged []string) {
	set := make(map[string]bool, len(privileged))
	for _, p := range privileged {
		set[p] = true
	}
	s.mu.Lock()
	s.authToken = token
	s.privileged = set
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
	// Surface scanner failure (oversized frame → bufio.ErrTooLong, framing
	// truncation, …) instead of silently dropping the client. Routine EOFs
	// (io.EOF on client disconnect) are normal; everything else is worth a
	// log line for the operator who later wonders why a peer's session ended.
	if err := sc.Err(); err != nil && err != io.EOF {
		s.writeTo(c, Envelope{Kind: KindEvent, Type: "error", TS: s.now().Unix(),
			Body: mustJSON(ErrorBody{Scope: "wire", Message: err.Error()})})
	}
}

func (s *Server) dispatch(c *client, env Envelope) {
	if env.Type == CmdAuth {
		s.authenticate(c, env)
		return
	}
	s.mu.Lock()
	h := s.handler
	priv := s.privileged[env.Type]
	s.mu.Unlock()
	if h == nil {
		s.respondErr(c, env.ID, "no handler")
		return
	}
	// Capability gate: a privileged command on an unauthenticated connection is
	// refused. This is the same-uid boundary the peer-cred check cannot draw —
	// it distinguishes the operator's GUI (which holds the token) from the
	// spawned agent's CLI (which does not). Read-only commands pass freely.
	if priv && !c.authed.Load() {
		s.respondErr(c, env.ID, "unauthorized: command requires the GUI capability token")
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

// authenticate handles the "auth" handshake: it constant-time compares the
// presented token against the configured one and, on a match, marks the
// connection authorized. An unset (empty) token never authenticates. The token
// is never logged; the rejection reason is intentionally generic.
func (s *Server) authenticate(c *client, env Envelope) {
	s.mu.Lock()
	token := s.authToken
	s.mu.Unlock()
	var b AuthBody
	if err := json.Unmarshal(env.Body, &b); err != nil {
		s.respondErr(c, env.ID, "auth: invalid body")
		return
	}
	if token == "" || subtle.ConstantTimeCompare([]byte(b.Token), []byte(token)) != 1 {
		s.respondErr(c, env.ID, "auth: rejected")
		return
	}
	c.authed.Store(true)
	s.writeTo(c, Envelope{
		Kind: KindResponse, ID: env.ID, Type: CmdAuth, TS: s.now().Unix(),
		Body: mustJSON(OKBody{OK: true}),
	})
}

func (s *Server) respondErr(c *client, id, msg string) {
	s.writeTo(c, Envelope{
		Kind: KindResponse, ID: id, Type: "error", TS: s.now().Unix(),
		Body: mustJSON(ErrorBody{Scope: "command", Message: msg}),
	})
}

// Broadcast sends an event to all connected clients. Used by the gateway bridge.
// When a capability token is configured, the event stream is operator-only: it
// carries every session's live text/tool activity (cross-session disclosure), so
// only authenticated connections (the GUI) receive it; an unauthenticated
// same-uid peer (a spawned agent) gets nothing. With no token configured the
// stream is open (legacy/dev + CLI observers).
func (s *Server) Broadcast(eventType string, body any) {
	s.mu.Lock()
	if len(s.clients) == 0 {
		s.mu.Unlock()
		return // no client attached — skip the per-event marshal+encode entirely
	}
	gated := s.authToken != ""
	cs := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	env := Envelope{Kind: KindEvent, Type: eventType, TS: s.now().Unix(), Body: mustJSON(body)}
	line, err := Encode(env)
	if err != nil {
		return
	}
	for _, c := range cs {
		if gated && !c.authed.Load() {
			continue
		}
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
