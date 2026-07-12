// Package input serializes remote input and owns the exclusive control lease.
package input

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/tarik02/webdesktop/input/eis"
)

var (
	ErrBusy                 = errors.New("input is owned by another peer")
	ErrDisabled             = errors.New("input is disabled")
	ErrPointerUnauthorized  = errors.New("pointer input was not authorized by the portal")
	ErrKeyboardUnauthorized = errors.New("keyboard input was not authorized by the portal")
	ErrNotReady             = errors.New("EIS input is not ready")
	ErrNotOwner             = errors.New("peer does not own input")
	ErrOverloaded           = errors.New("input queue is full")
	ErrClosed               = errors.New("input controller is closed")
)

// Config contains the implemented static input settings.
type Config struct {
	Enabled   bool
	Pointer   bool
	Keyboard  bool
	QueueSize int
}

// Authorization records the device classes granted by the portal.
type Authorization struct {
	Pointer  bool
	Keyboard bool
}

// Capabilities reports the input classes available to a lease holder.
type Capabilities struct {
	Pointer  bool
	Keyboard bool
}

// EventType identifies one input event.
type EventType uint8

const (
	EventPointerAbsolute EventType = iota + 1
	EventPointerRelative
	EventPointerButton
	EventPointerScroll
	EventKeyboardKey
)

// Event is one validated input transition or motion.
type Event struct {
	Sequence       uint64
	Type           EventType
	X              float64
	Y              float64
	DX             float64
	DY             float64
	ButtonCode     uint32
	Keycode        uint32
	Pressed        bool
	Horizontal     float64
	Vertical       float64
	StopHorizontal bool
	StopVertical   bool
}

type queuedEvent struct {
	owner      uint64
	generation uint64
	event      Event
}

// Controller owns one optional libei sender and one peer lease.
type Controller struct {
	cfg Config

	mu             sync.Mutex
	authorization  Authorization
	sender         *eis.Sender
	setupErr       error
	owner          uint64
	generation     uint64
	revoke         func(uint64, error)
	pressedKeys    map[uint32]struct{}
	pressedButtons map[uint32]struct{}
	closed         bool

	queue []queuedEvent
	wake  chan struct{}
	stop  chan struct{}
	done  chan struct{}
}

// New constructs the bounded input worker.
func New(cfg Config) (*Controller, error) {
	if cfg.QueueSize < 1 {
		return nil, errors.New("input queue size must be positive")
	}
	controller := &Controller{
		cfg:            cfg,
		pressedKeys:    make(map[uint32]struct{}),
		pressedButtons: make(map[uint32]struct{}),
		queue:          make([]queuedEvent, 0, cfg.QueueSize),
		wake:           make(chan struct{}, 1),
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
	go controller.run()
	return controller, nil
}

// Attach sets the portal authorization and transfers sender ownership.
func (c *Controller) Attach(authorization Authorization, sender *eis.Sender) error {
	if sender == nil {
		return errors.New("EIS sender is required")
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = sender.Close()
		return ErrClosed
	}
	if c.sender != nil {
		c.mu.Unlock()
		_ = sender.Close()
		return errors.New("input controller already has an EIS sender")
	}
	c.authorization = authorization
	c.sender = sender
	c.setupErr = nil
	c.mu.Unlock()

	go c.watchSender(sender)
	return nil
}

// SetUnavailable records portal authorization when the sender cannot start.
func (c *Controller) SetUnavailable(authorization Authorization, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authorization = authorization
	c.setupErr = err
}

// Acquire grants input to owner if the configured portal and EIS state is ready.
func (c *Controller) Acquire(owner uint64, revoke func(uint64, error)) (Capabilities, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return Capabilities{}, ErrClosed
	}
	if !c.cfg.Enabled {
		return Capabilities{}, ErrDisabled
	}
	if c.cfg.Pointer && !c.authorization.Pointer {
		return Capabilities{}, ErrPointerUnauthorized
	}
	if c.cfg.Keyboard && !c.authorization.Keyboard {
		return Capabilities{}, ErrKeyboardUnauthorized
	}
	if c.owner != 0 && c.owner != owner {
		return Capabilities{}, ErrBusy
	}
	if c.owner == owner {
		return Capabilities{Pointer: c.cfg.Pointer, Keyboard: c.cfg.Keyboard}, nil
	}
	if c.sender == nil {
		if c.setupErr != nil {
			return Capabilities{}, fmt.Errorf("%w: %v", ErrNotReady, c.setupErr)
		}
		return Capabilities{}, ErrNotReady
	}
	status := c.sender.Status()
	if !status.Connected ||
		(c.cfg.Pointer && !status.Pointer) ||
		(c.cfg.Keyboard && !status.Keyboard) {
		if status.Err != nil {
			return Capabilities{}, fmt.Errorf("%w: %v", ErrNotReady, status.Err)
		}
		return Capabilities{}, ErrNotReady
	}

	c.owner = owner
	c.generation++
	c.revoke = revoke
	clear(c.pressedKeys)
	clear(c.pressedButtons)
	return Capabilities{Pointer: c.cfg.Pointer, Keyboard: c.cfg.Keyboard}, nil
}

// Owns reports whether owner currently holds the input lease.
func (c *Controller) Owns(owner uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owner == owner
}

// Submit queues one event for the active lease.
func (c *Controller) Submit(owner uint64, event Event) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.owner != owner {
		c.mu.Unlock()
		return ErrNotOwner
	}
	switch event.Type {
	case EventKeyboardKey:
		if !c.cfg.Keyboard {
			c.mu.Unlock()
			return errors.New("keyboard input is disabled")
		}
	case EventPointerAbsolute, EventPointerRelative, EventPointerButton, EventPointerScroll:
		if !c.cfg.Pointer {
			c.mu.Unlock()
			return errors.New("pointer input is disabled")
		}
	default:
		c.mu.Unlock()
		return errors.New("input event type is invalid")
	}

	queued := queuedEvent{
		owner:      owner,
		generation: c.generation,
		event:      event,
	}
	if len(c.queue) > 0 {
		last := &c.queue[len(c.queue)-1]
		if last.owner == queued.owner && last.generation == queued.generation {
			switch {
			case last.event.Type == EventPointerAbsolute && event.Type == EventPointerAbsolute:
				last.event = event
				c.mu.Unlock()
				return nil
			case last.event.Type == EventPointerRelative && event.Type == EventPointerRelative:
				dx := last.event.DX + event.DX
				dy := last.event.DY + event.DY
				if !math.IsInf(dx, 0) && !math.IsInf(dy, 0) {
					last.event.Sequence = event.Sequence
					last.event.DX = dx
					last.event.DY = dy
					c.mu.Unlock()
					return nil
				}
			case last.event.Type == EventPointerScroll &&
				event.Type == EventPointerScroll &&
				!last.event.StopHorizontal &&
				!last.event.StopVertical &&
				!event.StopHorizontal &&
				!event.StopVertical:
				horizontal := last.event.Horizontal + event.Horizontal
				vertical := last.event.Vertical + event.Vertical
				if !math.IsInf(horizontal, 0) && !math.IsInf(vertical, 0) {
					last.event.Sequence = event.Sequence
					last.event.Horizontal = horizontal
					last.event.Vertical = vertical
					c.mu.Unlock()
					return nil
				}
			}
		}
	}
	if len(c.queue) < c.cfg.QueueSize {
		c.queue = append(c.queue, queued)
		select {
		case c.wake <- struct{}{}:
		default:
		}
		c.mu.Unlock()
		return nil
	}
	revoke := c.revokeLocked(ErrOverloaded)
	c.mu.Unlock()
	if revoke != nil {
		revoke(event.Sequence, ErrOverloaded)
	}
	return ErrOverloaded
}

// Release releases held state and drops owner's lease.
func (c *Controller) Release(owner uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owner != owner {
		return ErrNotOwner
	}
	c.revokeLocked(nil)
	return nil
}

// Revoke drops any lease and reports cause to its owner.
func (c *Controller) Revoke(cause error) {
	c.mu.Lock()
	revoke := c.revokeLocked(cause)
	c.mu.Unlock()
	if revoke != nil {
		revoke(0, cause)
	}
}

// Close releases held state before closing the libei sender.
func (c *Controller) Close() error {
	c.mu.Lock()
	if c.closed {
		sender := c.sender
		c.mu.Unlock()
		if sender != nil {
			return sender.Close()
		}
		return nil
	}
	c.closed = true
	c.revokeLocked(nil)
	sender := c.sender
	close(c.stop)
	c.mu.Unlock()

	<-c.done
	if sender != nil {
		return sender.Close()
	}
	return nil
}

func (c *Controller) run() {
	defer close(c.done)
	for {
		select {
		case <-c.stop:
			return
		case <-c.wake:
			for {
				c.mu.Lock()
				if len(c.queue) == 0 {
					c.mu.Unlock()
					break
				}
				queued := c.queue[0]
				c.queue = c.queue[1:]
				c.mu.Unlock()
				c.handleEvent(queued)
			}
		}
	}
}

func (c *Controller) handleEvent(queued queuedEvent) {
	c.mu.Lock()
	if c.closed || c.owner != queued.owner || c.generation != queued.generation || c.sender == nil {
		c.mu.Unlock()
		return
	}

	var err error
	switch queued.event.Type {
	case EventPointerAbsolute:
		err = c.sender.PointerAbsolute(queued.event.X, queued.event.Y)
	case EventPointerRelative:
		err = c.sender.PointerRelative(queued.event.DX, queued.event.DY)
	case EventPointerButton:
		_, pressed := c.pressedButtons[queued.event.ButtonCode]
		if pressed == queued.event.Pressed {
			c.mu.Unlock()
			return
		}
		err = c.sender.Button(queued.event.ButtonCode, queued.event.Pressed)
		if err == nil {
			if queued.event.Pressed {
				c.pressedButtons[queued.event.ButtonCode] = struct{}{}
			} else {
				delete(c.pressedButtons, queued.event.ButtonCode)
			}
		}
	case EventPointerScroll:
		err = c.sender.Scroll(
			queued.event.Horizontal,
			queued.event.Vertical,
			queued.event.StopHorizontal,
			queued.event.StopVertical,
		)
	case EventKeyboardKey:
		_, pressed := c.pressedKeys[queued.event.Keycode]
		if pressed == queued.event.Pressed {
			c.mu.Unlock()
			return
		}
		err = c.sender.KeyboardKey(queued.event.Keycode, queued.event.Pressed)
		if err == nil {
			if queued.event.Pressed {
				c.pressedKeys[queued.event.Keycode] = struct{}{}
			} else {
				delete(c.pressedKeys, queued.event.Keycode)
			}
		}
	}
	if err == nil {
		c.mu.Unlock()
		return
	}
	revoke := c.revokeLocked(err)
	c.mu.Unlock()
	if revoke != nil {
		revoke(queued.event.Sequence, err)
	}
}

func (c *Controller) watchSender(sender *eis.Sender) {
	reset := sender.Status().Reset
	for {
		select {
		case <-sender.Done():
			status := sender.Status()
			c.Revoke(errors.Join(ErrNotReady, status.Err))
			return
		case <-sender.Changes():
			status := sender.Status()
			resetChanged := status.Reset != reset
			reset = status.Reset
			if !status.Connected ||
				(c.cfg.Pointer && !status.Pointer) ||
				(c.cfg.Keyboard && !status.Keyboard) ||
				resetChanged {
				c.Revoke(errors.Join(ErrNotReady, status.Err))
			}
		case <-c.stop:
			return
		}
	}
}

func (c *Controller) revokeLocked(cause error) func(uint64, error) {
	if c.owner == 0 {
		return nil
	}
	c.releaseHeldLocked()
	c.owner = 0
	c.generation++
	c.queue = c.queue[:0]
	revoke := c.revoke
	c.revoke = nil
	if cause == nil {
		return nil
	}
	return revoke
}

func (c *Controller) releaseHeldLocked() {
	if c.sender != nil {
		keys := make([]uint32, 0, len(c.pressedKeys))
		for keycode := range c.pressedKeys {
			keys = append(keys, keycode)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, keycode := range keys {
			_ = c.sender.KeyboardKey(keycode, false)
		}

		buttons := make([]uint32, 0, len(c.pressedButtons))
		for code := range c.pressedButtons {
			buttons = append(buttons, code)
		}
		sort.Slice(buttons, func(i, j int) bool { return buttons[i] < buttons[j] })
		for _, code := range buttons {
			_ = c.sender.Button(code, false)
		}
	}
	clear(c.pressedKeys)
	clear(c.pressedButtons)
}
