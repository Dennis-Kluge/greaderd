package fetcher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/Dennis-Kluge/greaderd/internal/store"
)

func TestIsPublic(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"93.184.215.14", true},
		{"2606:2800:21f:cb07:6820:80da:af6b:8b2c", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"10.1.2.3", false},
		{"172.16.0.1", false},
		{"192.168.178.1", false},
		{"169.254.169.254", false}, // cloud metadata
		{"100.64.0.1", false},
		{"0.0.0.0", false},
		{"255.255.255.255", false},
		{"224.0.0.1", false},
		{"fc00::1", false},
		{"fe80::1", false},
		{"::ffff:127.0.0.1", false},
		{"::ffff:93.184.215.14", true},
		{"2002:7f00:1::", false}, // 6to4 for 127.0.0.1
	}
	for _, tt := range tests {
		if got := isPublic(netip.MustParseAddr(tt.ip)); got != tt.want {
			t.Errorf("isPublic(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestPrivateAddressesAreRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached a loopback server")
	}))
	defer srv.Close()

	s := &fakeStore{}
	f := New(s, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	if _, err := f.Discover(context.Background(), srv.URL); !errors.Is(err, ErrForbiddenAddress) {
		t.Errorf("Discover: error = %v, want ErrForbiddenAddress", err)
	}
	if err := f.Refresh(context.Background(), store.Feed{ID: 1, URL: srv.URL}); !errors.Is(err, ErrForbiddenAddress) {
		t.Errorf("Refresh: error = %v, want ErrForbiddenAddress", err)
	}
}

func TestRedirectToPrivateAddressIsRejected(t *testing.T) {
	// The redirect target is checked when connecting, like any other address.
	// A loopback server can only be reached with private networks allowed, so
	// this test uses a client that allows the first hop but not the second.
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect reached the internal server")
	}))
	defer internal.Close()
	internalAddr := netip.MustParseAddrPort(internal.Listener.Addr().String())

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer entry.Close()

	client := newClient(true)
	transport := client.Transport.(*http.Transport)
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if netip.MustParseAddrPort(address) == internalAddr {
			return nil, publicOnly(network, address, nil)
		}
		return dial(ctx, network, address)
	}

	f := New(&fakeStore{}, Options{Client: client, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if _, err := f.Discover(context.Background(), entry.URL); !errors.Is(err, ErrForbiddenAddress) {
		t.Errorf("Discover: error = %v, want ErrForbiddenAddress", err)
	}
}
