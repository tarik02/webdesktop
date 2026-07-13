// Package input serializes remote input and owns control leases.
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
	Locking   bool
	Pointer   bool
	Keyboard  bool
	QueueSize int
}

// Authorization records the device classes granted by the portal.
type Authorization struct {
	Pointer  bool
	Keyboard bool
}

// Capabilities reports the input classes available to an input session.
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

type ownerState struct {
	generation     uint64
	revoke         func(uint64, error)
	queued         int
	pressedKeys    map[uint32]struct{}
	pressedButtons map[uint32]struct{}
}

type revocation struct {
	revoke   func(uint64, error)
	sequence uint64
	cause    error
}

// Controller owns one optional libei sender and its peer leases.
type Controller struct {
	cfg Config

	mu             sync.Mutex
	authorization  Authorization
	sender         *eis.Sender
	setupErr       error
	generation     uint64
	owners         map[uint64]*ownerState
	pressedKeys    map[uint32]uint64
	pressedButtons map[uint32]uint64
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
		owners:         make(map[uint64]*ownerState),
		pressedKeys:    make(map[uint32]uint64),
		pressedButtons: make(map[uint32]uint64),
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

// Acquire grants input access if the configured portal and EIS state is ready.
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
	if state, ok := c.owners[owner]; ok {
		state.revoke = revoke
		return Capabilities{Pointer: c.cfg.Pointer, Keyboard: c.cfg.Keyboard}, nil
	}
	if c.cfg.Locking && len(c.owners) != 0 {
		return Capabilities{}, ErrBusy
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

	c.generation++
	c.owners[owner] = &ownerState{
		generation:     c.generation,
		revoke:         revoke,
		pressedKeys:    make(map[uint32]struct{}),
		pressedButtons: make(map[uint32]struct{}),
	}
	return Capabilities{Pointer: c.cfg.Pointer, Keyboard: c.cfg.Keyboard}, nil
}

// Owns reports whether owner has active input access.
func (c *Controller) Owns(owner uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.owners[owner]
	return ok
}

// Submit queues one event for the owner's active input session.
func (c *Controller) Submit(owner uint64, event Event) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	state, ok := c.owners[owner]
	if !ok {
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
		generation: state.generation,
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
	if state.queued < c.cfg.QueueSize {
		c.queue = append(c.queue, queued)
		state.queued++
		select {
		case c.wake <- struct{}{}:
		default:
		}
		c.mu.Unlock()
		return nil
	}
	revoke := c.revokeOwnerLocked(owner, event.Sequence, ErrOverloaded)
	c.mu.Unlock()
	callRevocation(revoke)
	return ErrOverloaded
}

// Release releases held state and drops the owner's input session.
func (c *Controller) Release(owner uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.owners[owner]; !ok {
		return ErrNotOwner
	}
	c.revokeOwnerLocked(owner, 0, nil)
	return nil
}

// Revoke drops all input sessions and reports the cause to their owners.
func (c *Controller) Revoke(cause error) {
	c.mu.Lock()
	revocations := c.revokeAllLocked(cause)
	c.mu.Unlock()
	for _, revoke := range revocations {
		callRevocation(revoke)
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
	c.revokeAllLocked(nil)
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
				if state, ok := c.owners[queued.owner]; ok && state.generation == queued.generation {
					state.queued--
				}
				c.mu.Unlock()
				c.handleEvent(queued)
			}
		}
	}
}

func (c *Controller) handleEvent(queued queuedEvent) {
	c.mu.Lock()
	state, ok := c.owners[queued.owner]
	if c.closed || !ok || state.generation != queued.generation || c.sender == nil {
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
		_, pressed := state.pressedButtons[queued.event.ButtonCode]
		if pressed == queued.event.Pressed {
			c.mu.Unlock()
			return
		}
		count := c.pressedButtons[queued.event.ButtonCode]
		if queued.event.Pressed {
			if count == 0 {
				err = c.sender.Button(queued.event.ButtonCode, true)
			}
			if err == nil {
				state.pressedButtons[queued.event.ButtonCode] = struct{}{}
				c.pressedButtons[queued.event.ButtonCode] = count + 1
			}
		} else {
			if count == 1 {
				err = c.sender.Button(queued.event.ButtonCode, false)
			}
			if err == nil {
				delete(state.pressedButtons, queued.event.ButtonCode)
				if count == 1 {
					delete(c.pressedButtons, queued.event.ButtonCode)
				} else {
					c.pressedButtons[queued.event.ButtonCode] = count - 1
				}
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
		_, pressed := state.pressedKeys[queued.event.Keycode]
		if pressed == queued.event.Pressed {
			c.mu.Unlock()
			return
		}
		count := c.pressedKeys[queued.event.Keycode]
		if queued.event.Pressed {
			if count == 0 {
				err = c.sender.KeyboardKey(queued.event.Keycode, true)
			}
			if err == nil {
				state.pressedKeys[queued.event.Keycode] = struct{}{}
				c.pressedKeys[queued.event.Keycode] = count + 1
			}
		} else {
			if count == 1 {
				err = c.sender.KeyboardKey(queued.event.Keycode, false)
			}
			if err == nil {
				delete(state.pressedKeys, queued.event.Keycode)
				if count == 1 {
					delete(c.pressedKeys, queued.event.Keycode)
				} else {
					c.pressedKeys[queued.event.Keycode] = count - 1
				}
			}
		}
	}
	if err == nil {
		c.mu.Unlock()
		return
	}
	revocations := make([]*revocation, 0, len(c.owners))
	if revoke := c.revokeOwnerLocked(queued.owner, queued.event.Sequence, err); revoke != nil {
		revocations = append(revocations, revoke)
	}
	revocations = append(revocations, c.revokeAllLocked(err)...)
	c.mu.Unlock()
	for _, revoke := range revocations {
		callRevocation(revoke)
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

func (c *Controller) revokeOwnerLocked(owner, sequence uint64, cause error) *revocation {
	state, ok := c.owners[owner]
	if !ok {
		return nil
	}
	c.releaseHeldLocked(state)
	delete(c.owners, owner)
	queue := c.queue[:0]
	for _, queued := range c.queue {
		if queued.owner != owner {
			queue = append(queue, queued)
		}
	}
	c.queue = queue
	if cause == nil || state.revoke == nil {
		return nil
	}
	return &revocation{revoke: state.revoke, sequence: sequence, cause: cause}
}

func (c *Controller) revokeAllLocked(cause error) []*revocation {
	owners := make([]uint64, 0, len(c.owners))
	for owner := range c.owners {
		owners = append(owners, owner)
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i] < owners[j] })

	revocations := make([]*revocation, 0, len(owners))
	for _, owner := range owners {
		if revoke := c.revokeOwnerLocked(owner, 0, cause); revoke != nil {
			revocations = append(revocations, revoke)
		}
	}
	return revocations
}

func (c *Controller) releaseHeldLocked(state *ownerState) {
	keys := make([]uint32, 0, len(state.pressedKeys))
	for keycode := range state.pressedKeys {
		keys = append(keys, keycode)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, keycode := range keys {
		count := c.pressedKeys[keycode]
		if count == 1 {
			if c.sender != nil {
				_ = c.sender.KeyboardKey(keycode, false)
			}
			delete(c.pressedKeys, keycode)
		} else {
			c.pressedKeys[keycode] = count - 1
		}
	}

	buttons := make([]uint32, 0, len(state.pressedButtons))
	for code := range state.pressedButtons {
		buttons = append(buttons, code)
	}
	sort.Slice(buttons, func(i, j int) bool { return buttons[i] < buttons[j] })
	for _, code := range buttons {
		count := c.pressedButtons[code]
		if count == 1 {
			if c.sender != nil {
				_ = c.sender.Button(code, false)
			}
			delete(c.pressedButtons, code)
		} else {
			c.pressedButtons[code] = count - 1
		}
	}
}

func callRevocation(revoke *revocation) {
	if revoke != nil {
		revoke.revoke(revoke.sequence, revoke.cause)
	}
}
