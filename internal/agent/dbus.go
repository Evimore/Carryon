package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/godbus/dbus/v5"
)

// The notification service every desktop provides on the session bus.
const (
	notifyName  = "org.freedesktop.Notifications"
	notifyPath  = dbus.ObjectPath("/org/freedesktop/Notifications")
	notifyIface = "org.freedesktop.Notifications"
)

// Bus is the desktop's notification service, reached over the user's session bus.
type Bus struct {
	conn    *dbus.Conn
	obj     dbus.BusObject
	signals chan Signal

	mu    sync.Mutex
	owner string // the service's unique name when it last showed a notification
}

// NewBus listens for the service's signals on conn, normally dbus.ConnectSessionBus().
func NewBus(conn *dbus.Conn) (*Bus, error) {
	if err := conn.AddMatchSignal(dbus.WithMatchObjectPath(notifyPath), dbus.WithMatchInterface(notifyIface)); err != nil {
		return nil, fmt.Errorf("listening to the desktop's notifications: %w", err)
	}
	b := &Bus{conn: conn, obj: conn.Object(notifyName, notifyPath), signals: make(chan Signal, 16)}
	raw := make(chan *dbus.Signal, 16)
	conn.Signal(raw)
	go b.forward(raw)
	return b, nil
}

// forward turns the service's signals into Signals until the connection closes.
func (b *Bus) forward(raw <-chan *dbus.Signal) {
	defer close(b.signals)
	for s := range raw {
		b.mu.Lock()
		owner := b.owner
		b.mu.Unlock()
		// Only the service that showed the notifications speaks for them.
		if s.Path != notifyPath || owner == "" || s.Sender != owner || len(s.Body) < 2 {
			continue
		}
		id, ok := s.Body[0].(uint32)
		if !ok {
			continue
		}
		switch s.Name {
		case notifyIface + ".ActionInvoked":
			if key, ok := s.Body[1].(string); ok {
				b.signals <- Signal{ID: id, Action: key}
			}
		case notifyIface + ".NotificationClosed":
			b.signals <- Signal{ID: id, Closed: true}
		}
	}
}

// Signals delivers buttons pressed and notifications closed.
func (b *Bus) Signals() <-chan Signal { return b.signals }

// Capabilities asks the service what it can do.
func (b *Bus) Capabilities(ctx context.Context) ([]string, error) {
	var caps []string
	if err := b.obj.CallWithContext(ctx, notifyIface+".GetCapabilities", 0).Store(&caps); err != nil {
		return nil, err
	}
	return caps, nil
}

// Notify shows n.
func (b *Bus) Notify(ctx context.Context, n Notification) (uint32, error) {
	actions := []string{}
	for _, a := range n.Actions {
		actions = append(actions, a.Key, a.Label)
	}
	hints := map[string]dbus.Variant{"urgency": dbus.MakeVariant(n.Urgency)}
	// Notify(app_name, replaces_id, app_icon, summary, body, actions, hints, expire_timeout)
	var id uint32
	err := b.obj.CallWithContext(ctx, notifyIface+".Notify", 0,
		"Carryon", uint32(0), n.Icon, n.Title, n.Body, actions, hints, int32(-1)).Store(&id)
	if err != nil {
		return 0, err
	}
	var owner string
	if err := b.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, notifyName).Store(&owner); err == nil {
		b.mu.Lock()
		b.owner = owner
		b.mu.Unlock()
	}
	return id, nil
}
