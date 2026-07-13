package webrtc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

type clipboardReceive struct {
	id      string
	formats []clipboard.Format
	sizes   []int
	index   int
	written int
	timer   *time.Timer
}

func (p *peer) setClipboardChannel(channel *pion.DataChannel) {
	if !channel.Ordered() || channel.MaxPacketLifeTime() != nil || channel.MaxRetransmits() != nil {
		p.logger.Info("rejecting clipboard data channel without reliable ordered delivery")
		_ = channel.Close()
		return
	}

	p.clipboardMu.Lock()
	if p.clipboard != nil {
		p.clipboardMu.Unlock()
		p.logger.Info("rejecting duplicate clipboard data channel")
		_ = channel.Close()
		return
	}
	p.clipboard = channel
	p.clipboardBufferedLow = make(chan struct{}, 1)
	p.clipboardClosed = make(chan struct{})
	channel.SetBufferedAmountLowThreshold(256 * 1024)
	channel.OnBufferedAmountLow(func() {
		select {
		case p.clipboardBufferedLow <- struct{}{}:
		default:
		}
	})
	p.clipboardMu.Unlock()

	channel.OnOpen(func() {
		p.logger.Info("clipboard data channel opened")
		updates, unsubscribe := p.service.clipboard.Subscribe()
		go func() {
			defer unsubscribe()
			for {
				select {
				case <-p.ctx.Done():
					return
				case content, ok := <-updates:
					if !ok {
						return
					}
					if !p.service.input.Owns(p.id) {
						continue
					}
					if err := p.writeClipboardContent(channel, content); err != nil {
						p.logger.Debug("send desktop clipboard content", zap.Error(err))
						return
					}
				}
			}
		}()
	})
	channel.OnClose(func() {
		p.logger.Info("clipboard data channel closed")
		p.clipboardMu.Lock()
		if p.clipboardReceive != nil {
			p.clipboardReceive.timer.Stop()
			p.clipboardReceive = nil
		}
		p.clipboardMu.Unlock()
		p.clipboardCloseOnce.Do(func() { close(p.clipboardClosed) })
	})
	channel.OnError(func(err error) {
		p.logger.Debug("clipboard data channel error", zap.Error(err))
		p.clipboardMu.Lock()
		if p.clipboardReceive != nil {
			p.clipboardReceive.timer.Stop()
			p.clipboardReceive = nil
		}
		p.clipboardMu.Unlock()
		p.clipboardCloseOnce.Do(func() { close(p.clipboardClosed) })
	})
	channel.OnMessage(func(message pion.DataChannelMessage) {
		p.handleClipboardMessage(channel, message)
	})
}

func (p *peer) handleClipboardMessage(channel *pion.DataChannel, message pion.DataChannelMessage) {
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
		if p.clipboardReceive != nil {
			p.clipboardMu.Unlock()
			p.writeClipboardError(channel, header.ID, "transfer_in_progress", "another clipboard transfer is in progress")
			return
		}
		if receive.index == len(receive.formats) {
			p.clipboardMu.Unlock()
			p.finishClipboardReceive(channel, receive)
			return
		}
		receive.timer = time.AfterFunc(clipboardWriteTimeout, func() {
			p.clipboardMu.Lock()
			if p.clipboardReceive != receive {
				p.clipboardMu.Unlock()
				return
			}
			p.clipboardReceive = nil
			p.clipboardMu.Unlock()
			p.writeClipboardError(channel, receive.id, "transfer_timeout", "clipboard transfer timed out")
		})
		p.clipboardReceive = receive
		p.clipboardMu.Unlock()
		return
	}

	p.clipboardMu.Lock()
	receive := p.clipboardReceive
	if receive == nil {
		p.clipboardMu.Unlock()
		p.writeClipboardError(channel, "", "unexpected_binary", "clipboard data arrived without a header")
		return
	}
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
		p.clipboardReceive = nil
		p.clipboardMu.Unlock()
		p.writeClipboardError(channel, receive.id, "invalid_transfer", "clipboard payload exceeds declared sizes")
		return
	}
	complete := receive.index == len(receive.formats)
	if complete {
		receive.timer.Stop()
		p.clipboardReceive = nil
	}
	p.clipboardMu.Unlock()
	if complete {
		p.finishClipboardReceive(channel, receive)
	}
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

func (p *peer) finishClipboardReceive(channel *pion.DataChannel, receive *clipboardReceive) {
	if !p.service.input.Owns(p.id) {
		p.writeClipboardError(channel, receive.id, "input_not_owned", "peer does not own input")
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, clipboardWriteTimeout)
	err := p.service.clipboard.Set(ctx, clipboard.Content{Formats: receive.formats})
	cancel()
	if err != nil {
		p.writeClipboardError(channel, receive.id, "clipboard_write_failed", err.Error())
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
	channel := p.clipboard
	p.clipboardMu.Unlock()
	content, ok := p.service.clipboard.Latest()
	if channel == nil || channel.ReadyState() != pion.DataChannelStateOpen || !ok {
		return
	}
	if err := p.writeClipboardContent(channel, content); err != nil {
		p.logger.Debug("send current desktop clipboard content", zap.Error(err))
	}
}

func (p *peer) writeClipboardContent(channel *pion.DataChannel, content clipboard.Content) error {
	formats := make([]clipboardFormatHeader, len(content.Formats))
	for index, format := range content.Formats {
		formats[index] = clipboardFormatHeader{MIME: format.MIME, Size: len(format.Data)}
	}
	p.clipboardWriteMu.Lock()
	defer p.clipboardWriteMu.Unlock()
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
			if channel.BufferedAmount() > 512*1024 {
				select {
				case <-p.ctx.Done():
					return p.ctx.Err()
				case <-p.clipboardClosed:
					return errors.New("clipboard data channel closed")
				case <-p.clipboardBufferedLow:
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
