// Package clipboard synchronizes desktop clipboard content with WebRTC peers.
package clipboard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	readTimeout     = 10 * time.Second
	maxContentBytes = 32 * 1024 * 1024
)

var (
	ErrDisabled = errors.New("clipboard synchronization is disabled")
	ErrNotReady = errors.New("clipboard portal is not ready")
)

// Format contains one clipboard MIME representation.
type Format struct {
	MIME string
	Data []byte
}

// Content is one clipboard selection with all synchronized representations.
type Content struct {
	Formats []Format
}

// Backend provides clipboard access for one authorized desktop session.
type Selection struct {
	Generation uint64
	MIMETypes  []string
}

type Backend interface {
	Changes() <-chan Selection
	CurrentGeneration() uint64
	Read(context.Context, uint64, string) ([]byte, error)
	Write(context.Context, Content) error
}

// Controller bridges one portal backend to all connected peers.
type Controller struct {
	enabled bool

	mu          sync.Mutex
	backend     Backend
	subscribers map[uint64]chan Content
	nextID      uint64
	latest      *Content
}

// New constructs a clipboard controller.
func New(enabled bool) *Controller {
	return &Controller{
		enabled:     enabled,
		subscribers: make(map[uint64]chan Content),
	}
}

// Enabled reports whether clipboard synchronization is configured.
func (c *Controller) Enabled() bool {
	return c.enabled
}

// Attach connects the controller to one authorized portal session.
func (c *Controller) Attach(ctx context.Context, backend Backend) error {
	if !c.enabled {
		return ErrDisabled
	}
	if backend == nil {
		return errors.New("clipboard backend is required")
	}

	c.mu.Lock()
	if c.backend != nil {
		c.mu.Unlock()
		return errors.New("clipboard controller already has a backend")
	}
	c.backend = backend
	c.latest = nil
	c.mu.Unlock()

	go c.run(ctx, backend)
	return nil
}

// Set makes content supplied by a peer the desktop clipboard selection.
func (c *Controller) Set(ctx context.Context, content Content) error {
	if !c.enabled {
		return ErrDisabled
	}
	if len(content.Formats) == 0 {
		return errors.New("clipboard content has no formats")
	}
	for _, format := range content.Formats {
		if NormalizeMIME(format.MIME) != format.MIME {
			return fmt.Errorf("unsupported clipboard MIME type %q", format.MIME)
		}
	}

	c.mu.Lock()
	backend := c.backend
	c.mu.Unlock()
	if backend == nil {
		return ErrNotReady
	}
	content = clone(content)
	if err := backend.Write(ctx, content); err != nil {
		return err
	}
	c.mu.Lock()
	if c.backend != backend {
		c.mu.Unlock()
		return ErrNotReady
	}
	latest := clone(content)
	c.latest = &latest
	c.mu.Unlock()
	return nil
}

// Latest returns the most recent desktop clipboard content.
func (c *Controller) Latest() (Content, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.latest == nil {
		return Content{}, false
	}
	return clone(*c.latest), true
}

// Subscribe returns desktop clipboard changes and a matching unsubscribe call.
func (c *Controller) Subscribe() (<-chan Content, func()) {
	updates := make(chan Content, 1)
	if !c.enabled {
		close(updates)
		return updates, func() {}
	}

	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.subscribers[id] = updates
	if c.latest != nil {
		updates <- clone(*c.latest)
	}
	c.mu.Unlock()

	return updates, func() {
		c.mu.Lock()
		if existing, ok := c.subscribers[id]; ok {
			delete(c.subscribers, id)
			close(existing)
		}
		c.mu.Unlock()
	}
}

func (c *Controller) run(ctx context.Context, backend Backend) {
	defer func() {
		c.mu.Lock()
		if c.backend == backend {
			c.backend = nil
			c.latest = nil
		}
		c.mu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case selection, ok := <-backend.Changes():
			if !ok {
				return
			}

			formats := make([]Format, 0, len(selection.MIMETypes))
			seen := make(map[string]struct{})
			total := 0
			for _, offered := range selection.MIMETypes {
				mimeType := NormalizeMIME(offered)
				if mimeType == "" {
					continue
				}
				if _, ok := seen[mimeType]; ok {
					continue
				}
				readCtx, cancel := context.WithTimeout(ctx, readTimeout)
				data, err := backend.Read(readCtx, selection.Generation, offered)
				cancel()
				if err != nil {
					continue
				}
				if total+len(data) > maxContentBytes {
					continue
				}
				total += len(data)
				seen[mimeType] = struct{}{}
				formats = append(formats, Format{MIME: mimeType, Data: data})
			}
			if backend.CurrentGeneration() != selection.Generation {
				continue
			}
			if len(formats) == 0 {
				continue
			}

			content := Content{Formats: formats}
			c.mu.Lock()
			latest := clone(content)
			c.latest = &latest
			for _, subscriber := range c.subscribers {
				select {
				case subscriber <- clone(content):
				default:
					select {
					case <-subscriber:
					default:
					}
					subscriber <- clone(content)
				}
			}
			c.mu.Unlock()
		}
	}
}

// NormalizeMIME returns the synchronized browser MIME type or an empty string.
func NormalizeMIME(value string) string {
	mimeType := strings.ToLower(strings.TrimSpace(strings.SplitN(value, ";", 2)[0]))
	switch mimeType {
	case "text/plain", "text/html", "image/png", "image/jpeg", "image/webp", "image/gif", "image/svg+xml":
		return mimeType
	default:
		return ""
	}
}

func clone(content Content) Content {
	formats := make([]Format, len(content.Formats))
	for index, format := range content.Formats {
		formats[index] = Format{
			MIME: format.MIME,
			Data: append([]byte(nil), format.Data...),
		}
	}
	return Content{Formats: formats}
}
