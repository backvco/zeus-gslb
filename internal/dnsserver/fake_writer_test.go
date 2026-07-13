package dnsserver

import (
	"net"

	"github.com/miekg/dns"
)

// fakeWriter is a minimal dns.ResponseWriter that just captures the message
// written to it, so handler tests can assert on the response without
// binding a real socket.
type fakeWriter struct {
	msg *dns.Msg
}

func (f *fakeWriter) LocalAddr() net.Addr  { return &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5355} }
func (f *fakeWriter) RemoteAddr() net.Addr { return &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345} }
func (f *fakeWriter) WriteMsg(m *dns.Msg) error {
	f.msg = m
	return nil
}
func (f *fakeWriter) Write(b []byte) (int, error) { return len(b), nil }
func (f *fakeWriter) Close() error                { return nil }
func (f *fakeWriter) TsigStatus() error           { return nil }
func (f *fakeWriter) TsigTimersOnly(bool)         {}
func (f *fakeWriter) Hijack()                     {}
