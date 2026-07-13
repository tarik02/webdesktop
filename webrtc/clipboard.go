package webrtc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	pion "github.com/pion/webrtc/v4"
	"github.com/tarik02/webdesktop/clipboard"
	"go.uber.org/zap"
)

const (
	clipboardVersion           = 1
	clipboardTypeContent       = "clipboard.content"
	clipboardTypeContentResult = "clipboard.content.result"
	clipboardTypeError         = "error"
	maxClipboardHeaderBytes    = 16 * 1024
	maxClipboardFormats        = 8
	maxClipboardBytes          = 32 * 1024 * 1024
	clipboardChunkBytes        = 64 * 1024
	clipboardWriteTimeout      = 15 * time.Second
)

type clipboardFormatHeader struct {
	MIME string `json:"mime_type"`
	Size int    `json:"size"`
}

type clipboardMessage struct {
	Version int                     `json:"version"`
	Type    string                  `json:"type"`
	ID      string                  `json:"id"`
	OK      bool                    `json:"ok,omitempty"`
	Formats []clipboardFormatHeader `json:"formats,omitempty"`
	Error   *protocolError          `json:"error,omitempty"`
}

type clipboardChannelState struct {
	channel         *pion.DataChannel
	ctx             context.Context
	bufferedLow     chan struct{}
	closed          chan struct{}
	cancel          context.CancelFunc
	closeOnce       sync.Once
	receive         *clipboardReceive
	receiveReserved bool
}

type clipboardReceive struct {
	id       string
	formats  []clipboard.Format
	sizes    []int
	index    int
	written  int
	timer    *time.Timer
	activity uint64
}

func (p *peer) setClipboardChannel(channel *pion.DataChannel) {
	if !channel.Ordered() || channel.MaxPacketLifeTime() != nil || channel.MaxRetransmits() != nil {
		p.logger.Info("rejecting clipboard data channel without reliable ordered delivery")
		_ = channel.Close()
		return
	}

	state := &clipboardChannelState{
		channel:     channel,
		bufferedLow: make(chan struct{}, 1),
		closed:      make(chan struct{}),
	}
	stateCtx, cancel := context.WithCancel(p.ctx)
	state.ctx = stateCtx
	state.cancel = cancel
	p.clipboardMu.Lock()
	previous := p.clipboardState
	p.clipboardState = state
	p.clipboardMu.Unlock()
	if previous != nil {
		previous.cancel()
		p.clipboardMu.Lock()
		if previous.receive != nil {
			previous.receive.timer.Stop()
			previous.receive = nil
		}
		previous.receiveReserved = false
		p.clipboardMu.Unlock()
		previous.closeOnce.Do(func() { close(previous.closed) })
		_ = previous.channel.Close()
	}
	channel.SetBufferedAmountLowThreshold(256 * 1024)
	channel.OnBufferedAmountLow(func() {
		select {
		case state.bufferedLow <- struct{}{}:
		default:
		}
	})

	cleanup := func() {
		cancel()
		p.clipboardMu.Lock()
		owned := p.clipboardState == state
		if owned {
			if state.receive != nil {
				state.receive.timer.Stop()
				state.receive = nil
			}
			state.receiveReserved = false
			p.clipboardState = nil
		}
		p.clipboardMu.Unlock()
		if owned {
			state.closeOnce.Do(func() { close(state.closed) })
		}
	}
	channel.OnOpen(func() {
		if !p.clipboardCurrent(state) {
			return
		}
		p.logger.Info("clipboard data channel opened")
		updates, unsubscribe := p.service.clipboard.Subscribe()
		go func() {
			defer unsubscribe()
			for {
				select {
				case <-stateCtx.Done():
					return
				case content, ok := <-updates:
					if !ok || !p.service.input.Owns(p.id) {
						if !ok {
							return
						}
						continue
					}
					if err := p.writeClipboardContent(channel, content, state); err != nil {
						p.logger.Debug("send desktop clipboard content", zap.Error(err))
						return
					}
				}
			}
		}()
	})
	channel.OnClose(func() { p.logger.Info("clipboard data channel closed"); cleanup() })
	channel.OnError(func(err error) { p.logger.Debug("clipboard data channel error", zap.Error(err)); cleanup() })
	channel.OnMessage(func(message pion.DataChannelMessage) { p.handleClipboardMessage(channel, message, state) })
}

func (p *peer) clipboardCurrent(state *clipboardChannelState) bool {
	p.clipboardMu.Lock()
	current := p.clipboardState == state
	p.clipboardMu.Unlock()
	if !current {
		return false
	}
	select {
	case <-state.ctx.Done():
		return false
	default:
		return true
	}
}

func (p *peer) handleClipboardMessage(channel *pion.DataChannel, message pion.DataChannelMessage, state *clipboardChannelState) {
	if !p.clipboardCurrent(state) {
		return
	}
	if !message.IsString && len(message.Data) > clipboardChunkBytes {
		p.writeClipboardError(channel, "", "invalid_transfer", "clipboard data chunk exceeds maximum size")
		return
	}
	if message.IsString {
		if len(message.Data) > maxClipboardHeaderBytes || !utf8.Valid(message.Data) {
			p.writeClipboardError(channel, "", "invalid_message", "invalid clipboard header")
			return
		}
		var header clipboardMessage
		decoder := json.NewDecoder(bytes.NewReader(message.Data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&header); err != nil {
			p.writeClipboardError(channel, "", "invalid_message", fmt.Sprintf("decode clipboard header: %v", err))
			return
		}
		var trailing struct{}
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			p.writeClipboardError(channel, header.ID, "invalid_message", "clipboard header contains multiple JSON values")
			return
		}
		if err := validateClipboardHeader(header); err != nil {
			p.writeClipboardError(channel, header.ID, "invalid_message", err.Error())
			return
		}
		if !p.service.input.Owns(p.id) {
			p.writeClipboardError(channel, header.ID, "input_not_owned", "peer does not own input")
			return
		}

		p.clipboardMu.Lock()
		if state.receive != nil || state.receiveReserved {
			p.clipboardMu.Unlock()
			p.writeClipboardError(channel, header.ID, "transfer_in_progress", "another clipboard transfer is in progress")
			return
		}
		state.receiveReserved = true
		p.clipboardMu.Unlock()

		receive := &clipboardReceive{
			id:      header.ID,
			formats: make([]clipboard.Format, len(header.Formats)),
			sizes:   make([]int, len(header.Formats)),
		}
		for index, format := range header.Formats {
			receive.formats[index] = clipboard.Format{MIME: format.MIME, Data: make([]byte, format.Size)}
			receive.sizes[index] = format.Size
		}
		for receive.index < len(receive.sizes) && receive.sizes[receive.index] == 0 {
			receive.index++
		}

		p.clipboardMu.Lock()
		if p.clipboardState != state || state.ctx.Err() != nil {
			state.receiveReserved = false
			p.clipboardMu.Unlock()
			return
		}
		if receive.index == len(receive.formats) {
			state.receiveReserved = false
			p.clipboardMu.Unlock()
			p.finishClipboardReceive(channel, state, receive)
			return
		}
		p.armClipboardReceiveTimer(channel, state, receive)
		state.receiveReserved = false
		state.receive = receive
		p.clipboardMu.Unlock()
		return
	}

	p.clipboardMu.Lock()
	receive := state.receive
	if receive == nil {
		p.clipboardMu.Unlock()
		p.writeClipboardError(channel, "", "unexpected_binary", "clipboard data arrived without a header")
		return
	}
	p.armClipboardReceiveTimer(channel, state, receive)
	remaining := message.Data
	for len(remaining) > 0 && receive.index < len(receive.formats) {
		available := receive.sizes[receive.index] - receive.written
		copied := min(available, len(remaining))
		copy(receive.formats[receive.index].Data[receive.written:], remaining[:copied])
		receive.written += copied
		remaining = remaining[copied:]
		if receive.written == receive.sizes[receive.index] {
			receive.index++
			receive.written = 0
			for receive.index < len(receive.sizes) && receive.sizes[receive.index] == 0 {
				receive.index++
			}
		}
	}
	if len(remaining) > 0 {
		receive.timer.Stop()
		state.receive = nil
		p.clipboardMu.Unlock()
		p.writeClipboardError(channel, receive.id, "invalid_transfer", "clipboard payload exceeds declared sizes")
		return
	}
	complete := receive.index == len(receive.formats)
	if complete {
		receive.timer.Stop()
		state.receive = nil
	}
	p.clipboardMu.Unlock()
	if complete {
		p.finishClipboardReceive(channel, state, receive)
	}
}

func (p *peer) armClipboardReceiveTimer(channel *pion.DataChannel, state *clipboardChannelState, receive *clipboardReceive) {
	if receive.timer != nil {
		receive.timer.Stop()
	}
	receive.activity++
	activity := receive.activity
	receive.timer = time.AfterFunc(clipboardWriteTimeout, func() {
		p.clipboardMu.Lock()
		if state.receive != receive || receive.activity != activity {
			p.clipboardMu.Unlock()
			return
		}
		state.receive = nil
		p.clipboardMu.Unlock()
		p.writeClipboardError(channel, receive.id, "transfer_timeout", "clipboard transfer timed out")
	})
}

func validateClipboardHeader(header clipboardMessage) error {
	if header.Version != clipboardVersion {
		return fmt.Errorf("clipboard version must be %d", clipboardVersion)
	}
	if header.Type != clipboardTypeContent {
		return errors.New("clipboard message type must be clipboard.content")
	}
	if header.ID == "" || len(header.ID) > 128 {
		return errors.New("clipboard id must contain between 1 and 128 bytes")
	}
	if len(header.Formats) == 0 || len(header.Formats) > maxClipboardFormats {
		return fmt.Errorf("clipboard content must contain between 1 and %d formats", maxClipboardFormats)
	}
	total := 0
	seen := make(map[string]struct{}, len(header.Formats))
	for _, format := range header.Formats {
		if clipboard.NormalizeMIME(format.MIME) != format.MIME {
			return fmt.Errorf("unsupported clipboard MIME type %q", format.MIME)
		}
		if _, ok := seen[format.MIME]; ok {
			return fmt.Errorf("duplicate clipboard MIME type %q", format.MIME)
		}
		if format.Size < 0 || format.Size > maxClipboardBytes {
			return fmt.Errorf("clipboard format %q has invalid size", format.MIME)
		}
		seen[format.MIME] = struct{}{}
		total += format.Size
		if total > maxClipboardBytes {
			return fmt.Errorf("clipboard content exceeds %d bytes", maxClipboardBytes)
		}
	}
	return nil
}

func (p *peer) finishClipboardReceive(channel *pion.DataChannel, state *clipboardChannelState, receive *clipboardReceive) {
	if !p.clipboardCurrent(state) {
		return
	}
	if !p.service.input.Owns(p.id) {
		p.writeClipboardError(channel, receive.id, "input_not_owned", "peer does not own input")
		return
	}
	ctx, cancel := context.WithTimeout(state.ctx, clipboardWriteTimeout)
	err := p.service.clipboard.Set(ctx, clipboard.Content{Formats: receive.formats})
	cancel()
	if err != nil {
		if p.clipboardCurrent(state) {
			p.writeClipboardError(channel, receive.id, "clipboard_write_failed", err.Error())
		}
		return
	}
	if !p.clipboardCurrent(state) {
		return
	}
	p.writeClipboardMessage(channel, clipboardMessage{
		Version: clipboardVersion,
		Type:    clipboardTypeContentResult,
		ID:      receive.id,
		OK:      true,
	})
}

func (p *peer) sendLatestClipboard() {
	p.clipboardMu.Lock()
	state := p.clipboardState
	p.clipboardMu.Unlock()
	content, ok := p.service.clipboard.Latest()
	if state == nil || !p.clipboardCurrent(state) || state.channel.ReadyState() != pion.DataChannelStateOpen || !ok {
		return
	}
	if err := p.writeClipboardContent(state.channel, content, state); err != nil {
		p.logger.Debug("send current desktop clipboard content", zap.Error(err))
	}
}

func (p *peer) writeClipboardContent(channel *pion.DataChannel, content clipboard.Content, state *clipboardChannelState) error {
	if !p.clipboardCurrent(state) {
		return errors.New("clipboard data channel is no longer current")
	}
	formats := make([]clipboardFormatHeader, len(content.Formats))
	for index, format := range content.Formats {
		formats[index] = clipboardFormatHeader{MIME: format.MIME, Size: len(format.Data)}
	}
	p.clipboardWriteMu.Lock()
	defer p.clipboardWriteMu.Unlock()
	if !p.clipboardCurrent(state) {
		return errors.New("clipboard data channel is no longer current")
	}
	p.clipboardSequence++
	header, err := json.Marshal(clipboardMessage{
		Version: clipboardVersion,
		Type:    clipboardTypeContent,
		ID:      fmt.Sprintf("server-%d", p.clipboardSequence),
		Formats: formats,
	})
	if err != nil {
		return err
	}
	if err := channel.SendText(string(header)); err != nil {
		return err
	}
	for _, format := range content.Formats {
		for offset := 0; offset < len(format.Data); offset += clipboardChunkBytes {
			if !p.clipboardCurrent(state) {
				return errors.New("clipboard data channel is no longer current")
			}
			if channel.BufferedAmount() > 512*1024 {
				select {
				case <-p.ctx.Done():
					return p.ctx.Err()
				case <-state.closed:
					return errors.New("clipboard data channel closed")
				case <-state.bufferedLow:
				}
			}
			end := min(offset+clipboardChunkBytes, len(format.Data))
			if err := channel.Send(format.Data[offset:end]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *peer) writeClipboardError(channel *pion.DataChannel, id, code, message string) {
	p.writeClipboardMessage(channel, clipboardMessage{
		Version: clipboardVersion,
		Type:    clipboardTypeError,
		ID:      id,
		Error:   &protocolError{Code: code, Message: message},
	})
}

func (p *peer) writeClipboardMessage(channel *pion.DataChannel, message clipboardMessage) {
	data, err := json.Marshal(message)
	if err != nil {
		return
	}
	p.clipboardWriteMu.Lock()
	defer p.clipboardWriteMu.Unlock()
	_ = channel.SendText(string(data))
}
