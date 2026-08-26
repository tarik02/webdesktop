package webrtc

import (
	"errors"
	"sync/atomic"

	pion "github.com/pion/webrtc/v4"
	"go.uber.org/zap"
)

const maxApplicationMessageBytes = 1024 * 1024

type applicationDataChannel struct {
	channel *pion.DataChannel
	opened  atomic.Bool
}

func (p *peer) setApplicationChannel(channel *pion.DataChannel, kind ApplicationChannel) {
	if p.service.cfg.ApplicationHandler == nil {
		_ = channel.Close()
		return
	}
	if !validApplicationChannelParameters(channel, kind) {
		p.logger.Info("rejecting application data channel with invalid delivery parameters",
			zap.String("label", channel.Label()),
		)
		_ = channel.Close()
		return
	}

	state := &applicationDataChannel{channel: channel}
	p.applicationMu.Lock()
	var occupied bool
	switch kind {
	case ApplicationChannelReliable:
		occupied = p.applicationReliable != nil
		if !occupied {
			p.applicationReliable = state
		}
	case ApplicationChannelRealtime:
		occupied = p.applicationRealtime != nil
		if !occupied {
			p.applicationRealtime = state
		}
	}
	p.applicationMu.Unlock()
	if occupied {
		p.logger.Info("rejecting duplicate application data channel", zap.String("label", channel.Label()))
		_ = channel.Close()
		return
	}

	channel.OnOpen(func() {
		if p.applicationChannelCurrent(kind, state) && state.opened.CompareAndSwap(false, true) {
			p.service.cfg.ApplicationHandler.ApplicationChannelOpened(p.service.peerInfo(p), kind)
		}
	})
	cleanup := func() {
		if p.detachApplicationChannel(kind, state) && state.opened.Load() {
			p.service.cfg.ApplicationHandler.ApplicationChannelClosed(p.service.peerInfo(p), kind)
		}
	}
	channel.OnClose(cleanup)
	channel.OnError(func(err error) {
		p.logger.Debug("application data channel error", zap.String("label", channel.Label()), zap.Error(err))
		cleanup()
	})
	channel.OnMessage(func(message pion.DataChannelMessage) {
		if !p.applicationChannelCurrent(kind, state) || len(message.Data) > maxApplicationMessageBytes {
			if len(message.Data) > maxApplicationMessageBytes {
				_ = channel.Close()
			}
			return
		}
		p.service.cfg.ApplicationHandler.ApplicationMessageReceived(
			p.service.peerInfo(p),
			kind,
			ApplicationMessage{Data: append([]byte(nil), message.Data...), IsString: message.IsString},
		)
	})
}

func validApplicationChannelParameters(channel *pion.DataChannel, kind ApplicationChannel) bool {
	switch kind {
	case ApplicationChannelReliable:
		return channel.Ordered() && channel.MaxPacketLifeTime() == nil && channel.MaxRetransmits() == nil
	case ApplicationChannelRealtime:
		maxRetransmits := channel.MaxRetransmits()
		return !channel.Ordered() && channel.MaxPacketLifeTime() == nil && maxRetransmits != nil && *maxRetransmits == 0
	default:
		return false
	}
}

func (p *peer) applicationChannelCurrent(kind ApplicationChannel, state *applicationDataChannel) bool {
	p.applicationMu.Lock()
	defer p.applicationMu.Unlock()
	if p.isClosing() {
		return false
	}
	switch kind {
	case ApplicationChannelReliable:
		return p.applicationReliable == state
	case ApplicationChannelRealtime:
		return p.applicationRealtime == state
	default:
		return false
	}
}

func (p *peer) detachApplicationChannel(kind ApplicationChannel, state *applicationDataChannel) bool {
	p.applicationMu.Lock()
	defer p.applicationMu.Unlock()
	switch kind {
	case ApplicationChannelReliable:
		if p.applicationReliable == state {
			p.applicationReliable = nil
			return true
		}
	case ApplicationChannelRealtime:
		if p.applicationRealtime == state {
			p.applicationRealtime = nil
			return true
		}
	}
	return false
}

func (p *peer) closeApplicationChannels() {
	p.applicationMu.Lock()
	reliable := p.applicationReliable
	realtime := p.applicationRealtime
	p.applicationReliable = nil
	p.applicationRealtime = nil
	p.applicationMu.Unlock()

	for _, item := range []struct {
		kind  ApplicationChannel
		state *applicationDataChannel
	}{
		{kind: ApplicationChannelReliable, state: reliable},
		{kind: ApplicationChannelRealtime, state: realtime},
	} {
		if item.state == nil {
			continue
		}
		_ = item.state.channel.Close()
		if item.state.opened.Load() {
			p.service.cfg.ApplicationHandler.ApplicationChannelClosed(p.service.peerInfo(p), item.kind)
		}
	}
}

func (p *peer) sendApplication(kind ApplicationChannel, message ApplicationMessage) error {
	if len(message.Data) > maxApplicationMessageBytes {
		return ErrApplicationMessageLarge
	}
	p.applicationMu.Lock()
	var state *applicationDataChannel
	switch kind {
	case ApplicationChannelReliable:
		state = p.applicationReliable
	case ApplicationChannelRealtime:
		state = p.applicationRealtime
	default:
		p.applicationMu.Unlock()
		return errors.New("unknown application channel")
	}
	p.applicationMu.Unlock()
	if state == nil || !state.opened.Load() || state.channel.ReadyState() != pion.DataChannelStateOpen {
		return ErrApplicationUnavailable
	}

	var err error
	switch kind {
	case ApplicationChannelReliable:
		p.applicationWriteReliableMu.Lock()
		err = sendApplicationMessage(state.channel, message)
		p.applicationWriteReliableMu.Unlock()
	case ApplicationChannelRealtime:
		p.applicationWriteRealtimeMu.Lock()
		err = sendApplicationMessage(state.channel, message)
		p.applicationWriteRealtimeMu.Unlock()
	}
	if err != nil {
		return err
	}
	return nil
}

func sendApplicationMessage(channel *pion.DataChannel, message ApplicationMessage) error {
	if message.IsString {
		return channel.SendText(string(message.Data))
	}
	return channel.Send(message.Data)
}
