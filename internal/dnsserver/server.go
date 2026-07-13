package dnsserver

import (
	"context"
	"fmt"

	"github.com/miekg/dns"
)

// Server runs the UDP + TCP DNS listeners for one Handler.
type Server struct {
	addr string
	h    *Handler

	udp *dns.Server
	tcp *dns.Server
}

// NewServer builds a Server bound to addr (e.g. ":5355") serving h. Both UDP
// and TCP are always started (§8/task spec: "UDP+TCP") — DNS responses can
// exceed the UDP-safe size once a record legitimately has 3 answers plus
// EDNS0 padding on some resolvers, so TCP must be available too.
func NewServer(addr string, h *Handler) *Server {
	return &Server{
		addr: addr,
		h:    h,
		udp:  &dns.Server{Addr: addr, Net: "udp", Handler: h},
		tcp:  &dns.Server{Addr: addr, Net: "tcp", Handler: h},
	}
}

// Run starts both listeners and blocks until ctx is cancelled or either
// listener fails. It always attempts to shut down both before returning.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 2)

	go func() { errCh <- runNamed("udp", s.udp.ListenAndServe) }()
	go func() { errCh <- runNamed("tcp", s.tcp.ListenAndServe) }()

	select {
	case <-ctx.Done():
		s.shutdown()
		return ctx.Err()
	case err := <-errCh:
		s.shutdown()
		return err
	}
}

func runNamed(name string, fn func() error) error {
	if err := fn(); err != nil {
		return fmt.Errorf("%s listener: %w", name, err)
	}
	return nil
}

func (s *Server) shutdown() {
	_ = s.udp.ShutdownContext(context.Background())
	_ = s.tcp.ShutdownContext(context.Background())
}
