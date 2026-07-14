package webrtc

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	pion "github.com/pion/webrtc/v4"
	remoteinput "github.com/tarik02/webdesktop/input"
	"go.uber.org/zap"
)

func (p *peer) onDataChannel(channel *pion.DataChannel) {
	if p.isClosing() {
		_ = channel.Close()
		return
	}
	switch channel.Label() {
	case "control":
		p.setControlChannel(channel)
	case "input":
		p.setInputChannel(channel)
	case "clipboard":
		if p.service.clipboard.Enabled() {
			p.setClipboardChannel(channel)
		} else {
			_ = channel.Close()
		}
	default:
		p.logger.Info("rejecting unsupported data channel", zap.String("label", channel.Label()))
		_ = channel.Close()
	}
}

func (p *peer) setControlChannel(channel *pion.DataChannel) {
	if !channel.Ordered() || channel.MaxPacketLifeTime() != nil || channel.MaxRetransmits() != nil {
		p.logger.Info("rejecting control data channel without reliable ordered delivery")
		_ = channel.Close()
		return
	}

	p.controlMu.Lock()
	previous := p.control
	p.control = channel
	p.controlMu.Unlock()
	if previous != nil {
		p.inputMu.Lock()
		p.inputLeaseGeneration++
		p.inputMu.Unlock()
		_ = p.service.input.Release(p.id)
		_ = previous.Close()
	}

	channel.OnOpen(func() {
		p.logger.Info("control data channel opened")
	})
	cleanup := func() {
		p.controlMu.Lock()
		owned := p.control == channel
		if owned {
			p.control = nil
		}
		p.controlMu.Unlock()
		if owned {
			p.inputMu.Lock()
			p.inputLeaseGeneration++
			p.inputMu.Unlock()
			_ = p.service.input.Release(p.id)
		}
	}
	channel.OnClose(func() { cleanup() })
	channel.OnError(func(err error) { p.logger.Debug("control data channel error", zap.Error(err)); cleanup() })
	channel.OnMessage(func(message pion.DataChannelMessage) {
		p.handleControlMessage(channel, message)
	})
}

func (p *peer) setInputChannel(channel *pion.DataChannel) {
	if !channel.Ordered() || channel.MaxPacketLifeTime() != nil || channel.MaxRetransmits() != nil {
		p.logger.Info("rejecting input data channel without reliable ordered delivery")
		_ = channel.Close()
		return
	}

	p.inputMu.Lock()
	previous := p.input
	p.inputChannelGeneration++
	channelGeneration := p.inputChannelGeneration
	p.input = channel
	p.inputSequence = 0
	p.inputSequenceSet = false
	p.inputLeaseGeneration++
	p.inputMu.Unlock()
	if previous != nil {
		_ = p.service.input.Release(p.id)
		_ = previous.Close()
	}

	channel.OnOpen(func() {
		p.logger.Info("input data channel opened")
	})
	cleanup := func() {
		p.inputMu.Lock()
		owned := p.input == channel && p.inputChannelGeneration == channelGeneration
		if owned {
			p.input = nil
			p.inputLeaseGeneration++
		}
		p.inputMu.Unlock()
		if owned {
			_ = p.service.input.Release(p.id)
		}
	}
	channel.OnClose(func() { cleanup() })
	channel.OnError(func(err error) { p.logger.Debug("input data channel error", zap.Error(err)); cleanup() })
	channel.OnMessage(func(message pion.DataChannelMessage) {
		p.handleInputMessage(channel, message)
	})
}

func (p *peer) handleControlMessage(channel *pion.DataChannel, message pion.DataChannelMessage) {
	p.controlMu.Lock()
	current := p.control == channel
	p.controlMu.Unlock()
	if !current || p.isClosing() {
		return
	}
	if !message.IsString {
		p.writeControlError(channel, "", "invalid_message", "control messages must be text")
		return
	}
	if len(message.Data) > maxControlMessageBytes {
		p.writeControlError(channel, "", "message_too_large", "control message exceeds 16384 bytes")
		return
	}
	if !utf8.Valid(message.Data) {
		p.writeControlError(channel, "", "invalid_message", "control message is not valid UTF-8")
		return
	}

	request, err := decodeControlRequest(message.Data)
	if err != nil {
		p.writeControlError(channel, request.ID.Value, "invalid_message", fmt.Sprintf("decode control message: %v", err))
		return
	}
	if protocolErr := validateControlRequest(request); protocolErr != nil {
		p.writeControlError(channel, request.ID.Value, protocolErr.Code, protocolErr.Message)
		return
	}

	switch request.Type.Value {
	case controlTypeInputAcquire:
		p.controlMu.Lock()
		currentControl := p.control == channel
		p.controlMu.Unlock()
		if !currentControl || p.isClosing() {
			return
		}
		if !p.connected.Load() {
			p.writeControlError(channel, request.ID.Value, "peer_not_connected", "WebRTC peer is not connected")
			return
		}
		p.inputMu.Lock()
		inputChannel := p.input
		p.inputMu.Unlock()
		if inputChannel == nil || inputChannel.ReadyState() != pion.DataChannelStateOpen {
			p.writeControlError(channel, request.ID.Value, "input_channel_required", "an open reliable ordered input data channel is required")
			return
		}
		p.inputMu.Lock()
		inputGeneration := p.inputChannelGeneration
		leaseGeneration := p.inputLeaseGeneration + 1
		p.inputLeaseGeneration = leaseGeneration
		inputStillCurrent := p.input == inputChannel
		p.inputMu.Unlock()
		if !inputStillCurrent || p.isClosing() {
			return
		}
		capabilities, err := p.service.input.Acquire(p.id, func(sequence uint64, cause error) {
			p.onInputRevoked(inputGeneration, leaseGeneration, sequence, cause)
		})
		p.controlMu.Lock()
		controlStillCurrent := p.control == channel
		p.controlMu.Unlock()
		p.inputMu.Lock()
		inputStillCurrent = p.input == inputChannel && p.inputChannelGeneration == inputGeneration && p.inputLeaseGeneration == leaseGeneration
		p.inputMu.Unlock()
		if err == nil && (!controlStillCurrent || !inputStillCurrent || p.isClosing()) {
			p.inputMu.Lock()
			p.inputLeaseGeneration++
			p.inputMu.Unlock()
			_ = p.service.input.Release(p.id)
			return
		}
		if err != nil {
			if !controlStillCurrent || p.isClosing() {
				return
			}
			code := inputErrorCode(err)
			p.writeControlError(channel, request.ID.Value, code, err.Error())
			return
		}
		if !p.writeControl(channel, controlResponse{
			Version: controlVersion,
			ID:      request.ID.Value,
			Type:    controlTypeInputAcquireResult,
			OK:      true,
			Input: &controlInput{
				Pointer:  capabilities.Pointer,
				Keyboard: capabilities.Keyboard,
			},
		}) {
			p.Close()
			return
		}
		if p.service.clipboard.Enabled() {
			p.goOwned(p.sendLatestClipboard)
		}
		return
	case controlTypeInputRelease:
		p.controlMu.Lock()
		currentControl := p.control == channel
		p.controlMu.Unlock()
		if !currentControl || p.isClosing() {
			return
		}
		if err := p.service.input.Release(p.id); err != nil {
			p.writeControlError(channel, request.ID.Value, inputErrorCode(err), err.Error())
			return
		}
		if !p.writeControl(channel, controlResponse{
			Version: controlVersion,
			ID:      request.ID.Value,
			Type:    controlTypeInputReleaseResult,
			OK:      true,
		}) {
			p.Close()
		}
		return
	}

	p.controlMu.Lock()
	qualityControl := p.control == channel
	p.controlMu.Unlock()
	if !qualityControl || p.isClosing() {
		return
	}
	p.service.qualityChangeMu.Lock()
	p.controlMu.Lock()
	qualityControl = p.control == channel
	p.controlMu.Unlock()
	if !qualityControl || p.isClosing() {
		p.service.qualityChangeMu.Unlock()
		return
	}
	qualityCurrent := p.service.source.Quality()
	quality := qualityCurrent
	profileName := qualityCurrent.Profile
	if request.Quality.Value.Profile.Set {
		profileName = request.Quality.Value.Profile.Value
	}
	profile, exists := p.service.source.Profile(profileName)
	if !exists {
		p.service.qualityChangeMu.Unlock()
		p.writeControlError(channel, request.ID.Value, "quality_update_failed", fmt.Sprintf("video profile %q is not configured", profileName))
		return
	}
	optionName := qualityCurrent.Option
	if request.Quality.Value.Profile.Set && !request.Quality.Value.Option.Set {
		optionName = profile.DefaultOption
	}
	if request.Quality.Value.Option.Set {
		optionName = request.Quality.Value.Option.Value
	}
	if request.Quality.Value.Profile.Set || request.Quality.Value.Option.Set {
		option, exists := profile.Options[optionName]
		if !exists {
			p.service.qualityChangeMu.Unlock()
			p.writeControlError(channel, request.ID.Value, "quality_update_failed", fmt.Sprintf("video option %q is not configured for profile %q", optionName, profileName))
			return
		}
		quality = option.Quality(profileName, optionName)
	}
	if request.Quality.Value.Width.Set {
		quality.Width = request.Quality.Value.Width.Value
	}
	if request.Quality.Value.Height.Set {
		quality.Height = request.Quality.Value.Height.Value
	}
	if request.Quality.Value.Framerate.Set {
		quality.Framerate = request.Quality.Value.Framerate.Value
	}
	if request.Quality.Value.BitrateKbps.Set {
		quality.BitrateKbps = request.Quality.Value.BitrateKbps.Value
	}
	err = p.service.source.UpdateQuality(quality)
	effective := p.service.source.Quality()
	_, currentExists := p.service.source.Profile(qualityCurrent.Profile)
	effectiveProfile, effectiveExists := p.service.source.Profile(effective.Profile)
	if err == nil && (!currentExists || !effectiveExists) {
		err = errors.New("media profile metadata is unavailable after quality update")
	}
	requesterNeedsReconnect := err == nil && effectiveExists && (!p.videoCodec.Compatible(effectiveProfile.Codec) ||
		p.videoFrontendTransform != effectiveProfile.FrontendTransform)
	var qualityGeneration uint64
	var incompatiblePeers []*peer
	if err == nil && currentExists && effectiveExists {
		for _, candidate := range p.service.peerSnapshot() {
			if candidate != p && (!candidate.videoCodec.Compatible(effectiveProfile.Codec) ||
				candidate.videoFrontendTransform != effectiveProfile.FrontendTransform) {
				incompatiblePeers = append(incompatiblePeers, candidate)
			}
		}
	}
	if requesterNeedsReconnect || len(incompatiblePeers) > 0 {
		p.service.qualityMu.Lock()
		p.service.qualityGeneration++
		qualityGeneration = p.service.qualityGeneration
		p.service.qualityMu.Unlock()
	}
	p.service.qualityChangeMu.Unlock()
	if err != nil {
		p.writeControlError(channel, request.ID.Value, "quality_update_failed", err.Error())
		return
	}
	responseWritten := p.writeControl(channel, controlResponse{
		Version: controlVersion,
		ID:      request.ID.Value,
		Type:    controlTypeQualitySetResult,
		OK:      true,
		Quality: qualityResponse(effective),
	})
	for _, candidate := range incompatiblePeers {
		p.service.closePeerForProfileChange(candidate, qualityGeneration)
	}
	if !responseWritten {
		p.Close()
		return
	}
	if requesterNeedsReconnect {
		p.Close()
	}
}

func (p *peer) handleInputMessage(channel *pion.DataChannel, message pion.DataChannelMessage) {
	p.inputMu.Lock()
	current := p.input == channel
	p.inputMu.Unlock()
	if !current || p.isClosing() {
		return
	}
	p.inputMessagesSeen.Add(1)
	if !message.IsString {
		p.writeInputError(channel, nil, "invalid_message", "input messages must be text")
		return
	}
	if len(message.Data) > maxInputMessageBytes {
		p.writeInputError(channel, nil, "message_too_large", "input message exceeds 4096 bytes")
		return
	}
	if !utf8.Valid(message.Data) {
		p.writeInputError(channel, nil, "invalid_message", "input message is not valid UTF-8")
		return
	}

	request, err := decodeInputRequest(message.Data)
	sequence := inputSequencePointer(request)
	if err != nil {
		p.writeInputError(channel, sequence, "invalid_message", fmt.Sprintf("decode input message: %v", err))
		return
	}
	event, protocolErr := validateInputRequest(request)
	if protocolErr != nil {
		p.writeInputError(channel, sequence, protocolErr.Code, protocolErr.Message)
		return
	}

	p.inputMu.Lock()
	inputGeneration := p.inputChannelGeneration
	if p.input != channel || p.isClosing() {
		p.inputMu.Unlock()
		return
	}
	if p.inputSequenceSet && request.Sequence.Value <= p.inputSequence {
		p.inputMu.Unlock()
		p.writeInputError(channel, sequence, "invalid_sequence", "sequence must increase monotonically")
		return
	}
	p.inputSequence = request.Sequence.Value
	p.inputSequenceSet = true
	p.inputMu.Unlock()

	if !p.connected.Load() {
		p.writeInputError(channel, sequence, "peer_not_connected", "WebRTC peer is not connected")
		return
	}
	if p.isClosing() {
		return
	}
	p.inputMu.Lock()
	currentInput := p.input == channel && p.inputChannelGeneration == inputGeneration && !p.isClosing()
	p.inputMu.Unlock()
	if !currentInput {
		return
	}
	if !p.service.input.Owns(p.id) {
		p.writeInputError(channel, sequence, "input_not_owned", "peer does not own input")
		return
	}
	if err := p.service.input.Submit(p.id, event); err != nil {
		if errors.Is(err, remoteinput.ErrOverloaded) {
			p.inputOverloads.Add(1)
			return
		}
		p.writeInputError(channel, sequence, inputErrorCode(err), err.Error())
		return
	}
	p.inputMessagesSent.Add(1)
}

func (p *peer) writeControlError(channel *pion.DataChannel, id, code, message string) {
	if !p.writeControl(channel, controlResponse{
		Version: controlVersion,
		ID:      id,
		Type:    controlTypeError,
		OK:      false,
		Error: &protocolError{
			Code:    code,
			Message: message,
		},
	}) {
		p.Close()
	}
}

func (p *peer) writeControl(channel *pion.DataChannel, response controlResponse) bool {
	data, err := json.Marshal(response)
	if err != nil {
		p.logger.Error("encode control response", zap.Error(err))
		return false
	}

	p.controlMu.Lock()
	current := p.control == channel
	p.controlMu.Unlock()
	if !current || p.isClosing() {
		return false
	}
	p.controlWriteMu.Lock()
	defer p.controlWriteMu.Unlock()
	if err := channel.SendText(string(data)); err != nil {
		p.logger.Debug("control data channel write stopped", zap.Error(err))
		return false
	}
	return true
}

func (p *peer) onInputRevoked(inputGeneration, leaseGeneration, sequence uint64, cause error) {
	p.inputMu.Lock()
	channel := p.input
	current := channel != nil && p.inputChannelGeneration == inputGeneration && p.inputLeaseGeneration == leaseGeneration
	if current && !p.isClosing() {
		p.input = nil
		p.inputChannelGeneration++
		p.inputLeaseGeneration++
	}
	p.inputMu.Unlock()
	if !current || p.isClosing() {
		return
	}
	var correlation *uint64
	if sequence != 0 {
		correlation = &sequence
	}
	p.writeInputError(channel, correlation, inputErrorCode(cause), cause.Error())
	_ = channel.Close()
}

func (p *peer) writeInputError(channel *pion.DataChannel, sequence *uint64, code, message string) {
	if p.isClosing() {
		return
	}
	data, err := json.Marshal(inputResponse{
		Version:  inputVersion,
		Sequence: sequence,
		Type:     inputTypeError,
		OK:       false,
		Error: &protocolError{
			Code:    code,
			Message: message,
		},
	})
	if err != nil {
		p.logger.Error("encode input response", zap.Error(err))
		return
	}

	p.inputWriteMu.Lock()
	defer p.inputWriteMu.Unlock()
	if err := channel.SendText(string(data)); err != nil {
		p.logger.Debug("input data channel write stopped", zap.Error(err))
	}
}

func inputSequencePointer(request inputRequest) *uint64 {
	if !request.Sequence.Set {
		return nil
	}
	sequence := request.Sequence.Value
	return &sequence
}

func inputErrorCode(err error) string {
	switch {
	case errors.Is(err, remoteinput.ErrBusy):
		return "input_busy"
	case errors.Is(err, remoteinput.ErrDisabled):
		return "input_disabled"
	case errors.Is(err, remoteinput.ErrPointerUnauthorized):
		return "input_pointer_unauthorized"
	case errors.Is(err, remoteinput.ErrKeyboardUnauthorized):
		return "input_keyboard_unauthorized"
	case errors.Is(err, remoteinput.ErrNotReady):
		return "input_not_ready"
	case errors.Is(err, remoteinput.ErrNotOwner):
		return "input_not_owned"
	case errors.Is(err, remoteinput.ErrOverloaded):
		return "input_overloaded"
	case errors.Is(err, remoteinput.ErrClosed):
		return "input_unavailable"
	default:
		return "input_failed"
	}
}
