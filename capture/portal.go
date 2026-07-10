// Package capture opens desktop capture streams through xdg-desktop-portal.
package capture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

const (
	SourceMonitor = "monitor"

	CursorModeHidden   = "hidden"
	CursorModeEmbedded = "embedded"
)

const (
	portalDestination   = "org.freedesktop.portal.Desktop"
	portalPath          = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	screenCastInterface = "org.freedesktop.portal.ScreenCast"
	requestInterface    = "org.freedesktop.portal.Request"
	sessionInterface    = "org.freedesktop.portal.Session"
	propertiesInterface = "org.freedesktop.DBus.Properties"

	requestResponseSignal = requestInterface + ".Response"
	sessionClosedSignal   = sessionInterface + ".Closed"

	sourceTypeMonitor  uint32 = 1
	cursorModeHidden   uint32 = 1
	cursorModeEmbedded uint32 = 2

	portalCleanupTimeout = 2 * time.Second
)

var (
	ErrRequestCancelled = errors.New("desktop capture authorization was cancelled")
	ErrSessionClosed    = errors.New("desktop portal screen cast session closed")
)

// Config controls the source requested from the desktop portal.
type Config struct {
	Source     string
	CursorMode string
}

// Validate checks the implemented portal capture options.
func (cfg Config) Validate() error {
	var errs []error

	if cfg.Source != SourceMonitor {
		errs = append(errs, errors.New("capture source must be monitor"))
	}

	switch cfg.CursorMode {
	case CursorModeHidden, CursorModeEmbedded:
	default:
		errs = append(errs, errors.New("capture cursor mode must be hidden or embedded"))
	}

	return errors.Join(errs...)
}

// Stream owns a duplicated PipeWire remote for one media pipeline.
type Stream struct {
	PipeWireFD        int
	NodeID            uint32
	PipeWireSerial    uint64
	HasPipeWireSerial bool
	Properties        map[string]dbus.Variant

	remote    *os.File
	closeOnce sync.Once
	closeErr  error
}

// Close releases the per-pipeline PipeWire remote duplicate.
func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.remote.Close()
	})
	return s.closeErr
}

type streamMetadata struct {
	nodeID            uint32
	pipeWireSerial    uint64
	hasPipeWireSerial bool
	properties        map[string]dbus.Variant
}

// Session owns one portal ScreenCast session and its PipeWire remote.
type Session struct {
	conn         *dbus.Conn
	path         dbus.ObjectPath
	remote       *os.File
	stream       streamMetadata
	signals      chan *dbus.Signal
	sessionMatch []dbus.MatchOption

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	stateMu   sync.Mutex
	closing   bool
	err       error
}

// Open runs the xdg-desktop-portal ScreenCast flow and returns one monitor stream.
func Open(ctx context.Context, cfg Config) (_ *Session, err error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connect to session D-Bus: %w", err)
	}
	if !conn.SupportsUnixFDs() {
		_ = conn.Close()
		return nil, errors.New("session D-Bus connection does not support Unix file descriptors")
	}

	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)

	requestMatch := []dbus.MatchOption{
		dbus.WithMatchSender(portalDestination),
		dbus.WithMatchInterface(requestInterface),
		dbus.WithMatchMember("Response"),
	}
	sessionMatch := []dbus.MatchOption{
		dbus.WithMatchSender(portalDestination),
		dbus.WithMatchInterface(sessionInterface),
		dbus.WithMatchMember("Closed"),
	}

	requestMatchAdded := false
	sessionMatchAdded := false
	var sessionPath dbus.ObjectPath
	var remote *os.File

	defer func() {
		if err == nil {
			return
		}

		var cleanupErr error
		if sessionPath.IsValid() {
			cleanupErr = errors.Join(cleanupErr, closePortalObject(conn, sessionPath, sessionInterface))
		}
		if remote != nil {
			cleanupErr = errors.Join(cleanupErr, remote.Close())
		}

		cleanupCtx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
		defer cancel()
		if requestMatchAdded {
			cleanupErr = errors.Join(cleanupErr, conn.RemoveMatchSignalContext(cleanupCtx, requestMatch...))
		}
		if sessionMatchAdded {
			cleanupErr = errors.Join(cleanupErr, conn.RemoveMatchSignalContext(cleanupCtx, sessionMatch...))
		}

		conn.RemoveSignal(signals)
		cleanupErr = errors.Join(cleanupErr, conn.Close())
		err = errors.Join(err, cleanupErr)
	}()

	if err = conn.AddMatchSignalContext(ctx, requestMatch...); err != nil {
		return nil, fmt.Errorf("subscribe to portal request responses: %w", err)
	}
	requestMatchAdded = true

	if err = conn.AddMatchSignalContext(ctx, sessionMatch...); err != nil {
		return nil, fmt.Errorf("subscribe to portal session closure: %w", err)
	}
	sessionMatchAdded = true

	client := portalClient{
		conn:    conn,
		object:  conn.Object(portalDestination, portalPath),
		signals: signals,
	}

	sessionToken, err := portalToken()
	if err != nil {
		return nil, fmt.Errorf("create portal session token: %w", err)
	}
	createResults, err := client.request(ctx, "CreateSession", "", map[string]dbus.Variant{
		"session_handle_token": dbus.MakeVariant(sessionToken),
	})
	if err != nil {
		return nil, fmt.Errorf("create screen cast session: %w", err)
	}

	sessionHandle, ok := createResults["session_handle"]
	if !ok {
		return nil, errors.New("create screen cast session response did not contain session_handle")
	}
	var sessionPathString string
	if err := dbus.Store([]any{sessionHandle}, &sessionPathString); err != nil {
		return nil, fmt.Errorf("decode screen cast session handle: %w", err)
	}
	sessionPath = dbus.ObjectPath(sessionPathString)
	if !sessionPath.IsValid() {
		return nil, fmt.Errorf("portal returned invalid session object path %q", sessionPathString)
	}

	var availableSourceTypesVariant dbus.Variant
	if err := client.object.CallWithContext(
		ctx,
		propertiesInterface+".Get",
		0,
		screenCastInterface,
		"AvailableSourceTypes",
	).Store(&availableSourceTypesVariant); err != nil {
		return nil, fmt.Errorf("read available portal source types: %w", err)
	}
	var availableSourceTypes uint32
	if err := dbus.Store([]any{availableSourceTypesVariant}, &availableSourceTypes); err != nil {
		return nil, fmt.Errorf("decode available portal source types: %w", err)
	}
	if availableSourceTypes&sourceTypeMonitor == 0 {
		return nil, errors.New("desktop portal does not advertise monitor capture")
	}

	cursorMode := cursorModeHidden
	if cfg.CursorMode == CursorModeEmbedded {
		cursorMode = cursorModeEmbedded
	}

	var availableCursorModesVariant dbus.Variant
	if err := client.object.CallWithContext(
		ctx,
		propertiesInterface+".Get",
		0,
		screenCastInterface,
		"AvailableCursorModes",
	).Store(&availableCursorModesVariant); err != nil {
		return nil, fmt.Errorf("read available portal cursor modes: %w", err)
	}
	var availableCursorModes uint32
	if err := dbus.Store([]any{availableCursorModesVariant}, &availableCursorModes); err != nil {
		return nil, fmt.Errorf("decode available portal cursor modes: %w", err)
	}
	if availableCursorModes&cursorMode == 0 {
		return nil, fmt.Errorf("desktop portal does not advertise requested %s cursor mode", cfg.CursorMode)
	}

	if _, err := client.request(ctx, "SelectSources", sessionPath, map[string]dbus.Variant{
		"types":       dbus.MakeVariant(sourceTypeMonitor),
		"multiple":    dbus.MakeVariant(false),
		"cursor_mode": dbus.MakeVariant(cursorMode),
	}, sessionPath); err != nil {
		return nil, fmt.Errorf("select screen cast source: %w", err)
	}

	startResults, err := client.request(
		ctx,
		"Start",
		sessionPath,
		map[string]dbus.Variant{},
		sessionPath,
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("start screen cast session: %w", err)
	}

	streamsVariant, ok := startResults["streams"]
	if !ok {
		return nil, errors.New("start screen cast response did not contain streams")
	}
	var streams []struct {
		NodeID     uint32
		Properties map[string]dbus.Variant
	}
	if err := dbus.Store([]any{streamsVariant}, &streams); err != nil {
		return nil, fmt.Errorf("decode screen cast streams: %w", err)
	}
	if len(streams) != 1 {
		return nil, fmt.Errorf("portal returned %d streams for a single-source request", len(streams))
	}

	var pipeWireSerial uint64
	pipeWireSerialVariant, hasPipeWireSerial := streams[0].Properties["pipewire-serial"]
	if hasPipeWireSerial {
		if err := dbus.Store([]any{pipeWireSerialVariant}, &pipeWireSerial); err != nil {
			return nil, fmt.Errorf("decode screen cast pipewire-serial: %w", err)
		}
	}

	if err := conn.RemoveMatchSignalContext(ctx, requestMatch...); err != nil {
		return nil, fmt.Errorf("unsubscribe from portal request responses: %w", err)
	}
	requestMatchAdded = false

	var remoteFD dbus.UnixFD
	if err := client.object.CallWithContext(
		ctx,
		screenCastInterface+".OpenPipeWireRemote",
		0,
		sessionPath,
		map[string]dbus.Variant{},
	).Store(&remoteFD); err != nil {
		return nil, fmt.Errorf("open portal PipeWire remote: %w", err)
	}
	if remoteFD < 0 {
		return nil, fmt.Errorf("portal returned invalid PipeWire file descriptor %d", remoteFD)
	}

	remote = os.NewFile(uintptr(remoteFD), "xdg-desktop-portal-pipewire")
	if remote == nil {
		return nil, fmt.Errorf("open portal PipeWire file descriptor %d", remoteFD)
	}

	session := &Session{
		conn:   conn,
		path:   sessionPath,
		remote: remote,
		stream: streamMetadata{
			nodeID:            streams[0].NodeID,
			pipeWireSerial:    pipeWireSerial,
			hasPipeWireSerial: hasPipeWireSerial,
			properties:        streams[0].Properties,
		},
		signals:      signals,
		sessionMatch: sessionMatch,
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
	go session.watch()

	return session, nil
}

// AcquireStream duplicates the portal PipeWire remote for one media pipeline.
// The caller owns the returned Stream and must close it.
func (s *Session) AcquireStream() (*Stream, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if s.closing {
		if s.err != nil {
			return nil, s.err
		}
		return nil, ErrSessionClosed
	}

	duplicateFD, err := unix.FcntlInt(s.remote.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("duplicate portal PipeWire file descriptor: %w", err)
	}
	duplicate := os.NewFile(uintptr(duplicateFD), "xdg-desktop-portal-pipewire-pipeline")
	if duplicate == nil {
		_ = unix.Close(duplicateFD)
		return nil, fmt.Errorf("open duplicated portal PipeWire file descriptor %d", duplicateFD)
	}

	return &Stream{
		PipeWireFD:        int(duplicate.Fd()),
		NodeID:            s.stream.nodeID,
		PipeWireSerial:    s.stream.pipeWireSerial,
		HasPipeWireSerial: s.stream.hasPipeWireSerial,
		Properties:        s.stream.properties,
		remote:            duplicate,
	}, nil
}

// Done closes when the portal closes the session or Close finishes.
func (s *Session) Done() <-chan struct{} {
	return s.done
}

// Err reports an unexpected portal closure or cleanup failure.
func (s *Session) Err() error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.err
}

// Close closes the portal session, PipeWire remote, and D-Bus connection.
func (s *Session) Close() error {
	s.finish(true, nil)
	return s.Err()
}

func (s *Session) watch() {
	for {
		select {
		case <-s.stop:
			return
		case signal, ok := <-s.signals:
			if !ok {
				s.finish(false, errors.New("desktop portal D-Bus connection closed"))
				return
			}
			if signal.Path == s.path && signal.Name == sessionClosedSignal {
				s.finish(false, ErrSessionClosed)
				return
			}
		}
	}
}

func (s *Session) finish(closePortal bool, cause error) {
	s.closeOnce.Do(func() {
		s.stateMu.Lock()
		s.closing = true
		s.err = errors.Join(s.err, cause)
		s.stateMu.Unlock()

		close(s.stop)

		var cleanupErr error
		if closePortal {
			cleanupErr = errors.Join(cleanupErr, closePortalObject(s.conn, s.path, sessionInterface))
		}
		cleanupErr = errors.Join(cleanupErr, s.remote.Close())

		cleanupCtx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
		defer cancel()
		cleanupErr = errors.Join(cleanupErr, s.conn.RemoveMatchSignalContext(cleanupCtx, s.sessionMatch...))

		s.conn.RemoveSignal(s.signals)
		cleanupErr = errors.Join(cleanupErr, s.conn.Close())

		s.stateMu.Lock()
		s.err = errors.Join(s.err, cleanupErr)
		s.stateMu.Unlock()
		close(s.done)
	})
}

type portalClient struct {
	conn    *dbus.Conn
	object  dbus.BusObject
	signals chan *dbus.Signal
}

func (c portalClient) request(
	ctx context.Context,
	method string,
	sessionPath dbus.ObjectPath,
	options map[string]dbus.Variant,
	args ...any,
) (map[string]dbus.Variant, error) {
	token, err := portalToken()
	if err != nil {
		return nil, fmt.Errorf("create request token: %w", err)
	}
	options["handle_token"] = dbus.MakeVariant(token)

	sender := strings.TrimPrefix(c.conn.Names()[0], ":")
	sender = strings.ReplaceAll(sender, ".", "_")
	expectedPath := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + sender + "/" + token)

	callArgs := append(append([]any{}, args...), options)
	var requestPath dbus.ObjectPath
	if err := c.object.CallWithContext(
		ctx,
		screenCastInterface+"."+method,
		0,
		callArgs...,
	).Store(&requestPath); err != nil {
		if ctx.Err() != nil {
			return nil, errors.Join(ctx.Err(), closePortalObject(c.conn, expectedPath, requestInterface))
		}
		return nil, err
	}
	if !requestPath.IsValid() {
		return nil, fmt.Errorf("portal returned invalid request object path %q", requestPath)
	}

	for {
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), closePortalObject(c.conn, requestPath, requestInterface))
		case signal, ok := <-c.signals:
			if !ok {
				return nil, errors.New("desktop portal D-Bus connection closed")
			}
			if sessionPath.IsValid() && signal.Path == sessionPath && signal.Name == sessionClosedSignal {
				return nil, ErrSessionClosed
			}
			if signal.Path != requestPath || signal.Name != requestResponseSignal {
				continue
			}

			var response uint32
			var results map[string]dbus.Variant
			if err := dbus.Store(signal.Body, &response, &results); err != nil {
				return nil, fmt.Errorf("decode portal request response: %w", err)
			}

			switch response {
			case 0:
				return results, nil
			case 1:
				return nil, ErrRequestCancelled
			case 2:
				return nil, errors.New("desktop portal request failed")
			default:
				return nil, fmt.Errorf("desktop portal returned unknown response code %d", response)
			}
		}
	}
}

func portalToken() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "webdesktop_" + hex.EncodeToString(random), nil
}

func closePortalObject(conn *dbus.Conn, path dbus.ObjectPath, iface string) error {
	ctx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
	defer cancel()
	return conn.Object(portalDestination, path).CallWithContext(ctx, iface+".Close", 0).Store()
}
