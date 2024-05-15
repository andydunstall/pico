package rpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/andydunstall/piko/pkg/conn"
	"github.com/andydunstall/piko/pkg/log"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

var (
	ErrStreamClosed = errors.New("stream closed")
)

type message struct {
	Header  *header
	Payload []byte
}

// Stream represents a bi-directional RPC stream between two peers. Either peer
// can send an RPC request to the other.
//
// The stream uses the underlying bi-directional connection to send RPC
// requests, and multiplexes multiple concurrent request/response RPCs on the
// same connection.
//
// Incoming RPC requests are handled in their own goroutine to avoid blocking
// the stream.
type Stream interface {
	Addr() string
	RPC(ctx context.Context, rpcType Type, req []byte) ([]byte, error)
	Monitor(
		ctx context.Context,
		interval time.Duration,
		timeout time.Duration,
	) error
	Close() error
}

type stream struct {
	conn    conn.Conn
	handler *Handler

	// nextMessageID is the ID of the next RPC message to send.
	nextMessageID *atomic.Uint64

	writeCh chan *message

	// responseHandlers contains channels for RPC responses.
	responseHandlers   map[uint64]chan<- *message
	responseHandlersMu sync.Mutex

	// shutdownCh is closed when the stream is shutdown.
	shutdownCh chan struct{}
	// shutdownErr is the first error that caused the stream to shutdown.
	shutdownErr error
	// shutdown indicates whether the stream is already shutdown.
	shutdown *atomic.Bool

	logger log.Logger
}

// NewStream creates an RPC stream on top of the given message-oriented
// connection.
func NewStream(conn conn.Conn, handler *Handler, logger log.Logger) Stream {
	stream := &stream{
		conn:             conn,
		handler:          handler,
		nextMessageID:    atomic.NewUint64(0),
		writeCh:          make(chan *message, 64),
		responseHandlers: make(map[uint64]chan<- *message),
		shutdownCh:       make(chan struct{}),
		shutdown:         atomic.NewBool(false),
		logger:           logger.WithSubsystem("rpc"),
	}
	go stream.reader()
	go stream.writer()

	return stream
}

func (s *stream) Addr() string {
	return s.conn.Addr()
}

// RPC sends the given request message to the peer and returns the response or
// an error.
//
// RPC is thread safe.
func (s *stream) RPC(ctx context.Context, rpcType Type, req []byte) ([]byte, error) {
	header := &header{
		RPCType: rpcType,
		ID:      s.nextMessageID.Inc(),
	}
	msg := &message{
		Header:  header,
		Payload: req,
	}

	ch := make(chan *message, 1)
	s.registerResponseHandler(header.ID, ch)
	defer s.unregisterResponseHandler(header.ID)

	select {
	case s.writeCh <- msg:
	case <-s.shutdownCh:
		return nil, s.shutdownErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case resp := <-ch:
		if resp.Header.Flags.ErrNotSupported() {
			return nil, fmt.Errorf("not supported")
		}
		return resp.Payload, nil
	case <-s.shutdownCh:
		return nil, s.shutdownErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Monitor monitors the stream is healthy using heartbeats.
func (s *stream) Monitor(
	ctx context.Context,
	interval time.Duration,
	timeout time.Duration,
) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := s.heartbeat(ctx, timeout); err != nil {
			return fmt.Errorf("heartbeat: %w", err)

		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.shutdownCh:
			return s.shutdownErr
		case <-ticker.C:
		}
	}
}

func (s *stream) Close() error {
	return s.closeStream(ErrStreamClosed)
}

func (s *stream) reader() {
	defer s.recoverPanic("reader()")

	for {
		b, err := s.conn.ReadMessage()
		if err != nil {
			_ = s.closeStream(fmt.Errorf("read: %w", err))
			return
		}

		var header header
		if err = header.Decode(b); err != nil {
			_ = s.closeStream(fmt.Errorf("decode header: %w", err))
			return
		}
		payload := b[headerSize:]

		s.logger.Debug(
			"message received",
			zap.String("type", header.RPCType.String()),
			zap.Bool("response", header.Flags.Response()),
			zap.Uint64("message_id", header.ID),
			zap.Int("len", len(payload)),
		)

		if header.Flags.Response() {
			s.handleResponse(&message{
				Header:  &header,
				Payload: payload,
			})
		} else {
			// Spawn a new goroutine for each request to avoid blocking
			// the read loop.
			go s.handleRequest(&message{
				Header:  &header,
				Payload: payload,
			})
		}

		select {
		case <-s.shutdownCh:
			return
		default:
		}
	}
}

func (s *stream) writer() {
	defer s.recoverPanic("writer()")

	for {
		select {
		case req := <-s.writeCh:
			if err := s.write(req); err != nil {
				_ = s.closeStream(fmt.Errorf("write: %w", err))
				return
			}

			s.logger.Debug(
				"message sent",
				zap.String("type", req.Header.RPCType.String()),
				zap.Bool("response", req.Header.Flags.Response()),
				zap.Uint64("message_id", req.Header.ID),
				zap.Int("len", len(req.Payload)),
			)
		case <-s.shutdownCh:
			return
		}
	}
}

func (s *stream) write(req *message) error {
	w, err := s.conn.NextWriter()
	if err != nil {
		return err
	}
	if _, err = w.Write(req.Header.Encode()); err != nil {
		return err
	}
	if len(req.Payload) > 0 {
		if _, err = w.Write(req.Payload); err != nil {
			return err
		}
	}
	return w.Close()
}

func (s *stream) closeStream(err error) error {
	// Only shutdown once.
	if !s.shutdown.CompareAndSwap(false, true) {
		return ErrStreamClosed
	}

	s.shutdownErr = ErrStreamClosed
	// Close to cancel pending RPC requests.
	close(s.shutdownCh)

	if err := s.conn.Close(); err != nil {
		return fmt.Errorf("close conn: %w", err)
	}

	s.logger.Debug(
		"stream closed",
		zap.Error(err),
	)

	return nil
}

func (s *stream) handleRequest(m *message) {
	handlerFunc, ok := s.handler.Find(m.Header.RPCType)
	if !ok {
		// If no handler is found, send a 'not supported' error to the client.
		s.logger.Warn(
			"rpc type not supported",
			zap.String("type", m.Header.RPCType.String()),
			zap.Uint64("message_id", m.Header.ID),
		)

		var flags flags
		flags.SetResponse()
		flags.SetErrNotSupported()
		msg := &message{
			Header: &header{
				RPCType: m.Header.RPCType,
				ID:      m.Header.ID,
				Flags:   flags,
			},
		}
		select {
		case s.writeCh <- msg:
			return
		case <-s.shutdownCh:
			return
		}
	}

	resp := handlerFunc(m.Payload)

	var flags flags
	flags.SetResponse()
	msg := &message{
		Header: &header{
			RPCType: m.Header.RPCType,
			ID:      m.Header.ID,
			Flags:   flags,
		},
		Payload: resp,
	}

	select {
	case s.writeCh <- msg:
		return
	case <-s.shutdownCh:
		return
	}
}

func (s *stream) handleResponse(m *message) {
	// If no handler is found, it means RPC has already returned so discard
	// the response.
	ch, ok := s.findResponseHandler(m.Header.ID)
	if ok {
		ch <- m
	}
}

func (s *stream) recoverPanic(prefix string) {
	if r := recover(); r != nil {
		_ = s.closeStream(fmt.Errorf("panic: %s: %v", prefix, r))
	}
}

func (s *stream) registerResponseHandler(id uint64, ch chan<- *message) {
	s.responseHandlersMu.Lock()
	defer s.responseHandlersMu.Unlock()

	s.responseHandlers[id] = ch
}

func (s *stream) unregisterResponseHandler(id uint64) {
	s.responseHandlersMu.Lock()
	defer s.responseHandlersMu.Unlock()

	delete(s.responseHandlers, id)
}

func (s *stream) findResponseHandler(id uint64) (chan<- *message, bool) {
	s.responseHandlersMu.Lock()
	defer s.responseHandlersMu.Unlock()

	ch, ok := s.responseHandlers[id]
	return ch, ok
}

func (s *stream) heartbeat(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ts := time.Now()
	_, err := s.RPC(ctx, TypeHeartbeat, nil)
	if err != nil {
		return fmt.Errorf("rpc: %w", err)
	}

	s.logger.Debug("heartbeat ok", zap.Duration("rtt", time.Since(ts)))

	return nil
}
