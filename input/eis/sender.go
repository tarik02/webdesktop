//go:build linux

// Package eis sends emulated input through a libei sender context.
package eis

/*
#cgo pkg-config: libei-1.0
#include "bridge.h"
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
	"unsafe"

	remoteinput "github.com/tarik02/webdesktop/input"

	"golang.org/x/sys/unix"
)

const dispatchPollInterval = 100 * time.Millisecond

var (
	ErrClosed   = errors.New("EIS sender is closed")
	ErrNotReady = errors.New("EIS device is not ready")
)

// Config controls the capabilities bound by the sender.
type Config struct {
	Name      string
	Pointer   bool
	Keyboard  bool
	MappingID string
}

// Region is one active absolute pointer region advertised by EIS.
type Region struct {
	X         uint32
	Y         uint32
	Width     uint32
	Height    uint32
	MappingID string
}

type device struct {
	handle *C.struct_ei_device
	active bool
}

type deviceRegion struct {
	device *C.struct_ei_device
	Region
}

// Sender owns one libei sender context and the backend file descriptor.
type Sender struct {
	cfg Config
	ei  *C.struct_ei

	mu        sync.Mutex
	devices   []device
	connected bool
	closed    bool
	sequence  uint32
	reset     uint64
	err       error

	stop      chan struct{}
	done      chan struct{}
	changes   chan struct{}
	closeOnce sync.Once
}

// New transfers ownership of backendFD to a libei sender context.
func New(backendFD int, cfg Config) (*Sender, error) {
	if backendFD < 0 {
		return nil, errors.New("EIS backend file is required")
	}
	if !cfg.Pointer && !cfg.Keyboard {
		_ = unix.Close(backendFD)
		return nil, errors.New("EIS sender requires pointer or keyboard")
	}
	if cfg.Name == "" {
		_ = unix.Close(backendFD)
		return nil, errors.New("EIS sender name is required")
	}

	ei := C.ei_new_sender(nil)
	if ei == nil {
		_ = unix.Close(backendFD)
		return nil, errors.New("create libei sender")
	}
	C.ei_log_set_priority(ei, C.EI_LOG_PRIORITY_ERROR)
	name := C.CString(cfg.Name)
	C.ei_configure_name(ei, name)
	C.free(unsafe.Pointer(name))

	if result := C.ei_setup_backend_fd(ei, C.int(backendFD)); result < 0 {
		C.ei_unref(ei)
		return nil, fmt.Errorf("set up libei backend fd: %d", int(result))
	}

	sender := &Sender{
		cfg:     cfg,
		ei:      ei,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		changes: make(chan struct{}, 1),
	}
	go sender.run()
	return sender, nil
}

// Done closes after the EIS connection stops.
func (s *Sender) Done() <-chan struct{} {
	return s.done
}

// Changes receives a signal when connection, device, or region state changes.
func (s *Sender) Changes() <-chan struct{} {
	return s.changes
}

// Status returns a snapshot of the sender state.
func (s *Sender) Status() remoteinput.SenderStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

// PointerAbsolute maps normalized coordinates through the active EIS regions.
func (s *Sender) PointerAbsolute(x, y float64) error {
	if math.IsNaN(x) || math.IsInf(x, 0) || x < 0 || x > 1 ||
		math.IsNaN(y) || math.IsInf(y, 0) || y < 0 || y > 1 {
		return errors.New("absolute pointer coordinates must be finite and between 0 and 1")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	region, mappedX, mappedY, err := s.absoluteRegionLocked(x, y)
	if err != nil {
		return err
	}
	C.ei_device_pointer_motion_absolute(region.device, C.double(mappedX), C.double(mappedY))
	C.ei_device_frame(region.device, C.ei_now(s.ei))
	return nil
}

// PointerRelative sends relative pointer motion.
func (s *Sender) PointerRelative(dx, dy float64) error {
	if math.IsNaN(dx) || math.IsInf(dx, 0) || math.IsNaN(dy) || math.IsInf(dy, 0) {
		return errors.New("relative pointer deltas must be finite")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	device := s.deviceLocked(C.EI_DEVICE_CAP_POINTER)
	if device == nil {
		return fmt.Errorf("%w: relative pointer", ErrNotReady)
	}
	C.ei_device_pointer_motion(device, C.double(dx), C.double(dy))
	C.ei_device_frame(device, C.ei_now(s.ei))
	return nil
}

// Button sends one Linux input button transition.
func (s *Sender) Button(code uint32, pressed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	device := s.deviceLocked(C.EI_DEVICE_CAP_BUTTON)
	if device == nil {
		return fmt.Errorf("%w: pointer button", ErrNotReady)
	}
	C.ei_device_button_button(device, C.uint32_t(code), C.bool(pressed))
	C.ei_device_frame(device, C.ei_now(s.ei))
	return nil
}

// Scroll sends continuous deltas and optional axis stop events in one frame.
func (s *Sender) Scroll(horizontal, vertical float64, stopHorizontal, stopVertical bool) error {
	if math.IsNaN(horizontal) ||
		math.IsInf(horizontal, 0) ||
		math.IsNaN(vertical) ||
		math.IsInf(vertical, 0) {
		return errors.New("scroll deltas must be finite")
	}
	if horizontal == 0 && vertical == 0 && !stopHorizontal && !stopVertical {
		return errors.New("scroll requires a delta or an axis stop")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	device := s.deviceLocked(C.EI_DEVICE_CAP_SCROLL)
	if device == nil {
		return fmt.Errorf("%w: pointer scroll", ErrNotReady)
	}
	if horizontal != 0 || vertical != 0 {
		C.ei_device_scroll_delta(device, C.double(horizontal), C.double(vertical))
	}
	if stopHorizontal || stopVertical {
		C.ei_device_scroll_stop(device, C.bool(stopHorizontal), C.bool(stopVertical))
	}
	C.ei_device_frame(device, C.ei_now(s.ei))
	return nil
}

// KeyboardKey sends one Linux evdev key transition.
func (s *Sender) KeyboardKey(keycode uint32, pressed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	device := s.deviceLocked(C.EI_DEVICE_CAP_KEYBOARD)
	if device == nil {
		return fmt.Errorf("%w: keyboard", ErrNotReady)
	}
	C.ei_device_keyboard_key(device, C.uint32_t(keycode), C.bool(pressed))
	C.ei_device_frame(device, C.ei_now(s.ei))
	return nil
}

// Close stops dispatch, ends emulation, and releases the libei context.
func (s *Sender) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Sender) run() {
	defer close(s.done)

	fd := int32(C.ei_get_fd(s.ei))
	pollFDs := []unix.PollFd{{Fd: fd, Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR}}
	stopping := false

	for !stopping {
		select {
		case <-s.stop:
			stopping = true
			continue
		default:
		}

		count, err := unix.Poll(pollFDs, int(dispatchPollInterval.Milliseconds()))
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			s.fail(fmt.Errorf("poll EIS fd: %w", err))
			break
		}
		if count == 0 {
			continue
		}

		s.mu.Lock()
		C.ei_dispatch(s.ei)
		for {
			event := C.ei_get_event(s.ei)
			if event == nil {
				break
			}
			stopping = s.handleEventLocked(event) || stopping
			C.ei_event_unref(event)
		}
		s.notifyLocked()
		s.mu.Unlock()

		if pollFDs[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			s.fail(errors.New("EIS connection closed"))
			break
		}
	}

	s.mu.Lock()
	for i := range s.devices {
		if s.devices[i].active {
			C.ei_device_stop_emulating(s.devices[i].handle)
		}
		C.ei_device_unref(s.devices[i].handle)
	}
	s.devices = nil
	s.connected = false
	s.closed = true
	if s.ei != nil {
		C.ei_unref(s.ei)
		s.ei = nil
	}
	s.notifyLocked()
	s.mu.Unlock()
}

func (s *Sender) handleEventLocked(event *C.struct_ei_event) bool {
	switch C.ei_event_get_type(event) {
	case C.EI_EVENT_CONNECT:
		s.connected = true
	case C.EI_EVENT_DISCONNECT:
		s.connected = false
		s.reset++
		if s.err == nil {
			s.err = errors.New("EIS disconnected")
		}
		return true
	case C.EI_EVENT_SEAT_ADDED:
		seat := C.ei_event_get_seat(event)
		if seat != nil {
			C.webdesktop_ei_bind_capabilities(seat, C.bool(s.cfg.Pointer), C.bool(s.cfg.Keyboard))
		}
	case C.EI_EVENT_DEVICE_ADDED:
		s.addDeviceLocked(C.ei_event_get_device(event), false)
	case C.EI_EVENT_DEVICE_RESUMED:
		device := s.addDeviceLocked(C.ei_event_get_device(event), true)
		if device != nil && !device.active {
			s.sequence++
			C.ei_device_start_emulating(device.handle, C.uint32_t(s.sequence))
			device.active = true
		}
	case C.EI_EVENT_DEVICE_PAUSED:
		if device := s.findDeviceLocked(C.ei_event_get_device(event)); device != nil {
			device.active = false
			s.reset++
		}
	case C.EI_EVENT_DEVICE_REMOVED:
		s.reset++
		s.removeDeviceLocked(C.ei_event_get_device(event))
	}
	return false
}

func (s *Sender) addDeviceLocked(handle *C.struct_ei_device, active bool) *device {
	if handle == nil {
		return nil
	}
	if existing := s.findDeviceLocked(handle); existing != nil {
		if active && !existing.active {
			s.sequence++
			C.ei_device_start_emulating(existing.handle, C.uint32_t(s.sequence))
			existing.active = true
		}
		return existing
	}
	s.devices = append(s.devices, device{
		handle: C.ei_device_ref(handle),
		active: active,
	})
	added := &s.devices[len(s.devices)-1]
	if active {
		s.sequence++
		C.ei_device_start_emulating(added.handle, C.uint32_t(s.sequence))
	}
	return added
}

func (s *Sender) findDeviceLocked(handle *C.struct_ei_device) *device {
	for i := range s.devices {
		if s.devices[i].handle == handle {
			return &s.devices[i]
		}
	}
	return nil
}

func (s *Sender) removeDeviceLocked(handle *C.struct_ei_device) {
	for i := range s.devices {
		if s.devices[i].handle != handle {
			continue
		}
		C.ei_device_unref(s.devices[i].handle)
		s.devices = append(s.devices[:i], s.devices[i+1:]...)
		return
	}
}

func (s *Sender) deviceLocked(capability C.enum_ei_device_capability) *C.struct_ei_device {
	if s.closed || !s.connected {
		return nil
	}
	for i := len(s.devices) - 1; i >= 0; i-- {
		if s.devices[i].active &&
			C.ei_device_has_capability(s.devices[i].handle, capability) != C.bool(false) {
			return s.devices[i].handle
		}
	}
	return nil
}

func (s *Sender) regionsLocked() []deviceRegion {
	var regions []deviceRegion
	for i := range s.devices {
		if !s.devices[i].active ||
			C.ei_device_has_capability(s.devices[i].handle, C.EI_DEVICE_CAP_POINTER_ABSOLUTE) == C.bool(false) {
			continue
		}
		for index := 0; ; index++ {
			region := C.ei_device_get_region(s.devices[i].handle, C.size_t(index))
			if region == nil {
				break
			}
			mappingID := ""
			if value := C.ei_region_get_mapping_id(region); value != nil {
				mappingID = C.GoString(value)
			}
			if s.cfg.MappingID != "" && mappingID != s.cfg.MappingID {
				continue
			}
			regions = append(regions, deviceRegion{
				device: s.devices[i].handle,
				Region: Region{
					X:         uint32(C.ei_region_get_x(region)),
					Y:         uint32(C.ei_region_get_y(region)),
					Width:     uint32(C.ei_region_get_width(region)),
					Height:    uint32(C.ei_region_get_height(region)),
					MappingID: mappingID,
				},
			})
		}
	}
	return regions
}

func (s *Sender) absoluteRegionLocked(x, y float64) (deviceRegion, float64, float64, error) {
	if s.closed {
		return deviceRegion{}, 0, 0, ErrClosed
	}
	if !s.connected {
		return deviceRegion{}, 0, 0, fmt.Errorf("%w: absolute pointer", ErrNotReady)
	}
	regions := s.regionsLocked()
	if len(regions) == 0 {
		return deviceRegion{}, 0, 0, fmt.Errorf("%w: absolute pointer region", ErrNotReady)
	}
	if len(regions) == 1 || s.cfg.MappingID != "" {
		region := regions[len(regions)-1]
		return region,
			float64(region.X) + x*float64(max(region.Width, 1)-1),
			float64(region.Y) + y*float64(max(region.Height, 1)-1),
			nil
	}

	minX := regions[0].X
	minY := regions[0].Y
	maxX := regions[0].X + regions[0].Width
	maxY := regions[0].Y + regions[0].Height
	for _, region := range regions[1:] {
		minX = min(minX, region.X)
		minY = min(minY, region.Y)
		maxX = max(maxX, region.X+region.Width)
		maxY = max(maxY, region.Y+region.Height)
	}
	targetX := float64(minX) + x*float64(max(maxX-minX, 1)-1)
	targetY := float64(minY) + y*float64(max(maxY-minY, 1)-1)
	for _, region := range regions {
		if targetX >= float64(region.X) &&
			targetX < float64(region.X+region.Width) &&
			targetY >= float64(region.Y) &&
			targetY < float64(region.Y+region.Height) {
			return region, targetX, targetY, nil
		}
	}

	nearest := regions[0]
	nearestX, nearestY := clampToRegion(targetX, targetY, nearest.Region)
	nearestDistance := math.Hypot(targetX-nearestX, targetY-nearestY)
	for _, region := range regions[1:] {
		candidateX, candidateY := clampToRegion(targetX, targetY, region.Region)
		if distance := math.Hypot(targetX-candidateX, targetY-candidateY); distance < nearestDistance {
			nearest = region
			nearestX = candidateX
			nearestY = candidateY
			nearestDistance = distance
		}
	}
	return nearest, nearestX, nearestY, nil
}

func clampToRegion(x, y float64, region Region) (float64, float64) {
	maxX := float64(region.X + max(region.Width, 1) - 1)
	maxY := float64(region.Y + max(region.Height, 1) - 1)
	return min(max(x, float64(region.X)), maxX), min(max(y, float64(region.Y)), maxY)
}

func (s *Sender) statusLocked() remoteinput.SenderStatus {
	status := remoteinput.SenderStatus{
		Connected: s.connected && !s.closed,
		Reset:     s.reset,
		Err:       s.err,
	}
	regions := s.regionsLocked()
	if s.cfg.Pointer {
		status.Pointer = s.deviceLocked(C.EI_DEVICE_CAP_POINTER) != nil &&
			s.deviceLocked(C.EI_DEVICE_CAP_BUTTON) != nil &&
			s.deviceLocked(C.EI_DEVICE_CAP_SCROLL) != nil &&
			len(regions) > 0
	}
	if s.cfg.Keyboard {
		status.Keyboard = s.deviceLocked(C.EI_DEVICE_CAP_KEYBOARD) != nil
	}
	return status
}

func (s *Sender) fail(err error) {
	s.mu.Lock()
	s.err = errors.Join(s.err, err)
	s.connected = false
	s.reset++
	s.notifyLocked()
	s.mu.Unlock()
}

func (s *Sender) notifyLocked() {
	select {
	case s.changes <- struct{}{}:
	default:
	}
}
