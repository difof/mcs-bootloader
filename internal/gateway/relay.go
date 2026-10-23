package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"time"
)

func setKeepAlive(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(30 * time.Second)
	}
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

type relayResult struct {
	direction int
	bytes     int64
	err       error
}

type trafficWriter struct {
	writer io.Writer
	bytes  *atomic.Uint64
}

func (w trafficWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.bytes.Add(uint64(n))
	return n, err
}

func relay(ctx context.Context, client, backend net.Conn, stats ...*counters) (toBackend, toClient int64, err error) {
	results := make(chan relayResult, 2)
	copyDirection := func(direction int, dst, src net.Conn) {
		var writer io.Writer = dst
		if len(stats) > 0 {
			counter := &stats[0].bytesToBackend
			if direction == 1 {
				counter = &stats[0].bytesToClient
			}
			writer = trafficWriter{writer: dst, bytes: counter}
		}
		n, copyErr := io.Copy(writer, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		results <- relayResult{direction: direction, bytes: n, err: copyErr}
	}
	go copyDirection(0, backend, client)
	go copyDirection(1, client, backend)
	first := <-results
	if first.err != nil {
		client.Close()
		backend.Close()
	}
	// After one peer has ended its stream, allow the other direction to drain.
	// This timeout applies only to a closing session, never to live gameplay.
	drain := time.NewTimer(5 * time.Second)
	defer drain.Stop()
	var second relayResult
	select {
	case second = <-results:
	case <-ctx.Done():
		client.Close()
		backend.Close()
		second = <-results
	case <-drain.C:
		client.Close()
		backend.Close()
		second = <-results
	}
	for _, r := range []relayResult{first, second} {
		if r.direction == 0 {
			toBackend = r.bytes
		} else {
			toClient = r.bytes
		}
		if r.err != nil && !errors.Is(r.err, net.ErrClosed) && ctx.Err() == nil {
			err = r.err
		}
	}
	return
}
