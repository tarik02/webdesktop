// Package capture opens desktop capture streams through xdg-desktop-portal.
package capture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/tarik02/webdesktop/clipboard"
	"golang.org/x/sys/unix"
)

const (
	ApplicationID = "io.github.tarik02.webdesktop"

	SourceMonitor = "monitor"

	CursorModeHidden   = "hidden"
	CursorModeEmbedded = "embedded"
)

const (
	portalDestination      = "org.freedesktop.portal.Desktop"
	portalPath             = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	screenCastInterface    = "org.freedesktop.portal.ScreenCast"
	remoteDesktopInterface = "org.freedesktop.portal.RemoteDesktop"
	clipboardInterface     = "org.freedesktop.portal.Clipboard"
	requestInterface       = "org.freedesktop.portal.Request"
	sessionInterface       = "org.freedesktop.portal.Session"
	propertiesInterface    = "org.freedesktop.DBus.Properties"
	registryInterface      = "org.freedesktop.host.portal.Registry"

	requestResponseSignal   = requestInterface + ".Response"
	sessionClosedSignal     = sessionInterface + ".Closed"
	selectionChangedSignal  = clipboardInterface + ".SelectionOwnerChanged"
	selectionTransferSignal = clipboardInterface + ".SelectionTransfer"

	sourceTypeMonitor   uint32 = 1
	cursorModeHidden    uint32 = 1
	cursorModeEmbedded  uint32 = 2
	deviceTypeKeyboard  uint32 = 1
	deviceTypePointer   uint32 = 2
	persistUntilRevoked uint32 = 2

	portalCleanupTimeout = 2 * time.Second
	portalCallTimeout    = 10 * time.Second
	maxClipboardBytes    = 32 * 1024 * 1024
	restoreStateVersion  = 1
)

var (
	ErrRequestCancelled = errors.New("desktop capture authorization was cancelled")
	ErrSessionClosed    = errors.New("desktop portal screen cast session closed")
)

// Config controls the source requested from the desktop portal.
type Config struct {
	Source     string
	CursorMode string
	Input      InputConfig
	Clipboard  bool
}

// InputConfig controls whether the portal session also authorizes remote input.
type InputConfig struct {
	Enabled  bool
	Pointer  bool
	Keyboard bool
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
	if cfg.Input.Enabled && !cfg.Input.Pointer && !cfg.Input.Keyboard {
		errs = append(errs, errors.New("capture input requires pointer or keyboard"))
	}
	if cfg.Clipboard && (!cfg.Input.Enabled || !cfg.Input.Keyboard) {
		errs = append(errs, errors.New("clipboard synchronization requires remote keyboard input"))
	}

	return errors.Join(errs...)
}

// Stream owns one PipeWire remote for one media pipeline.
type Stream struct {
	PipeWireFD        int
	NodeID            uint32
	PipeWireSerial    uint64
	HasPipeWireSerial bool
	MappingID         string
	Properties        map[string]dbus.Variant

	remote    *os.File
	closeOnce sync.Once
	closeErr  error
}

// Close releases the per-pipeline PipeWire remote.
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
	mappingID         string
	properties        map[string]dbus.Variant
}

// InputAuthorization reports the device classes granted by the portal.
type InputAuthorization struct {
	Enabled  bool
	Pointer  bool
	Keyboard bool
}

// Session owns one portal session and its optional EIS remote.
type Session struct {
	conn             *dbus.Conn
	path             dbus.ObjectPath
	eisRemote        *os.File
	stream           streamMetadata
	input            InputAuthorization
	clipboard        bool
	restore          RestoreStatus
	signals          chan *dbus.Signal
	sessionMatch     []dbus.MatchOption
	clipboardMatch   []dbus.MatchOption
	clipboardChanges chan []string
	clipboardMu      sync.RWMutex
	clipboardContent clipboard.Content

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	stateMu   sync.Mutex
	closing   bool
	err       error
}

// RestoreStatus reports portal identity and restore-token rotation for this session.
type RestoreStatus struct {
	ApplicationID  string
	StatePath      string
	TokenAttempted bool
	TokenRotated   bool
}

// Open runs the xdg-desktop-portal capture flow and returns one monitor stream.
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
	clipboardMatch := []dbus.MatchOption{
		dbus.WithMatchSender(portalDestination),
		dbus.WithMatchInterface(clipboardInterface),
	}

	requestMatchAdded := false
	sessionMatchAdded := false
	clipboardMatchAdded := false
	var sessionPath dbus.ObjectPath
	var eisRemote *os.File

	defer func() {
		if err == nil {
			return
		}

		var cleanupErr error
		if sessionPath.IsValid() {
			cleanupErr = errors.Join(cleanupErr, closePortalObject(conn, sessionPath, sessionInterface))
		}
		if eisRemote != nil {
			cleanupErr = errors.Join(cleanupErr, eisRemote.Close())
		}

		cleanupCtx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
		defer cancel()
		if requestMatchAdded {
			cleanupErr = errors.Join(cleanupErr, conn.RemoveMatchSignalContext(cleanupCtx, requestMatch...))
		}
		if sessionMatchAdded {
			cleanupErr = errors.Join(cleanupErr, conn.RemoveMatchSignalContext(cleanupCtx, sessionMatch...))
		}
		if clipboardMatchAdded {
			cleanupErr = errors.Join(cleanupErr, conn.RemoveMatchSignalContext(cleanupCtx, clipboardMatch...))
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
	if cfg.Clipboard {
		if err = conn.AddMatchSignalContext(ctx, clipboardMatch...); err != nil {
			return nil, fmt.Errorf("subscribe to portal clipboard signals: %w", err)
		}
		clipboardMatchAdded = true
	}

	client := portalClient{
		conn:    conn,
		object:  conn.Object(portalDestination, portalPath),
		signals: signals,
	}
	if err := client.object.CallWithContext(
		ctx,
		registryInterface+".Register",
		0,
		ApplicationID,
		map[string]dbus.Variant{},
	).Err; err != nil {
		return nil, fmt.Errorf("register portal application identity %q: %w", ApplicationID, err)
	}

	restoreToken, restorePath, tokenAttempted, err := loadRestoreToken(cfg)
	if err != nil {
		return nil, fmt.Errorf("load portal restore token: %w", err)
	}

	sessionToken, err := portalToken()
	if err != nil {
		return nil, fmt.Errorf("create portal session token: %w", err)
	}
	createInterface := screenCastInterface
	if cfg.Input.Enabled {
		createInterface = remoteDesktopInterface
	}
	createResults, err := client.request(ctx, createInterface, "CreateSession", "", map[string]dbus.Variant{
		"session_handle_token": dbus.MakeVariant(sessionToken),
	})
	if err != nil {
		return nil, fmt.Errorf("create portal session: %w", err)
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

	sourceOptions := map[string]dbus.Variant{
		"types":       dbus.MakeVariant(sourceTypeMonitor),
		"multiple":    dbus.MakeVariant(false),
		"cursor_mode": dbus.MakeVariant(cursorMode),
	}
	if !cfg.Input.Enabled {
		sourceOptions["persist_mode"] = dbus.MakeVariant(persistUntilRevoked)
		if restoreToken != "" {
			sourceOptions["restore_token"] = dbus.MakeVariant(restoreToken)
		}
	}
	if _, err := client.request(
		ctx,
		screenCastInterface,
		"SelectSources",
		sessionPath,
		sourceOptions,
		sessionPath,
	); err != nil {
		return nil, fmt.Errorf("select screen cast source: %w", err)
	}

	requestedDevices := uint32(0)
	if cfg.Input.Enabled {
		var remoteDesktopVersionVariant dbus.Variant
		if err := client.object.CallWithContext(
			ctx,
			propertiesInterface+".Get",
			0,
			remoteDesktopInterface,
			"version",
		).Store(&remoteDesktopVersionVariant); err != nil {
			return nil, fmt.Errorf("read remote desktop portal version: %w", err)
		}
		var remoteDesktopVersion uint32
		if err := dbus.Store([]any{remoteDesktopVersionVariant}, &remoteDesktopVersion); err != nil {
			return nil, fmt.Errorf("decode remote desktop portal version: %w", err)
		}
		if remoteDesktopVersion < 2 {
			return nil, fmt.Errorf("remote desktop portal version %d does not support ConnectToEIS", remoteDesktopVersion)
		}

		var availableDeviceTypesVariant dbus.Variant
		if err := client.object.CallWithContext(
			ctx,
			propertiesInterface+".Get",
			0,
			remoteDesktopInterface,
			"AvailableDeviceTypes",
		).Store(&availableDeviceTypesVariant); err != nil {
			return nil, fmt.Errorf("read available remote desktop device types: %w", err)
		}
		var availableDeviceTypes uint32
		if err := dbus.Store([]any{availableDeviceTypesVariant}, &availableDeviceTypes); err != nil {
			return nil, fmt.Errorf("decode available remote desktop device types: %w", err)
		}
		if cfg.Input.Pointer {
			requestedDevices |= deviceTypePointer
		}
		if cfg.Input.Keyboard {
			requestedDevices |= deviceTypeKeyboard
		}
		if missing := requestedDevices &^ availableDeviceTypes; missing != 0 {
			return nil, fmt.Errorf("desktop portal does not advertise requested input device types 0x%x", missing)
		}
		deviceOptions := map[string]dbus.Variant{
			"types":        dbus.MakeVariant(requestedDevices),
			"persist_mode": dbus.MakeVariant(persistUntilRevoked),
		}
		if restoreToken != "" {
			deviceOptions["restore_token"] = dbus.MakeVariant(restoreToken)
		}
		if _, err := client.request(
			ctx,
			remoteDesktopInterface,
			"SelectDevices",
			sessionPath,
			deviceOptions,
			sessionPath,
		); err != nil {
			return nil, fmt.Errorf("select remote desktop devices: %w", err)
		}

		if cfg.Clipboard {
			var clipboardVersionVariant dbus.Variant
			if err := client.object.CallWithContext(
				ctx,
				propertiesInterface+".Get",
				0,
				clipboardInterface,
				"version",
			).Store(&clipboardVersionVariant); err != nil {
				return nil, fmt.Errorf("read clipboard portal version: %w", err)
			}
			var clipboardVersion uint32
			if err := dbus.Store([]any{clipboardVersionVariant}, &clipboardVersion); err != nil {
				return nil, fmt.Errorf("decode clipboard portal version: %w", err)
			}
			if clipboardVersion < 1 {
				return nil, fmt.Errorf("clipboard portal version %d is not supported", clipboardVersion)
			}
			if err := client.object.CallWithContext(
				ctx,
				clipboardInterface+".RequestClipboard",
				0,
				sessionPath,
				map[string]dbus.Variant{},
			).Err; err != nil {
				return nil, fmt.Errorf("request portal clipboard access: %w", err)
			}
		}
	}

	startResults, err := client.request(
		ctx,
		createInterface,
		"Start",
		sessionPath,
		map[string]dbus.Variant{},
		sessionPath,
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("start screen cast session: %w", err)
	}
	tokenRotated := false
	if restoreTokenVariant, ok := startResults["restore_token"]; ok {
		var nextRestoreToken string
		if err := dbus.Store([]any{restoreTokenVariant}, &nextRestoreToken); err != nil {
			return nil, fmt.Errorf("decode portal restore token: %w", err)
		}
		if nextRestoreToken == "" {
			return nil, errors.New("portal returned an empty restore token")
		}
		if err := storeRestoreToken(restorePath, cfg, nextRestoreToken); err != nil {
			return nil, fmt.Errorf("store portal restore token: %w", err)
		}
		tokenRotated = true
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
	var mappingID string
	if mappingIDVariant, ok := streams[0].Properties["mapping_id"]; ok {
		if err := dbus.Store([]any{mappingIDVariant}, &mappingID); err != nil {
			return nil, fmt.Errorf("decode screen cast mapping_id: %w", err)
		}
	}

	authorizedDevices := uint32(0)
	if cfg.Input.Enabled {
		devicesVariant, ok := startResults["devices"]
		if !ok {
			return nil, errors.New("start remote desktop response did not contain devices")
		}
		if err := dbus.Store([]any{devicesVariant}, &authorizedDevices); err != nil {
			return nil, fmt.Errorf("decode authorized remote desktop devices: %w", err)
		}
	}
	clipboardEnabled := false
	if cfg.Clipboard {
		clipboardVariant, ok := startResults["clipboard_enabled"]
		if !ok {
			return nil, errors.New("start remote desktop response did not contain clipboard_enabled")
		}
		if err := dbus.Store([]any{clipboardVariant}, &clipboardEnabled); err != nil {
			return nil, fmt.Errorf("decode authorized clipboard state: %w", err)
		}
		if !clipboardEnabled {
			return nil, errors.New("desktop portal did not authorize clipboard access")
		}
	}

	if err := conn.RemoveMatchSignalContext(ctx, requestMatch...); err != nil {
		return nil, fmt.Errorf("unsubscribe from portal request responses: %w", err)
	}
	requestMatchAdded = false

	if cfg.Input.Enabled {
		var eisFD dbus.UnixFD
		if err := client.object.CallWithContext(
			ctx,
			remoteDesktopInterface+".ConnectToEIS",
			0,
			sessionPath,
			map[string]dbus.Variant{},
		).Store(&eisFD); err != nil {
			return nil, fmt.Errorf("connect portal session to EIS: %w", err)
		}
		if eisFD < 0 {
			return nil, fmt.Errorf("portal returned invalid EIS file descriptor %d", eisFD)
		}
		eisRemote = os.NewFile(uintptr(eisFD), "xdg-desktop-portal-eis")
		if eisRemote == nil {
			return nil, fmt.Errorf("open portal EIS file descriptor %d", eisFD)
		}
	}

	session := &Session{
		conn:      conn,
		path:      sessionPath,
		eisRemote: eisRemote,
		stream: streamMetadata{
			nodeID:            streams[0].NodeID,
			pipeWireSerial:    pipeWireSerial,
			hasPipeWireSerial: hasPipeWireSerial,
			mappingID:         mappingID,
			properties:        streams[0].Properties,
		},
		input: InputAuthorization{
			Enabled:  cfg.Input.Enabled,
			Pointer:  authorizedDevices&deviceTypePointer != 0,
			Keyboard: authorizedDevices&deviceTypeKeyboard != 0,
		},
		restore: RestoreStatus{
			ApplicationID:  ApplicationID,
			StatePath:      restorePath,
			TokenAttempted: tokenAttempted,
			TokenRotated:   tokenRotated,
		},
		signals:          signals,
		sessionMatch:     sessionMatch,
		clipboardMatch:   clipboardMatch,
		clipboardChanges: make(chan []string, 1),
		clipboard:        clipboardEnabled,
		stop:             make(chan struct{}),
		done:             make(chan struct{}),
	}
	go session.watch()

	return session, nil
}

// RestoreStatus reports whether this launch supplied and rotated a portal token.
func (s *Session) RestoreStatus() RestoreStatus {
	return s.restore
}

// AcquireStream opens an independent portal PipeWire remote for one media pipeline.
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

	ctx, cancel := context.WithTimeout(context.Background(), portalCallTimeout)
	defer cancel()
	var remoteFD dbus.UnixFD
	if err := s.conn.Object(portalDestination, portalPath).CallWithContext(
		ctx,
		screenCastInterface+".OpenPipeWireRemote",
		0,
		s.path,
		map[string]dbus.Variant{},
	).Store(&remoteFD); err != nil {
		return nil, fmt.Errorf("open portal PipeWire remote: %w", err)
	}
	if remoteFD < 0 {
		return nil, fmt.Errorf("portal returned invalid PipeWire file descriptor %d", remoteFD)
	}
	remote := os.NewFile(uintptr(remoteFD), "xdg-desktop-portal-pipewire-pipeline")
	if remote == nil {
		_ = unix.Close(int(remoteFD))
		return nil, fmt.Errorf("open portal PipeWire file descriptor %d", remoteFD)
	}

	return &Stream{
		PipeWireFD:        int(remote.Fd()),
		NodeID:            s.stream.nodeID,
		PipeWireSerial:    s.stream.pipeWireSerial,
		HasPipeWireSerial: s.stream.hasPipeWireSerial,
		MappingID:         s.stream.mappingID,
		Properties:        s.stream.properties,
		remote:            remote,
	}, nil
}

// AcquireEIS duplicates the portal EIS connection for one libei sender.
// The caller owns the returned file descriptor and must close or transfer it.
func (s *Session) AcquireEIS() (int, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if s.closing {
		if s.err != nil {
			return -1, s.err
		}
		return -1, ErrSessionClosed
	}
	if s.eisRemote == nil {
		return -1, errors.New("portal session does not have an EIS connection")
	}

	duplicateFD, err := unix.FcntlInt(s.eisRemote.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("duplicate portal EIS file descriptor: %w", err)
	}
	return duplicateFD, nil
}

// InputAuthorization reports the portal device selection.
func (s *Session) InputAuthorization() InputAuthorization {
	return s.input
}

// Changes reports MIME types offered by a new desktop clipboard owner.
func (s *Session) Changes() <-chan []string {
	return s.clipboardChanges
}

// Read transfers one MIME representation from the desktop clipboard.
func (s *Session) Read(ctx context.Context, mimeType string) ([]byte, error) {
	if !s.clipboard {
		return nil, errors.New("portal session does not have clipboard access")
	}
	var selectionFD dbus.UnixFD
	if err := s.conn.Object(portalDestination, portalPath).CallWithContext(
		ctx,
		clipboardInterface+".SelectionRead",
		0,
		s.path,
		mimeType,
	).Store(&selectionFD); err != nil {
		return nil, fmt.Errorf("read portal clipboard selection %q: %w", mimeType, err)
	}
	if selectionFD < 0 {
		return nil, fmt.Errorf("portal returned invalid clipboard file descriptor %d", selectionFD)
	}
	file := os.NewFile(uintptr(selectionFD), "xdg-desktop-portal-clipboard-read")
	if file == nil {
		_ = unix.Close(int(selectionFD))
		return nil, fmt.Errorf("open portal clipboard file descriptor %d", selectionFD)
	}
	stopCancel := context.AfterFunc(ctx, func() {
		_ = file.Close()
	})
	defer func() {
		stopCancel()
		_ = file.Close()
	}()

	data, err := io.ReadAll(io.LimitReader(file, maxClipboardBytes+1))
	if err != nil {
		return nil, fmt.Errorf("transfer portal clipboard selection %q: %w", mimeType, err)
	}
	if len(data) > maxClipboardBytes {
		return nil, fmt.Errorf("portal clipboard selection %q exceeds %d bytes", mimeType, maxClipboardBytes)
	}
	return data, nil
}

// Write makes content supplied by a remote peer the desktop clipboard selection.
func (s *Session) Write(ctx context.Context, content clipboard.Content) error {
	if !s.clipboard {
		return errors.New("portal session does not have clipboard access")
	}
	mimeTypes := make([]string, len(content.Formats))
	stored := clipboard.Content{Formats: make([]clipboard.Format, len(content.Formats))}
	for index, format := range content.Formats {
		mimeTypes[index] = format.MIME
		stored.Formats[index] = clipboard.Format{
			MIME: format.MIME,
			Data: append([]byte(nil), format.Data...),
		}
	}

	s.clipboardMu.Lock()
	s.clipboardContent = stored
	s.clipboardMu.Unlock()
	if err := s.conn.Object(portalDestination, portalPath).CallWithContext(
		ctx,
		clipboardInterface+".SetSelection",
		0,
		s.path,
		map[string]dbus.Variant{"mime_types": dbus.MakeVariant(mimeTypes)},
	).Err; err != nil {
		return fmt.Errorf("set portal clipboard selection: %w", err)
	}
	return nil
}

// MappingID identifies the EIS absolute region paired with the captured stream.
func (s *Session) MappingID() string {
	return s.stream.mappingID
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

// Close closes the portal session, EIS remote, and D-Bus connection.
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
			switch signal.Name {
			case selectionChangedSignal:
				var sessionPath dbus.ObjectPath
				var options map[string]dbus.Variant
				if err := dbus.Store(signal.Body, &sessionPath, &options); err != nil || sessionPath != s.path {
					continue
				}
				var sessionIsOwner bool
				if owner, ok := options["session_is_owner"]; ok {
					if err := dbus.Store([]any{owner}, &sessionIsOwner); err != nil {
						continue
					}
				}
				if sessionIsOwner {
					continue
				}
				var mimeTypes []string
				if offered, ok := options["mime_types"]; ok {
					if err := dbus.Store([]any{offered}, &mimeTypes); err != nil {
						continue
					}
				}
				select {
				case s.clipboardChanges <- mimeTypes:
				default:
					select {
					case <-s.clipboardChanges:
					default:
					}
					s.clipboardChanges <- mimeTypes
				}
			case selectionTransferSignal:
				var sessionPath dbus.ObjectPath
				var mimeType string
				var serial uint32
				if err := dbus.Store(signal.Body, &sessionPath, &mimeType, &serial); err != nil || sessionPath != s.path {
					continue
				}
				go s.writeSelection(mimeType, serial)
			}
		}
	}
}

func (s *Session) writeSelection(mimeType string, serial uint32) {
	s.clipboardMu.RLock()
	var data []byte
	found := false
	for _, format := range s.clipboardContent.Formats {
		if format.MIME == mimeType {
			data = append([]byte(nil), format.Data...)
			found = true
			break
		}
	}
	s.clipboardMu.RUnlock()

	success := false
	ctx, cancel := context.WithTimeout(context.Background(), portalCallTimeout)
	defer cancel()
	if found {
		var selectionFD dbus.UnixFD
		if err := s.conn.Object(portalDestination, portalPath).CallWithContext(
			ctx,
			clipboardInterface+".SelectionWrite",
			0,
			s.path,
			serial,
		).Store(&selectionFD); err == nil && selectionFD >= 0 {
			file := os.NewFile(uintptr(selectionFD), "xdg-desktop-portal-clipboard-write")
			if file == nil {
				_ = unix.Close(int(selectionFD))
			} else {
				stopCancel := context.AfterFunc(ctx, func() {
					_ = file.Close()
				})
				written := 0
				var writeErr error
				for written < len(data) {
					var count int
					count, writeErr = file.Write(data[written:])
					written += count
					if writeErr != nil {
						break
					}
					if count == 0 {
						writeErr = io.ErrShortWrite
						break
					}
				}
				stopCancel()
				closeErr := file.Close()
				success = written == len(data) && writeErr == nil && closeErr == nil
			}
		}
	}
	doneCtx, doneCancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
	defer doneCancel()
	_ = s.conn.Object(portalDestination, portalPath).CallWithContext(
		doneCtx,
		clipboardInterface+".SelectionWriteDone",
		0,
		s.path,
		serial,
		success,
	).Err
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
		if s.eisRemote != nil {
			cleanupErr = errors.Join(cleanupErr, s.eisRemote.Close())
		}

		cleanupCtx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
		defer cancel()
		cleanupErr = errors.Join(cleanupErr, s.conn.RemoveMatchSignalContext(cleanupCtx, s.sessionMatch...))
		if s.clipboard {
			cleanupErr = errors.Join(cleanupErr, s.conn.RemoveMatchSignalContext(cleanupCtx, s.clipboardMatch...))
		}

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
	iface string,
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
		iface+"."+method,
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

type restoreState struct {
	Version       int         `json:"version"`
	ApplicationID string      `json:"application_id"`
	Source        string      `json:"source"`
	ScreenShare   bool        `json:"screen_share"`
	Input         InputConfig `json:"input"`
	Clipboard     bool        `json:"clipboard"`
	RestoreToken  string      `json:"restore_token"`
}

func loadRestoreToken(cfg Config) (string, string, bool, error) {
	statePath, err := restoreTokenPath()
	if err != nil {
		return "", "", false, err
	}

	file, err := os.Open(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return "", statePath, false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	defer func() {
		_ = file.Close()
	}()

	info, err := file.Stat()
	if err != nil {
		return "", "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", "", false, errors.New("restore token state is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return "", "", false, fmt.Errorf("restore token state permissions are %04o, want 0600", info.Mode().Perm())
	}

	var state restoreState
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return "", "", false, err
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", "", false, errors.New("restore token state contains multiple JSON values")
		}
		return "", "", false, err
	}

	matches := state.Version == restoreStateVersion &&
		state.ApplicationID == ApplicationID &&
		state.Source == cfg.Source &&
		state.ScreenShare &&
		state.Input == cfg.Input &&
		state.Clipboard == cfg.Clipboard &&
		state.RestoreToken != ""
	if !matches {
		if err := os.Remove(statePath); err != nil {
			return "", "", false, fmt.Errorf("discard incompatible restore token state: %w", err)
		}
		return "", statePath, false, nil
	}
	return state.RestoreToken, statePath, true, nil
}

func storeRestoreToken(statePath string, cfg Config, token string) error {
	state := restoreState{
		Version:       restoreStateVersion,
		ApplicationID: ApplicationID,
		Source:        cfg.Source,
		ScreenShare:   true,
		Input:         cfg.Input,
		Clipboard:     cfg.Clipboard,
		RestoreToken:  token,
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	directory := filepath.Dir(statePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}

	file, err := os.CreateTemp(directory, ".portal-restore-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() {
		_ = os.Remove(tempPath)
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, statePath)
}

func restoreTokenPath() (string, error) {
	if stateHome := os.Getenv("XDG_STATE_HOME"); stateHome != "" {
		if !filepath.IsAbs(stateHome) {
			return "", errors.New("XDG_STATE_HOME must be an absolute path")
		}
		return filepath.Join(stateHome, "webdesktop", "portal-restore.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "webdesktop", "portal-restore.json"), nil
}

func closePortalObject(conn *dbus.Conn, path dbus.ObjectPath, iface string) error {
	ctx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
	defer cancel()
	return conn.Object(portalDestination, path).CallWithContext(ctx, iface+".Close", 0).Store()
}
