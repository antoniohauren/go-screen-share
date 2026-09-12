//go:build linux && cgo

package sharer

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type testPortal struct{ conn *dbus.Conn }

func (p testPortal) Start(sender dbus.Sender, session dbus.ObjectPath, parent string, options map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	name := strings.ReplaceAll(strings.TrimPrefix(string(sender), ":"), ".", "_")
	path := dbus.ObjectPath(string(portalPath) + "/request/" + name + "/" + options["handle_token"].Value().(string))
	streams := []struct {
		Node       uint32
		Properties map[string]dbus.Variant
	}{{42, map[string]dbus.Variant{"source_type": dbus.MakeVariant(uint32(1))}}}
	if err := p.conn.Emit(path, portalRequest+".Response", uint32(0), map[string]dbus.Variant{
		"streams": dbus.MakeVariant(streams),
	}); err != nil {
		return "", dbus.MakeFailedError(err)
	}
	return path, nil
}

// Run with dbus-run-session -- go test ./internal/sharer -run TestPortalStreams.
func TestPortalStreams(t *testing.T) {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		t.Skip("requires session bus; run with dbus-run-session")
	}
	backend, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if err := backend.Export(testPortal{backend}, portalPath, screenCast); err != nil {
		t.Fatal(err)
	}
	client, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	s := &Source{conn: client, owner: backend.Names()[0]}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	results, err := s.request(ctx, "Start", map[string]dbus.Variant{}, dbus.ObjectPath("/session/test"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := results["streams"].Signature().String(); got != "a(ua{sv})" {
		t.Fatalf("portal returned missing or invalid streams: signature %q, want a(ua{sv})", got)
	}
	var streams []struct {
		Node       uint32
		Properties map[string]dbus.Variant
	}
	if err := results["streams"].Store(&streams); err != nil {
		t.Fatal(err)
	}
	if len(streams) != 1 || streams[0].Node != 42 || streams[0].Properties["source_type"].Value() != uint32(1) {
		t.Fatalf("unexpected streams: %#v", streams)
	}
}
